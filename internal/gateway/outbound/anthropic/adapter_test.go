package anthropic

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/adaptertest"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
)

func TestBuildRequestGoldens(t *testing.T) {
	tests := []struct {
		name, input, golden, baseURL, model string
		headers                             http.Header
	}{
		{
			name:    "role merging and default token budget",
			input:   "testdata/request-role-merge.ir.json",
			golden:  "testdata/request-role-merge.golden.json",
			baseURL: "https://upstream.invalid/prefix/v1",
			model:   "claude-resolved",
			headers: http.Header{
				"X-Api-Key": {"fixture-secret"},
				"X-Multi":   {"one", "two"},
			},
		},
		{
			name:    "thinking and full endpoint",
			input:   "testdata/request-thinking.ir.json",
			golden:  "testdata/request-thinking.golden.json",
			baseURL: "https://upstream.invalid/prefix/v1/messages",
			model:   "claude-thinking-resolved",
			headers: http.Header{"Authorization": {"Bearer fixture-secret"}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var input ir.Request
			readJSON(t, test.input, &input)
			before, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			target := adapter.Target{BaseURL: test.baseURL, Model: test.model, Headers: test.headers.Clone()}
			originalHeaders := target.Headers.Clone()

			request, err := New().BuildRequest(t.Context(), &input, target)
			if err != nil {
				t.Fatal(err)
			}
			if request == nil || request.Body == nil {
				t.Fatal("BuildRequest returned an incomplete request")
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
			after, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatalf("BuildRequest mutated the IR input\nbefore: %s\nafter:  %s", before, after)
			}
			if !reflect.DeepEqual(target.Headers, originalHeaders) {
				t.Fatalf("BuildRequest mutated Target.Headers: got %v, want %v", target.Headers, originalHeaders)
			}
			for name, values := range originalHeaders {
				if !reflect.DeepEqual(request.Header.Values(name), values) {
					t.Fatalf("request header %q = %v, want %v", name, request.Header.Values(name), values)
				}
			}
			got, err := json.Marshal(struct {
				Method  string          `json:"method"`
				URL     string          `json:"url"`
				Headers http.Header     `json:"headers"`
				Body    json.RawMessage `json:"body"`
			}{request.Method, request.URL.String(), request.Header, body})
			if err != nil {
				t.Fatal(err)
			}
			assertJSONFile(t, got, test.golden)
		})
	}
}

func TestDefaultMaxTokensIsConfigurable(t *testing.T) {
	a := New()
	a.DefaultMaxTokens = 8192
	request, err := a.BuildRequest(t.Context(), &ir.Request{
		Messages: []ir.Message{ir.NewMessage(ir.RoleUser, ir.NewTextBlock("hello"))},
	}, adapter.Target{BaseURL: "https://upstream.invalid", Model: "claude-resolved"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := request.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	var body struct {
		MaxTokens int `json:"max_tokens"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.MaxTokens != 8192 {
		t.Fatalf("max_tokens = %d, want configured default 8192", body.MaxTokens)
	}
}

func TestBuildRequestRejectsUnrepresentableInput(t *testing.T) {
	tests := []struct {
		name    string
		request ir.Request
		message string
	}{
		{
			name: "assistant first",
			request: ir.Request{Messages: []ir.Message{
				ir.NewMessage(ir.RoleAssistant, ir.NewTextBlock("prefill")),
			}},
			message: "first message",
		},
		{
			name: "tools deferred",
			request: ir.Request{
				Messages: []ir.Message{ir.NewMessage(ir.RoleUser, ir.NewTextBlock("hello"))},
				Tools:    []ir.Tool{{Name: "lookup", Schema: json.RawMessage(`{"type":"object"}`)}},
			},
			message: "tool",
		},
		{
			name: "streaming deferred",
			request: ir.Request{
				Messages: []ir.Message{ir.NewMessage(ir.RoleUser, ir.NewTextBlock("hello"))},
				Stream:   true,
			},
			message: "stream",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, err := New().BuildRequest(t.Context(), &test.request, adapter.Target{
				BaseURL: "https://upstream.invalid", Model: "claude-resolved",
			})
			if request != nil {
				if err := request.Body.Close(); err != nil {
					t.Fatal(err)
				}
				t.Fatal("BuildRequest returned a request alongside an error")
			}
			var gatewayError *gwerr.Error
			if !errors.As(err, &gatewayError) || gatewayError.Kind != gwerr.KindInvalidRequest ||
				gatewayError.Code != "unsupported_feature" || !strings.Contains(strings.ToLower(gatewayError.Message), test.message) {
				t.Fatalf("error = %#v, want unsupported %s error", err, test.message)
			}
		})
	}
}

func TestParseResponseFixtures(t *testing.T) {
	tests := []struct {
		name, response, want string
	}{
		{"text and cache usage", "testdata/response-text.json", "testdata/response-text.ir.json"},
		{"thinking and minimal tool use", "testdata/response-thinking.json", "testdata/response-thinking.ir.json"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := &trackedBody{Reader: bytes.NewReader(readFile(t, test.response))}
			got, err := New().ParseResponse(&http.Response{StatusCode: http.StatusOK, Body: body})
			if err != nil {
				t.Fatal(err)
			}
			if body.closed {
				t.Fatal("ParseResponse closed its caller-owned response body")
			}
			encoded, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			assertJSONFile(t, encoded, test.want)
		})
	}
}

func TestParseAnthropicErrorEnvelope(t *testing.T) {
	body := &trackedBody{Reader: bytes.NewReader(readFile(t, "testdata/response-error.json"))}
	response, err := New().ParseResponse(&http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header: http.Header{
			"Request-Id":  {"req_header"},
			"Retry-After": {"3"},
		},
		Body: body,
	})
	if response != nil {
		t.Fatalf("ParseResponse returned IR alongside an error: %+v", response)
	}
	if body.closed {
		t.Fatal("ParseResponse closed its caller-owned response body")
	}
	var gatewayError *gwerr.Error
	if !errors.As(err, &gatewayError) {
		t.Fatalf("error = %T %v, want *gwerr.Error", err, err)
	}
	want := &gwerr.Error{
		Kind:              gwerr.KindRateLimit,
		Message:           "Too many requests.",
		UpstreamRequestID: "req_header",
		UpstreamStatus:    http.StatusTooManyRequests,
		RetryAfter:        3 * time.Second,
	}
	if !reflect.DeepEqual(gatewayError, want) {
		t.Fatalf("gateway error = %+v, want %+v", gatewayError, want)
	}
}

func TestMain(m *testing.M) {
	adaptertest.RegisterFlags()
	os.Exit(m.Run())
}

func TestOutboundContract(t *testing.T) {
	a := New()
	adaptertest.RunOutboundContract(t, a, "testdata")
	if a.Protocol() != ir.ProtocolAnthropicMessages {
		t.Fatalf("Protocol = %q, want %q", a.Protocol(), ir.ProtocolAnthropicMessages)
	}
	wantCapabilities := adapter.Capabilities{Images: true, Reasoning: true, TopK: true}
	if got := a.Capabilities(); !reflect.DeepEqual(got, wantCapabilities) {
		t.Fatalf("Capabilities = %+v, want %+v", got, wantCapabilities)
	}

	body := &trackedBody{Reader: strings.NewReader("event: message_start\n\n")}
	stream, err := a.ParseStream(t.Context(), &http.Response{StatusCode: http.StatusOK, Body: body})
	if stream != nil {
		t.Fatal("ParseStream returned fake events for an unsupported feature")
	}
	if body.closed {
		t.Fatal("ParseStream closed the caller-owned body on rejection")
	}
	var gatewayError *gwerr.Error
	if !errors.As(err, &gatewayError) || gatewayError.Kind != gwerr.KindInvalidRequest ||
		gatewayError.Status != http.StatusBadRequest || gatewayError.Code != "unsupported_feature" ||
		!strings.Contains(strings.ToLower(gatewayError.Message), "stream") {
		t.Fatalf("ParseStream error = %#v, want typed unsupported streaming error", err)
	}
}

type trackedBody struct {
	io.Reader
	closed bool
}

func (b *trackedBody) Close() error {
	b.closed = true
	return nil
}

func readJSON(t *testing.T, path string, dst any) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(readFile(t, path)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		t.Fatalf("%s has trailing JSON", path)
	}
}

func assertJSONFile(t *testing.T, got []byte, path string) {
	t.Helper()
	var gotValue, wantValue any
	gotDecoder := json.NewDecoder(bytes.NewReader(got))
	gotDecoder.UseNumber()
	if err := gotDecoder.Decode(&gotValue); err != nil {
		t.Fatalf("decode actual JSON: %v\n%s", err, got)
	}
	want := readFile(t, path)
	wantDecoder := json.NewDecoder(bytes.NewReader(want))
	wantDecoder.UseNumber()
	if err := wantDecoder.Decode(&wantValue); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("JSON mismatch\ngot:  %s\nwant: %s", got, want)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body
}
