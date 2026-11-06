package reliability_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/gateway/core"
	"github.com/minhhhduc/gnvipi/internal/gateway/dispatch"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"github.com/minhhhduc/gnvipi/internal/gateway/reliability"
	"github.com/minhhhduc/gnvipi/internal/gateway/routing"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func TestLimiter_Unbounded(t *testing.T) {
	l := reliability.NewLimiter()
	ctx := t.Context()

	for _, limit := range []int{0, -1, -5} {
		p := &provider.Provider{ID: "p-unbounded", MaxConcurrency: limit}
		var releases []func()
		for range 50 {
			rel, err := l.Acquire(ctx, p)
			if err != nil {
				t.Fatalf("unexpected error for limit %d: %v", limit, err)
			}
			releases = append(releases, rel)
		}
		for _, rel := range releases {
			rel()
		}
		if got := l.InFlight("p-unbounded"); got != 0 {
			t.Fatalf("in-flight = %d, want 0", got)
		}
	}

	// Nil provider is also unbounded.
	rel, err := l.Acquire(ctx, nil)
	if err != nil {
		t.Fatalf("unexpected error for nil provider: %v", err)
	}
	rel()
}

func TestLimiter_MaxConcurrencyEnforced(t *testing.T) {
	l := reliability.NewLimiter()
	ctx := t.Context()
	p := &provider.Provider{ID: "p1", MaxConcurrency: 2, QueueTimeout: 0}

	rel1, err := l.Acquire(ctx, p)
	if err != nil {
		t.Fatalf("acquire 1 failed: %v", err)
	}
	if got := l.InFlight(p.ID); got != 1 {
		t.Fatalf("in-flight = %d, want 1", got)
	}

	rel2, err := l.Acquire(ctx, p)
	if err != nil {
		t.Fatalf("acquire 2 failed: %v", err)
	}
	if got := l.InFlight(p.ID); got != 2 {
		t.Fatalf("in-flight = %d, want 2", got)
	}

	// Third attempt should immediately fail with 503 Overloaded.
	_, err = l.Acquire(ctx, p)
	if err == nil {
		t.Fatal("acquire 3 succeeded, want overloaded error")
	}
	var gwErr *gwerr.Error
	if !errors.As(err, &gwErr) {
		t.Fatalf("error = %v, want *gwerr.Error", err)
	}
	if gwErr.Kind != gwerr.KindOverloaded || gwErr.Status != http.StatusServiceUnavailable {
		t.Fatalf("got kind=%s status=%d, want overloaded/503", gwErr.Kind, gwErr.Status)
	}

	// Release one slot.
	rel1()
	if got := l.InFlight(p.ID); got != 1 {
		t.Fatalf("in-flight after release = %d, want 1", got)
	}

	// Now third attempt can succeed.
	rel3, err := l.Acquire(ctx, p)
	if err != nil {
		t.Fatalf("acquire 3 after release failed: %v", err)
	}
	if got := l.InFlight(p.ID); got != 2 {
		t.Fatalf("in-flight = %d, want 2", got)
	}

	rel2()
	rel3()
	if got := l.InFlight(p.ID); got != 0 {
		t.Fatalf("in-flight final = %d, want 0", got)
	}
}

func TestLimiter_QueueTimeoutWaitsAndSucceeds(t *testing.T) {
	l := reliability.NewLimiter()
	ctx := t.Context()
	p := &provider.Provider{ID: "p-wait", MaxConcurrency: 1, QueueTimeout: 500 * time.Millisecond}

	rel1, err := l.Acquire(ctx, p)
	if err != nil {
		t.Fatalf("acquire 1 failed: %v", err)
	}

	acquired := make(chan struct{})
	go func() {
		rel2, err := l.Acquire(ctx, p)
		if err != nil {
			t.Errorf("acquire 2 failed: %v", err)
			return
		}
		close(acquired)
		rel2()
	}()

	// Ensure goroutine 2 has entered the queue.
	time.Sleep(30 * time.Millisecond)
	rel1()

	select {
	case <-acquired:
		// Succeeded before queue timeout!
	case <-time.After(400 * time.Millisecond):
		t.Fatal("timed out waiting for goroutine 2 to acquire slot")
	}
}

func TestLimiter_QueueTimeoutExpiresReturns503(t *testing.T) {
	l := reliability.NewLimiter()
	ctx := t.Context()
	p := &provider.Provider{ID: "p-timeout", MaxConcurrency: 1, QueueTimeout: 40 * time.Millisecond}

	rel1, err := l.Acquire(ctx, p)
	if err != nil {
		t.Fatalf("acquire 1 failed: %v", err)
	}
	defer rel1()

	start := time.Now()
	_, err = l.Acquire(ctx, p)
	duration := time.Since(start)

	if err == nil {
		t.Fatal("acquire succeeded, want queue timeout error")
	}
	if duration < 35*time.Millisecond {
		t.Fatalf("queue timeout returned too quickly: %v", duration)
	}

	var gwErr *gwerr.Error
	if !errors.As(err, &gwErr) {
		t.Fatalf("error = %v, want *gwerr.Error", err)
	}
	if gwErr.Kind != gwerr.KindOverloaded || gwErr.Status != http.StatusServiceUnavailable {
		t.Fatalf("got kind=%s status=%d, want overloaded/503", gwErr.Kind, gwErr.Status)
	}
}

func TestLimiter_ContextCanceledWhileWaiting(t *testing.T) {
	l := reliability.NewLimiter()
	p := &provider.Provider{ID: "p-cancel", MaxConcurrency: 1, QueueTimeout: time.Second}

	rel1, err := l.Acquire(context.Background(), p)
	if err != nil {
		t.Fatalf("acquire 1 failed: %v", err)
	}
	defer rel1()

	waitCtx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)

	go func() {
		_, err := l.Acquire(waitCtx, p)
		errCh <- err
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("cancellation did not unblock acquire")
	}

	// Slot should not be leaked; once rel1 is called, a fresh acquire should succeed immediately.
	rel1()
	relFresh, err := l.Acquire(context.Background(), p)
	if err != nil {
		t.Fatalf("fresh acquire failed: %v", err)
	}
	relFresh()
}

func TestLimiter_ContextAlreadyCanceled(t *testing.T) {
	l := reliability.NewLimiter()
	p := &provider.Provider{ID: "p-precancel", MaxConcurrency: 1, QueueTimeout: time.Second}

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := l.Acquire(canceledCtx, p)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestLimiter_ReleaseIsIdempotent(t *testing.T) {
	l := reliability.NewLimiter()
	p := &provider.Provider{ID: "p-idempotent", MaxConcurrency: 1}

	rel, err := l.Acquire(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	// Calling release multiple times must not panic or underflow tokens.
	rel()
	rel()
	rel()

	// Should still allow acquiring exactly 1 slot.
	rel2, err := l.Acquire(t.Context(), p)
	if err != nil {
		t.Fatalf("subsequent acquire failed: %v", err)
	}
	rel2()
}

func TestLimiter_MultipleProvidersIndependent(t *testing.T) {
	l := reliability.NewLimiter()
	ctx := t.Context()
	pA := &provider.Provider{ID: "prov-a", MaxConcurrency: 1}
	pB := &provider.Provider{ID: "prov-b", MaxConcurrency: 1}

	relA, err := l.Acquire(ctx, pA)
	if err != nil {
		t.Fatal(err)
	}
	defer relA()

	// Provider A is full; Provider B should still acquire without obstruction.
	relB, err := l.Acquire(ctx, pB)
	if err != nil {
		t.Fatalf("provider B acquire failed while A was full: %v", err)
	}
	relB()

	// Provider A should still reject.
	_, err = l.Acquire(ctx, pA)
	if err == nil {
		t.Fatal("provider A second acquire should have failed")
	}
}

func TestLimiter_DynamicLimitUpdate(t *testing.T) {
	l := reliability.NewLimiter()
	ctx := t.Context()

	p1 := &provider.Provider{ID: "p-dyn", MaxConcurrency: 1}
	rel1, err := l.Acquire(ctx, p1)
	if err != nil {
		t.Fatal(err)
	}
	defer rel1()

	// With limit 1, second acquire fails.
	_, err = l.Acquire(ctx, p1)
	if err == nil {
		t.Fatal("expected failure at limit 1")
	}

	// Update limit to 3.
	p3 := &provider.Provider{ID: "p-dyn", MaxConcurrency: 3}
	rel2, err := l.Acquire(ctx, p3)
	if err != nil {
		t.Fatalf("acquire under new limit 3 failed: %v", err)
	}
	defer rel2()

	rel3, err := l.Acquire(ctx, p3)
	if err != nil {
		t.Fatalf("acquire 2 under new limit 3 failed: %v", err)
	}
	rel2()
	rel3()
}

func TestLimiter_ResetAndInvalidate(t *testing.T) {
	l := reliability.NewLimiter()
	pA := &provider.Provider{ID: "p-a", MaxConcurrency: 2}
	pB := &provider.Provider{ID: "p-b", MaxConcurrency: 2}

	relA, err := l.Acquire(t.Context(), pA)
	if err != nil {
		t.Fatal(err)
	}
	relB, err := l.Acquire(t.Context(), pB)
	if err != nil {
		t.Fatal(err)
	}

	if got := l.InFlight("p-a"); got != 1 {
		t.Fatalf("p-a in flight = %d, want 1", got)
	}
	if got := l.InFlight("p-b"); got != 1 {
		t.Fatalf("p-b in flight = %d, want 1", got)
	}

	l.Invalidate("p-a")
	if got := l.InFlight("p-a"); got != 0 {
		t.Fatalf("after invalidate p-a in flight = %d, want 0", got)
	}
	if got := l.InFlight("p-b"); got != 1 {
		t.Fatalf("p-b should remain untouched: in flight = %d, want 1", got)
	}

	l.Reset()
	if got := l.InFlight("p-b"); got != 0 {
		t.Fatalf("after reset p-b in flight = %d, want 0", got)
	}

	// Old holders releasing after Reset/Invalidate must not panic.
	relA()
	relB()
}

func TestLimiter_OverloadedErrorProtocolEncoding(t *testing.T) {
	err := &gwerr.Error{
		Kind:    gwerr.KindOverloaded,
		Status:  http.StatusServiceUnavailable,
		Message: `provider "p-test" max concurrency reached (4)`,
	}

	for _, tc := range []struct {
		protocol       gwerr.Protocol
		expectedStatus int
		expectedType   string
	}{
		{gwerr.ProtocolAnthropicMessages, http.StatusServiceUnavailable, "overloaded_error"},
		{gwerr.ProtocolOpenAIChat, http.StatusServiceUnavailable, "server_error"},
		{gwerr.ProtocolOpenAIResponses, http.StatusServiceUnavailable, "server_error"},
	} {
		t.Run(string(tc.protocol), func(t *testing.T) {
			body, status, encErr := gwerr.Encode(tc.protocol, err)
			if encErr != nil {
				t.Fatalf("Encode failed: %v", encErr)
			}
			if status != tc.expectedStatus {
				t.Fatalf("status = %d, want %d", status, tc.expectedStatus)
			}
			bodyStr := string(body)
			if !strings.Contains(bodyStr, tc.expectedType) {
				t.Fatalf("body %q does not contain expected type %q", bodyStr, tc.expectedType)
			}
			if !strings.Contains(bodyStr, "max concurrency reached") {
				t.Fatalf("body %q does not contain error message", bodyStr)
			}

			// Write via gwerr.Write to ResponseWriter.
			rec := httptest.NewRecorder()
			if writeErr := gwerr.Write(rec, tc.protocol, err); writeErr != nil {
				t.Fatalf("gwerr.Write failed: %v", writeErr)
			}
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("HTTP code = %d, want 503", rec.Code)
			}
		})
	}
}

// TestConcurrency_RequestIsolation_CrossTalkProof verifies under -race that N concurrent
// requests across different providers and protocols maintain strict request isolation:
// every response header and body strictly matches its request ID with zero cross-talk.
func TestConcurrency_RequestIsolation_CrossTalkProof(t *testing.T) {
	// Set up mock upstream server that answers across different protocols.
	// It echoes back the request ID and unique payload for verification.
	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		reqID := r.Header.Get(core.RequestIDHeader)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body error: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		var payload struct {
			Model    string          `json:"model"`
			Messages []any           `json:"messages"`
			Input    json.RawMessage `json:"input"`
			Marker   string          `json:"marker"`
		}
		if unmarshalErr := json.Unmarshal(body, &payload); unmarshalErr != nil {
			t.Errorf("unmarshal body error: %v", unmarshalErr)
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(core.RequestIDHeader, reqID)

		resp := map[string]string{
			"id":     reqID,
			"model":  payload.Model,
			"marker": payload.Marker,
			"echo":   string(body),
		}
		if encodeErr := json.NewEncoder(w).Encode(resp); encodeErr != nil {
			t.Errorf("encode response error: %v", encodeErr)
		}
	}))
	defer upstream.Close()

	// Configure providers across different protocols.
	providers := []provider.LegacyProvider{
		{
			Name:    "vendor-chat",
			BaseURL: upstream.URL,
			APIKey:  "key-chat",
			Model:   "model-chat",
			Alias:   "alias-chat",
		},
		{
			Name:         "vendor-messages",
			BaseURL:      upstream.URL,
			APIKey:       "key-messages",
			Model:        "model-messages",
			Alias:        "alias-messages",
			MessagesOnly: true,
		},
		{
			Name:          "vendor-responses",
			BaseURL:       upstream.URL,
			APIKey:        "key-responses",
			Model:         "model-responses",
			Alias:         "alias-responses",
			ResponsesOnly: true,
		},
	}

	resolver, err := routing.NewLegacyResolver(providers, nil)
	if err != nil {
		t.Fatal(err)
	}

	dispatcher := dispatch.New(resolver, nil)
	engine := gin.New()
	engine.Use(dispatcher.Handle)

	// Concurrently send N requests across the 3 protocols.
	const totalRequests = 60
	type scenario struct {
		path  string
		alias string
	}
	scenarios := []scenario{
		{"/v1/chat/completions", "alias-chat"},
		{"/v1/messages", "alias-messages"},
		{"/v1/responses", "alias-responses"},
	}

	var wg sync.WaitGroup
	errCh := make(chan error, totalRequests)

	for i := range totalRequests {
		wg.Add(1)
		sc := scenarios[i%len(scenarios)]
		reqID := fmt.Sprintf("req-iso-%03d-%d", i, time.Now().UnixNano())
		marker := fmt.Sprintf("marker-%d-%s", i, sc.alias)

		go func(sc scenario, reqID, marker string) {
			defer wg.Done()

			reqBody := fmt.Sprintf(`{"model":%q,"marker":%q,"index":%d}`, sc.alias, marker, i)
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, sc.path, strings.NewReader(reqBody))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(core.RequestIDHeader, reqID)

			rec := httptest.NewRecorder()
			engine.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				errCh <- fmt.Errorf("request %s (%s) got status %d: %s", reqID, sc.path, rec.Code, rec.Body.String())
				return
			}

			// Verify Request ID isolation in response header.
			respReqID := rec.Header().Get(core.RequestIDHeader)
			if respReqID != reqID {
				errCh <- fmt.Errorf("cross-talk! request %s received response header X-Request-ID=%q", reqID, respReqID)
				return
			}

			// Verify response content isolation.
			var respData map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &respData); err != nil {
				errCh <- fmt.Errorf("request %s body unmarshal error: %w", reqID, err)
				return
			}
			if respData["id"] != reqID {
				errCh <- fmt.Errorf("cross-talk in body! request %s got body id=%q", reqID, respData["id"])
				return
			}
			if respData["marker"] != marker {
				errCh <- fmt.Errorf("cross-talk in body! request %s got body marker=%q, want %q", reqID, respData["marker"], marker)
				return
			}
		}(sc, reqID, marker)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Error(err)
	}

	if calls := upstreamCalls.Load(); calls != int64(totalRequests) {
		t.Fatalf("upstream received %d calls, want %d", calls, totalRequests)
	}
}

// TestDispatch_LimiterEnforcement_EndToEnd verifies that when a provider's max_concurrency
// is exceeded and queue_timeout expires, dispatch.Handle answers with 503 Overloaded
// in the appropriate wire protocol shape.
func TestDispatch_LimiterEnforcement_EndToEnd(t *testing.T) {
	requestStarted := make(chan struct{})
	allowFinish := make(chan struct{})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case requestStarted <- struct{}{}:
		default:
		}
		<-allowFinish
		w.Header().Set("Content-Type", "application/json")
		if _, writeErr := w.Write([]byte(`{"result":"ok"}`)); writeErr != nil {
			t.Errorf("write response error: %v", writeErr)
		}
	}))
	defer upstream.Close()

	// Provider with MaxConcurrency: 1 and QueueTimeout: 30ms.
	p := provider.Provider{
		ID:             "prov-limited",
		Name:           "prov-limited",
		BaseURL:        upstream.URL,
		Protocol:       ir.ProtocolAnthropicMessages,
		Models:         []provider.ModelMapping{{Alias: "claude-test", UpstreamModel: "claude-v1"}},
		MaxConcurrency: 1,
		QueueTimeout:   30 * time.Millisecond,
		Enabled:        true,
	}

	resolver := &routing.LegacyResolver{}
	if err := resolver.UpdateProviders([]provider.Provider{p}, nil); err != nil {
		t.Fatal(err)
	}

	dispatcher := dispatch.New(resolver, nil)
	engine := gin.New()
	engine.Use(dispatcher.Handle)

	// Launch request 1 (will occupy the single slot).
	req1Done := make(chan struct{})
	go func() {
		defer close(req1Done)
		req1 := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test"}`))
		req1.Header.Set("Content-Type", "application/json")
		rec1 := httptest.NewRecorder()
		engine.ServeHTTP(rec1, req1)
		if rec1.Code != http.StatusOK {
			t.Errorf("request 1 code = %d, want 200", rec1.Code)
		}
	}()

	// Wait until upstream starts processing request 1.
	<-requestStarted

	// Launch request 2 while request 1 is in-flight. It must wait and timeout with 503 Overloaded.
	req2 := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test"}`))
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	engine.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("request 2 code = %d, want 503", rec2.Code)
	}

	// Verify Anthropic protocol error shape.
	var anthropicErr struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &anthropicErr); err != nil {
		t.Fatalf("unmarshal anthropic error: %v, body=%s", err, rec2.Body.String())
	}
	if anthropicErr.Type != "error" || anthropicErr.Error.Type != "overloaded_error" {
		t.Fatalf("unexpected anthropic error shape: %+v", anthropicErr)
	}
	if !strings.Contains(anthropicErr.Error.Message, "prov-limited") {
		t.Fatalf("error message does not mention provider: %s", anthropicErr.Error.Message)
	}

	// Release request 1.
	close(allowFinish)
	<-req1Done

	// Launch request 3 now that request 1 has finished. It should succeed immediately.
	allowFinish3 := make(chan struct{})
	close(allowFinish3)
	req3 := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test"}`))
	req3.Header.Set("Content-Type", "application/json")
	rec3 := httptest.NewRecorder()
	engine.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusOK {
		t.Fatalf("request 3 code = %d, want 200: %s", rec3.Code, rec3.Body.String())
	}
}

// TestDispatch_ClientCancellationReleasesToken_Integration verifies that if a client disconnects
// or cancels during an in-flight request, the concurrency slot is immediately released and not leaked.
func TestDispatch_ClientCancellationReleasesToken_Integration(t *testing.T) {
	reqReceived := make(chan struct{})
	allowFinish := make(chan struct{})
	var closeOnce sync.Once
	defer closeOnce.Do(func() { close(allowFinish) })

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-reqReceived:
		default:
			close(reqReceived)
		}
		select {
		case <-r.Context().Done():
		case <-allowFinish:
		}
	}))
	defer upstream.Close()
	defer upstream.CloseClientConnections()

	p := provider.Provider{
		ID:             "prov-cancel-test",
		Name:           "prov-cancel-test",
		BaseURL:        upstream.URL,
		Protocol:       ir.ProtocolOpenAIChat,
		Models:         []provider.ModelMapping{{Alias: "gpt-cancel", UpstreamModel: "gpt-4"}},
		MaxConcurrency: 1,
		QueueTimeout:   100 * time.Millisecond,
		Enabled:        true,
	}

	resolver := &routing.LegacyResolver{}
	if err := resolver.UpdateProviders([]provider.Provider{p}, nil); err != nil {
		t.Fatal(err)
	}

	dispatcher := dispatch.New(resolver, nil)
	engine := gin.New()
	engine.Use(dispatcher.Handle)

	clientCtx, cancelClient := context.WithCancel(context.Background())
	req1Done := make(chan struct{})

	go func() {
		defer close(req1Done)
		req1 := httptest.NewRequestWithContext(clientCtx, http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-cancel"}`)))
		req1.Header.Set("Content-Type", "application/json")
		rec1 := httptest.NewRecorder()
		engine.ServeHTTP(rec1, req1)
	}()

	// Wait until upstream receives request 1.
	<-reqReceived

	// Now client cancels.
	cancelClient()
	<-req1Done
	closeOnce.Do(func() { close(allowFinish) })

	// Give runtime a moment to process defer release().
	time.Sleep(20 * time.Millisecond)

	// Verify that provider slot was freed and is not held.
	if inFlight := dispatcher.Limiter.InFlight("prov-cancel-test"); inFlight != 0 {
		t.Fatalf("expected 0 in-flight after cancellation, got %d", inFlight)
	}
}
