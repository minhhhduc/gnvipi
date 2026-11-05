package outbound_test

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/outbound"
)

func TestNewJSONRequestUsesResolvedTargetWithoutMutatingInputs(t *testing.T) {
	fields := map[string]any{
		"model":    "unresolved",
		"vendor":   json.RawMessage(`900719925474099312345678901`),
		"messages": []any{},
	}
	headers := http.Header{"Authorization": {"Bearer fixture-only"}, "X-Fixture": {"one", "two"}}
	wantHeaders := headers.Clone()
	request, err := outbound.NewJSONRequest(t.Context(), adapter.Target{
		BaseURL: "https://upstream.invalid/prefix",
		Model:   "resolved-model",
		Headers: headers,
	}, "/chat/completions", fields)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := request.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	body, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	var model string
	if err := json.Unmarshal(got["model"], &model); err != nil || model != "resolved-model" {
		t.Fatalf("model = %q, %v", model, err)
	}
	if string(got["vendor"]) != "900719925474099312345678901" {
		t.Fatalf("vendor = %s", got["vendor"])
	}
	if request.URL.String() != "https://upstream.invalid/prefix/v1/chat/completions" {
		t.Fatalf("URL = %s", request.URL)
	}
	if request.Header.Get("Content-Type") != "application/json" || !reflect.DeepEqual(request.Header.Values("Authorization"), []string{"Bearer fixture-only"}) {
		t.Fatalf("headers = %v", request.Header)
	}
	if !reflect.DeepEqual(headers, wantHeaders) || fields["model"] != "unresolved" {
		t.Fatalf("inputs mutated: headers=%v fields=%v", headers, fields)
	}
}

func TestNewJSONRequestEndpointForms(t *testing.T) {
	for _, base := range []string{
		"https://upstream.invalid",
		"https://upstream.invalid/v1",
		"https://upstream.invalid/v1/chat/completions",
	} {
		request, err := outbound.NewJSONRequest(t.Context(), adapter.Target{BaseURL: base, Model: "m"}, "/chat/completions", map[string]any{"messages": []any{}})
		if err != nil {
			t.Fatalf("base %q: %v", base, err)
		}
		if request.URL.String() != "https://upstream.invalid/v1/chat/completions" {
			t.Errorf("base %q URL = %s", base, request.URL)
		}
		if err := request.Body.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for _, base := range []string{
		"https://user:secret@upstream.invalid",
		"https://upstream.invalid/prefix?token=secret",
		"https://upstream.invalid/%2e%2e/escape",
	} {
		if request, err := outbound.NewJSONRequest(t.Context(), adapter.Target{BaseURL: base, Model: "m"}, "/chat/completions", nil); err == nil {
			if request != nil && request.Body != nil {
				if err := request.Body.Close(); err != nil {
					t.Fatal(err)
				}
			}
			t.Errorf("unsafe base accepted: %s", base)
		}
	}
}

func TestUpstreamErrorMapsEnvelopeHeadersAndRetryAfter(t *testing.T) {
	response := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header: http.Header{
			"Retry-After":  {"2.5"},
			"X-Request-Id": {"req_fixture"},
		},
	}
	got := outbound.UpstreamError(response, []byte(`{"error":{"message":"slow down","type":"rate_limit_error","code":"quota"}}`))
	want := &gwerr.Error{
		Kind:              gwerr.KindRateLimit,
		Message:           "slow down",
		Code:              "quota",
		UpstreamRequestID: "req_fixture",
		UpstreamStatus:    http.StatusTooManyRequests,
		RetryAfter:        2500 * time.Millisecond,
	}
	if got.Kind != want.Kind || got.Message != want.Message || got.Code != want.Code || got.UpstreamRequestID != want.UpstreamRequestID || got.UpstreamStatus != want.UpstreamStatus || got.RetryAfter != want.RetryAfter {
		t.Fatalf("error = %+v, want %+v", got, want)
	}

	fallback := outbound.UpstreamError(&http.Response{StatusCode: http.StatusBadGateway, Header: make(http.Header)}, []byte(`not-json`))
	if fallback.Kind != gwerr.KindOverloaded || fallback.UpstreamStatus != http.StatusBadGateway || !strings.Contains(fallback.Message, "502") {
		t.Fatalf("fallback = %+v", fallback)
	}
}
