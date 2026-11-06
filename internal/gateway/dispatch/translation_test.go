package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/core"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	inboundanthropic "github.com/minhhhduc/gnvipi/internal/gateway/inbound/anthropic"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/outbound"
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"github.com/minhhhduc/gnvipi/internal/gateway/relay"
	"github.com/minhhhduc/gnvipi/internal/gateway/routing"
	"github.com/minhhhduc/gnvipi/internal/gateway/sse"
	"github.com/minhhhduc/gnvipi/internal/gateway/upstream"
)

const (
	translationAdapterName = "pipeline-test"
	translationSecret      = "pipeline-provider-secret"
)

type translationInbound struct{ decodes *atomic.Int32 }

func (a translationInbound) Protocol() ir.Protocol { return ir.ProtocolOpenAIChat }

func (a translationInbound) DecodeRequest(r *http.Request) (*ir.Request, error) {
	a.decodes.Add(1)
	var wire struct {
		Model  string `json:"model"`
		Prompt string `json:"prompt"`
		Stream bool   `json:"stream"`
	}
	if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
		return nil, &gwerr.Error{Kind: gwerr.KindInvalidRequest, Message: "invalid fake request"}
	}
	return &ir.Request{Model: wire.Model, Stream: wire.Stream, Messages: []ir.Message{ir.NewMessage(ir.RoleUser, ir.NewTextBlock(wire.Prompt))}}, nil
}

func (translationInbound) EncodeResponse(response *ir.Response) ([]byte, error) {
	answer := ""
	if len(response.Content) > 0 {
		answer = response.Content[0].Text
	}
	return json.Marshal(struct {
		ID     string   `json:"id"`
		Model  string   `json:"model"`
		Answer string   `json:"answer"`
		Usage  ir.Usage `json:"usage"`
	}{response.ID, response.Model, answer, response.Usage})
}

func (translationInbound) EncodeStream(context.Context, adapter.Stream, *sse.Writer) error {
	return errors.New("fake inbound streaming is unsupported")
}

type translationOutbound struct{ builds *atomic.Int32 }

func (translationOutbound) Protocol() ir.Protocol { return ir.ProtocolAnthropicMessages }
func (translationOutbound) Capabilities() adapter.Capabilities {
	return adapter.Capabilities{}
}

func (a translationOutbound) BuildRequest(ctx context.Context, request *ir.Request, target adapter.Target) (*http.Request, error) {
	a.builds.Add(1)
	text := ""
	if len(request.Messages) > 0 && len(request.Messages[0].Content) > 0 {
		text = request.Messages[0].Content[0].Text
	}
	body, err := json.Marshal(struct {
		Adapter string `json:"adapter"`
		Model   string `json:"model"`
		Text    string `json:"text"`
		Stream  bool   `json:"stream"`
	}{"custom-selected", target.Model, text, request.Stream})
	if err != nil {
		return nil, err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(target.BaseURL, "/")+"/ignored", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	r.Header = target.Headers.Clone()
	if r.Header == nil {
		r.Header = make(http.Header)
	}
	r.Header.Set("Content-Type", "application/json")
	return r, nil
}

func (translationOutbound) ParseResponse(response *http.Response) (*ir.Response, error) {
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, err := io.ReadAll(response.Body)
		if err != nil {
			return nil, err
		}
		return nil, &gwerr.Error{
			Status: response.StatusCode, Kind: gwerr.KindFromHTTPStatus(response.StatusCode), Message: string(body),
			RetryAfter: 2 * time.Second, UpstreamRequestID: response.Header.Get(core.RequestIDHeader),
		}
	}
	var wire struct {
		ID     string   `json:"id"`
		Model  string   `json:"model"`
		Answer string   `json:"answer"`
		Usage  ir.Usage `json:"usage"`
	}
	if err := json.NewDecoder(response.Body).Decode(&wire); err != nil {
		return nil, err
	}
	result := ir.NewTextResponse(wire.ID, wire.Model, wire.Answer, wire.Usage)
	return &result, nil
}

func (translationOutbound) ParseStream(context.Context, *http.Response) (adapter.Stream, error) {
	return nil, errors.New("fake outbound streaming is unsupported")
}

type translationRecorder struct{ completions chan *core.Completion }

func newTranslationRecorder() *translationRecorder {
	return &translationRecorder{completions: make(chan *core.Completion, 8)}
}
func (r *translationRecorder) Record(completion *core.Completion) { r.completions <- completion }

func receiveTranslationCompletion(t *testing.T, recorder *translationRecorder) *core.Completion {
	t.Helper()
	select {
	case completion := <-recorder.completions:
		return completion
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for translation completion")
		return nil
	}
}

func translationProvider(t *testing.T, baseURL string) provider.Provider {
	t.Helper()
	protocol, err := provider.ParseProtocol("custom:" + translationAdapterName)
	if err != nil {
		t.Fatal(err)
	}
	return provider.Provider{
		ID: "pipeline-provider", Name: "Pipeline provider", BaseURL: strings.TrimRight(baseURL, "/") + "/base", Protocol: protocol,
		Auth:    provider.Auth{Scheme: provider.AuthBearer, Key: translationSecret, HeaderName: "X-Vendor-Key"},
		Headers: map[string]string{"X-Static": "configured"}, ForwardHeaders: []string{"X-Trace"},
		Models: []provider.ModelMapping{{Alias: "client-alias", UpstreamModel: "upstream-model"}}, Enabled: true,
		EndpointPaths: map[ir.Protocol]string{ir.ProtocolAnthropicMessages: "/translated"},
		ExtraBody:     json.RawMessage(`{"tenant":9007199254740993}`),
	}
}

func translationResolver(t *testing.T, p *provider.Provider) *routing.LegacyResolver {
	t.Helper()
	resolver, err := routing.NewLegacyResolver(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := resolver.UpdateProviders([]provider.Provider{*p}, nil); err != nil {
		t.Fatal(err)
	}
	return resolver
}

func translationRegistry(decodes, builds *atomic.Int32, inbound, outboundAdapter bool) *adapter.Registry {
	registry := adapter.NewRegistry()
	if inbound {
		registry.RegisterInbound(translationInbound{decodes: decodes})
	}
	if outboundAdapter {
		outbound.RegisterCustom(registry, translationAdapterName, translationOutbound{builds: builds})
	}
	return registry
}

func translationDispatcher(t *testing.T, p *provider.Provider, registry *adapter.Registry, recorder core.Recorder) *Dispatcher {
	t.Helper()
	factory := upstream.NewFactory()
	t.Cleanup(factory.CloseIdleConnections)
	dispatcher := New(translationResolver(t, p), &relay.Options{Factory: factory, Recorder: recorder})
	dispatcher.Registry = registry
	return dispatcher
}

func TestNonStreamingTranslationRoundTripErrorAndUsage(t *testing.T) {
	requests := make(chan struct {
		header http.Header
		body   []byte
	}, 2)
	responseClosed := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		requests <- struct {
			header http.Header
			body   []byte
		}{r.Header.Clone(), body}
		if bytes.Contains(body, []byte(`"text":"fail"`)) {
			w.Header().Set(core.RequestIDHeader, "upstream-failure-id")
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			if _, err := io.WriteString(w, `{"error":{"message":"`+translationSecret+` denied"}}`); err != nil {
				t.Error(err)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := io.WriteString(w, `{"id":"upstream-id","model":"upstream-model","answer":"pong","usage":{"input_tokens":7,"output_tokens":3,"cache_read_tokens":2}}`); err != nil {
			t.Error(err)
		}
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			responseClosed <- struct{}{}
		case <-time.After(2 * time.Second):
			t.Error("translated response body was not closed or canceled")
		}
	}))
	defer server.Close()

	var decodes, builds atomic.Int32
	recorder := newTranslationRecorder()
	prov := translationProvider(t, server.URL)
	dispatcher := translationDispatcher(t, &prov, translationRegistry(&decodes, &builds, true, true), recorder)
	engine := gin.New()
	engine.Use(dispatcher.Handle)
	engine.POST("/v1/chat/completions", func(c *gin.Context) { t.Error("translated request reached legacy downstream") })

	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"client-alias","prompt":"ping"}`))
	request.Header.Set(core.RequestIDHeader, "client-request-id")
	request.Header.Set("Authorization", "Bearer client-secret")
	request.Header.Set("X-Trace", "forwarded")
	request.Header.Set("X-Blocked", "private")
	out := httptest.NewRecorder()
	engine.ServeHTTP(out, request)
	if out.Code != http.StatusOK {
		t.Fatalf("translated response = %d %s", out.Code, out.Body.String())
	}
	var response struct {
		ID, Model, Answer string
		Usage             ir.Usage
	}
	if err := json.Unmarshal(out.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.ID != "upstream-id" || response.Model != "client-alias" || response.Answer != "pong" || response.Usage != (ir.Usage{InputTokens: 7, OutputTokens: 3, CacheReadTokens: 2}) {
		t.Fatalf("translated response = %+v", response)
	}
	observed := <-requests
	var upstreamBody map[string]json.RawMessage
	if err := json.Unmarshal(observed.body, &upstreamBody); err != nil {
		t.Fatal(err)
	}
	for field, want := range map[string]string{"adapter": `"custom-selected"`, "model": `"upstream-model"`, "text": `"ping"`, "stream": "false", "tenant": "9007199254740993"} {
		if string(upstreamBody[field]) != want {
			t.Errorf("upstream %s = %s, want %s", field, upstreamBody[field], want)
		}
	}
	if observed.header.Get("X-Vendor-Key") != "Bearer "+translationSecret || observed.header.Get("Authorization") != "" || observed.header.Get("X-Static") != "configured" || observed.header.Get("X-Trace") != "forwarded" || observed.header.Get("X-Blocked") != "" || observed.header.Get("Accept-Encoding") != "identity" || observed.header.Get(core.RequestIDHeader) != "client-request-id" {
		t.Fatalf("upstream headers = %v", observed.header)
	}
	select {
	case <-responseClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream response remained owned after parsing")
	}
	completion := receiveTranslationCompletion(t, recorder)
	if completion.Err != nil || completion.Usage != response.Usage || completion.Request.ClientModel != "client-alias" || completion.Request.UpstreamModel != "upstream-model" || completion.Request.ResolvedProvider != "pipeline-provider" || completion.Request.InboundProtocol != ir.ProtocolOpenAIChat || completion.Request.UpstreamProtocol != providerProtocol(t) {
		t.Fatalf("success completion = %+v", completion)
	}

	errorOut := httptest.NewRecorder()
	errorRequest := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"client-alias","prompt":"fail"}`))
	engine.ServeHTTP(errorOut, errorRequest)
	if errorOut.Code != http.StatusTooManyRequests || errorOut.Header().Get("Retry-After") != "2" || errorOut.Header().Get(core.RequestIDHeader) != "upstream-failure-id" || strings.Contains(errorOut.Body.String(), translationSecret) || !strings.Contains(errorOut.Body.String(), "[redacted]") {
		t.Fatalf("translated error = %d %v %s", errorOut.Code, errorOut.Header(), errorOut.Body.String())
	}
	<-requests
	errorCompletion := receiveTranslationCompletion(t, recorder)
	if errorCompletion.Err == nil || errorCompletion.Usage != (ir.Usage{}) || errorCompletion.Request.ClientModel != "client-alias" {
		t.Fatalf("error completion = %+v", errorCompletion)
	}
	if decodes.Load() != 2 || builds.Load() != 2 {
		t.Fatalf("adapter calls: decodes=%d builds=%d", decodes.Load(), builds.Load())
	}
	select {
	case extra := <-recorder.completions:
		t.Fatalf("request completion recorded more than once: %+v", extra)
	default:
	}
}

func providerProtocol(t *testing.T) ir.Protocol {
	t.Helper()
	protocol, err := provider.ParseProtocol("custom:" + translationAdapterName)
	if err != nil {
		t.Fatal(err)
	}
	return protocol
}

func TestTranslationFallbackPreservesOriginalBody(t *testing.T) {
	p := translationProvider(t, "https://upstream.invalid")
	for _, tc := range []struct {
		name     string
		registry func(*atomic.Int32, *atomic.Int32) *adapter.Registry
		body     string
	}{
		{"nil registry", func(*atomic.Int32, *atomic.Int32) *adapter.Registry { return nil }, ` { "model":"client-alias", "stream":false, "opaque":9007199254740993 } `},
		{"missing inbound", func(d, b *atomic.Int32) *adapter.Registry { return translationRegistry(d, b, false, true) }, ` { "model":"client-alias", "stream":false, "opaque":9007199254740993 } `},
		{"missing outbound", func(d, b *atomic.Int32) *adapter.Registry { return translationRegistry(d, b, true, false) }, ` { "model":"client-alias", "stream":false, "opaque":9007199254740993 } `},
		{"streaming", func(d, b *atomic.Int32) *adapter.Registry { return translationRegistry(d, b, true, true) }, ` { "model":"client-alias", "stream":true, "opaque":9007199254740993 } `},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var decodes, builds atomic.Int32
			dispatcher := translationDispatcher(t, &p, tc.registry(&decodes, &builds), nil)
			engine := gin.New()
			engine.Use(dispatcher.Handle)
			engine.POST("/v1/chat/completions", func(c *gin.Context) {
				body, err := io.ReadAll(c.Request.Body)
				if err != nil {
					t.Fatal(err)
				}
				c.Data(http.StatusOK, "application/json", body)
			})
			out := httptest.NewRecorder()
			engine.ServeHTTP(out, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(tc.body)))
			if out.Code != http.StatusOK || out.Body.String() != tc.body || decodes.Load() != 0 || builds.Load() != 0 {
				t.Fatalf("fallback = %d %q, decodes=%d builds=%d", out.Code, out.Body.String(), decodes.Load(), builds.Load())
			}
		})
	}
}

func TestTranslationCancellationStopsUpstreamAndRecordsOnce(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	defer server.Close()
	var decodes, builds atomic.Int32
	recorder := newTranslationRecorder()
	prov := translationProvider(t, server.URL)
	dispatcher := translationDispatcher(t, &prov, translationRegistry(&decodes, &builds, true, true), recorder)
	engine := gin.New()
	engine.Use(dispatcher.Handle)
	engine.POST("/v1/chat/completions", func(c *gin.Context) { t.Error("canceled translation reached legacy downstream") })

	ctx, cancel := context.WithCancel(t.Context())
	request := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"client-alias","prompt":"wait"}`))
	out := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		engine.ServeHTTP(out, request)
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("translation never reached upstream")
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request survived client cancellation")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatcher did not return after cancellation")
	}
	completion := receiveTranslationCompletion(t, recorder)
	if !errors.Is(completion.Err, context.Canceled) || completion.Usage != (ir.Usage{}) {
		t.Fatalf("canceled completion = %+v", completion)
	}
	select {
	case extra := <-recorder.completions:
		t.Fatalf("cancellation recorded more than once: %+v", extra)
	default:
	}
}

func TestTranslationDropsUnsupportedParametersHeader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := io.WriteString(w, `{"id":"upstream-id","model":"upstream-model","answer":"pong"}`); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()

	var decodes, builds atomic.Int32
	prov := translationProvider(t, server.URL)
	registry := translationRegistry(&decodes, &builds, true, true)
	// Register Anthropic inbound so /v1/messages is decoded with native top_k
	registry.RegisterInbound(inboundanthropic.New())
	dispatcher := translationDispatcher(t, &prov, registry, newTranslationRecorder())

	engine := gin.New()
	engine.Use(dispatcher.Handle)

	clientJSON := `{"model":"client-alias","max_tokens":100,"messages":[{"role":"user","content":"ping"}],"top_k":40}`
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages", strings.NewReader(clientJSON))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer client-secret")

	out := httptest.NewRecorder()
	engine.ServeHTTP(out, request)

	if out.Code != http.StatusOK {
		t.Fatalf("status = %d %s", out.Code, out.Body.String())
	}
	if got := out.Header().Get("X-Gateway-Dropped-Params"); got != "top_k" {
		t.Errorf("X-Gateway-Dropped-Params = %q, want %q", got, "top_k")
	}
}
