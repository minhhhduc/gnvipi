package openairesponses_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/outbound/openairesponses"
)

var _ adapter.Outbound = openairesponses.New()

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func compareJSON(t *testing.T, got, want []byte) {
	t.Helper()
	var values [2]any
	for i, body := range [][]byte{got, want} {
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.UseNumber()
		if err := decoder.Decode(&values[i]); err != nil {
			t.Fatalf("decode JSON %d: %v\n%s", i, err, body)
		}
		if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
			t.Fatalf("JSON %d has trailing data: %v", i, err)
		}
	}
	if !reflect.DeepEqual(values[0], values[1]) {
		t.Fatalf("JSON mismatch\ngot:  %s\nwant: %s", got, want)
	}
}

func loadRequest(t *testing.T, name string) *ir.Request {
	t.Helper()
	var request ir.Request
	if err := json.Unmarshal(fixture(t, name), &request); err != nil {
		t.Fatal(err)
	}
	return &request
}

func TestAdapterContract(t *testing.T) {
	out := openairesponses.New()
	if out == nil || out.Protocol() != ir.ProtocolOpenAIResponses {
		t.Fatalf("New/Protocol = %#v/%q", out, out.Protocol())
	}
	want := adapter.Capabilities{Images: true, Reasoning: true, StructuredOutput: true}
	if got := out.Capabilities(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Capabilities = %+v, want %+v", got, want)
	}
}

func TestBuildRequestGoldens(t *testing.T) {
	for _, name := range []string{"legacy-common", "instructions", "mixed-instructions"} {
		t.Run(name, func(t *testing.T) {
			headers := http.Header{
				"Authorization": {"Bearer provider-credential"},
				"X-Trace":       {"first", "second"},
			}
			original := headers.Clone()
			target := adapter.Target{BaseURL: "https://provider.invalid/v1", Model: "resolved-model", Headers: headers}
			input := loadRequest(t, name+".request.ir.json")
			before, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			request, err := openairesponses.New().BuildRequest(t.Context(), input, target)
			if err != nil {
				t.Fatal(err)
			}
			after, marshalErr := json.Marshal(input)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			if !bytes.Equal(before, after) {
				t.Fatalf("BuildRequest mutated IR: before %s, after %s", before, after)
			}
			defer func() {
				if err := request.Body.Close(); err != nil {
					t.Error(err)
				}
			}()
			if !reflect.DeepEqual(headers, original) {
				t.Fatalf("BuildRequest mutated Target.Headers: got %v, want %v", headers, original)
			}
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Fatal(err)
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
			compareJSON(t, got, fixture(t, name+".request.golden.json"))
		})
	}
}

func TestProviderEndpointSemantics(t *testing.T) {
	request := &ir.Request{Messages: []ir.Message{ir.NewMessage(ir.RoleUser, ir.NewTextBlock("hello"))}}
	for base, want := range map[string]string{
		"https://provider.invalid":                   "https://provider.invalid/v1/responses",
		"https://provider.invalid/v1":                "https://provider.invalid/v1/responses",
		"https://provider.invalid/v1/responses":      "https://provider.invalid/v1/responses",
		"https://provider.invalid/prefix":            "https://provider.invalid/prefix/v1/responses",
		"https://provider.invalid/prefix/responses/": "https://provider.invalid/prefix/responses",
	} {
		t.Run(base, func(t *testing.T) {
			got, err := openairesponses.New().BuildRequest(t.Context(), request, adapter.Target{BaseURL: base, Model: "upstream"})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := got.Body.Close(); err != nil {
					t.Error(err)
				}
			}()
			if got.URL.String() != want {
				t.Fatalf("URL = %q, want %q", got.URL, want)
			}
		})
	}
}

func TestBuildRequestRejectsMalformedOrUnsupportedIR(t *testing.T) {
	validTarget := adapter.Target{BaseURL: "https://provider.invalid", Model: "upstream"}
	invalidImage := ir.NewImageBlockURL("")
	badSchema := ir.ResponseFormat{Type: ir.ResponseFormatJSONSchema, JSONSchema: &ir.JSONSchema{Schema: json.RawMessage(`{"type":"object"}`)}}
	for _, tc := range []struct {
		name    string
		request *ir.Request
	}{
		{"nil request", nil},
		{"streaming", &ir.Request{Stream: true, Messages: []ir.Message{ir.NewMessage(ir.RoleUser, ir.NewTextBlock("hello"))}}},
		{"unsupported role", &ir.Request{Messages: []ir.Message{ir.NewMessage(ir.Role("system"), ir.NewTextBlock("hello"))}}},
		{"invalid image", &ir.Request{Messages: []ir.Message{ir.NewMessage(ir.RoleUser, invalidImage)}}},
		{"invalid response format", &ir.Request{Messages: []ir.Message{ir.NewMessage(ir.RoleUser, ir.NewTextBlock("hello"))}, ResponseFormat: &badSchema}},
		{"tools deferred", &ir.Request{Messages: []ir.Message{ir.NewMessage(ir.RoleUser, ir.NewTextBlock("hello"))}, Tools: []ir.Tool{{Name: "lookup"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, err := openairesponses.New().BuildRequest(t.Context(), tc.request, validTarget)
			if request != nil && request.Body != nil {
				if err := request.Body.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if err == nil {
				t.Fatal("BuildRequest accepted malformed or unsupported IR")
			}
		})
	}
}

func TestParseResponseGoldensAndBodyOwnership(t *testing.T) {
	for _, name := range []string{"completed", "incomplete"} {
		t.Run(name, func(t *testing.T) {
			body := newTrackedBody(fixture(t, name+".response.json"))
			response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}
			got, err := openairesponses.New().ParseResponse(response)
			if err != nil {
				t.Fatal(err)
			}
			if body.closed || body.Len() != 0 {
				t.Fatalf("ParseResponse body ownership/read = closed %v, remaining %d", body.closed, body.Len())
			}
			encoded, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			compareJSON(t, encoded, fixture(t, name+".response.ir.json"))
			if err := body.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestParseResponseMapsUpstreamErrorAndLeavesBodyOpen(t *testing.T) {
	body := newTrackedBody(fixture(t, "upstream-error.response.json"))
	response := &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{
		"Retry-After":  {"30"},
		"X-Request-Id": {"req_responses_123"},
	}, Body: body}
	got, err := openairesponses.New().ParseResponse(response)
	if got != nil {
		t.Fatalf("ParseResponse returned response with error: %+v", got)
	}
	var gatewayError *gwerr.Error
	if !errors.As(err, &gatewayError) {
		t.Fatalf("ParseResponse error = %T %v, want *gwerr.Error", err, err)
	}
	view, err := json.Marshal(struct {
		Kind              gwerr.Kind `json:"kind"`
		Message           string     `json:"message"`
		Status            int        `json:"status,omitempty"`
		Code              string     `json:"code,omitempty"`
		UpstreamStatus    int        `json:"upstream_status,omitempty"`
		UpstreamRequestID string     `json:"upstream_request_id,omitempty"`
		RetryAfterMS      int64      `json:"retry_after_ms,omitempty"`
	}{gatewayError.Kind, gatewayError.Message, gatewayError.Status, gatewayError.Code, gatewayError.UpstreamStatus, gatewayError.UpstreamRequestID, gatewayError.RetryAfter.Milliseconds()})
	if err != nil {
		t.Fatal(err)
	}
	compareJSON(t, view, fixture(t, "upstream-error.golden.json"))
	if body.closed || body.Len() != 0 {
		t.Fatalf("error body ownership/read = closed %v, remaining %d", body.closed, body.Len())
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestParseResponseRejectsMalformedWire(t *testing.T) {
	if got, err := openairesponses.New().ParseResponse(nil); err == nil || got != nil {
		t.Fatalf("nil response = %+v, %v", got, err)
	}
	for _, bodyText := range []string{
		`not json`,
		`{"id":"resp","model":"m","status":"completed","output":[]} {}`,
		`{"id":"resp","model":"m","status":"incomplete","output":[]}`,
		`{"id":"resp","model":"m","status":"completed","output":[{"type":"future_item"}]}`,
		`{"id":"resp","model":"m","status":"completed","output":[{"type":"message","content":[{"type":"output_text"}]}]}`,
		`{"id":"resp","model":"m","status":"completed","output":[{"type":"function_call","call_id":"c","name":"f","arguments":"{"}]}`,
	} {
		t.Run(bodyText, func(t *testing.T) {
			body := newTrackedBody([]byte(bodyText))
			got, err := openairesponses.New().ParseResponse(&http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body})
			if err == nil || got != nil {
				t.Fatalf("malformed response = %+v, %v", got, err)
			}
			if body.closed {
				t.Fatal("ParseResponse closed caller-owned body on rejection")
			}
			if err := body.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestParseStreamIsTypedUnsupportedWithoutTakingBodyOwnership(t *testing.T) {
	body := newTrackedBody([]byte("data: should-not-be-read\n\n"))
	response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body}
	stream, err := openairesponses.New().ParseStream(context.Background(), response)
	if stream != nil {
		t.Fatalf("ParseStream returned fake stream: %#v", stream)
	}
	var gatewayError *gwerr.Error
	if !errors.As(err, &gatewayError) || gatewayError.Code != "unsupported_feature" || gatewayError.Kind != gwerr.KindInvalidRequest {
		t.Fatalf("ParseStream error = %T %v", err, err)
	}
	if body.closed || body.Len() != len("data: should-not-be-read\n\n") {
		t.Fatalf("ParseStream touched caller-owned body: closed %v, remaining %d", body.closed, body.Len())
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
}

type trackedBody struct {
	*bytes.Reader
	closed bool
}

func newTrackedBody(body []byte) *trackedBody {
	return &trackedBody{Reader: bytes.NewReader(body)}
}

func (b *trackedBody) Close() error {
	b.closed = true
	return nil
}
