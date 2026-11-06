package openaichat_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/adaptertest"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/outbound/openaichat"
)

func TestMain(m *testing.M) {
	adaptertest.RegisterFlags()
	os.Exit(m.Run())
}

func TestContract(t *testing.T) {
	adaptertest.RunOutboundContract(t, openaichat.New(), "testdata")
}

func TestConfiguredRequestUsesNormalizedRoutingAndContent(t *testing.T) {
	out := openaichat.New()
	out.InstructionRole = ir.InstructionDeveloper
	out.UseMaxCompletionTokens = true
	maxTokens := 64
	extensions := map[string]json.RawMessage{
		"model":          json.RawMessage(`"extension-model"`),
		"messages":       json.RawMessage(`[{"role":"user","content":"extension content"}]`),
		"stream":         json.RawMessage(`true`),
		"vendor_integer": json.RawMessage(`900719925474099312345678901`),
	}
	input := &ir.Request{
		Model:        "client-alias",
		Instructions: []ir.Instruction{ir.NewTextInstruction(ir.InstructionSystem, "Follow policy")},
		Messages:     []ir.Message{ir.NewMessage(ir.RoleUser, ir.NewTextBlock("normalized content"))},
		Sampling:     ir.Sampling{MaxOutputTokens: &maxTokens},
		Extensions:   extensions,
	}
	headers := http.Header{"Authorization": {"Bearer fixture-only"}, "X-Fixture": {"one", "two"}}
	wantHeaders := headers.Clone()

	request, err := out.BuildRequest(t.Context(), input, adapter.Target{
		BaseURL: "https://upstream.invalid/v1/chat/completions",
		Model:   "resolved-model",
		Headers: headers,
	})
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
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	var model string
	if err := json.Unmarshal(fields["model"], &model); err != nil || model != "resolved-model" {
		t.Fatalf("model = %q, %v", model, err)
	}
	var stream bool
	if err := json.Unmarshal(fields["stream"], &stream); err != nil || stream {
		t.Fatalf("stream = %v, %v", stream, err)
	}
	if _, found := fields["max_tokens"]; found {
		t.Fatal("max_tokens emitted with UseMaxCompletionTokens")
	}
	var gotMax int
	if err := json.Unmarshal(fields["max_completion_tokens"], &gotMax); err != nil || gotMax != maxTokens {
		t.Fatalf("max_completion_tokens = %d, %v", gotMax, err)
	}
	var messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(fields["messages"], &messages); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].Role != "developer" || string(messages[0].Content) != `"Follow policy"` || messages[1].Role != "user" || string(messages[1].Content) != `"normalized content"` {
		t.Fatalf("messages = %s", fields["messages"])
	}
	if got := string(fields["vendor_integer"]); got != "900719925474099312345678901" {
		t.Fatalf("vendor_integer = %s", got)
	}
	if request.URL.String() != "https://upstream.invalid/v1/chat/completions" {
		t.Fatalf("URL = %s", request.URL)
	}
	if request.Header.Get("Content-Type") != "application/json" || !reflect.DeepEqual(headers, wantHeaders) {
		t.Fatalf("headers mutated or missing content type: request=%v target=%v", request.Header, headers)
	}
	if string(extensions["model"]) != `"extension-model"` || string(extensions["messages"]) != `[{"role":"user","content":"extension content"}]` {
		t.Fatalf("extensions mutated: %v", extensions)
	}
}

func TestResponseAndStreamPreserveBodyOwnership(t *testing.T) {
	out := openaichat.New()
	responseBody := &trackedBody{Reader: strings.NewReader(`{"id":"chatcmpl_owned","object":"chat.completion","model":"vendor/model","choices":[{"index":0,"message":{"role":"assistant","content":"owned"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)}
	response := &http.Response{StatusCode: http.StatusOK, Body: responseBody, Header: make(http.Header)}
	got, err := out.ParseResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	if responseBody.closed || got == nil || len(got.Content) != 1 || got.Content[0].Text != "owned" {
		t.Fatalf("response=%+v body.closed=%v", got, responseBody.closed)
	}
	if err := responseBody.Close(); err != nil {
		t.Fatal(err)
	}

	if !out.Capabilities().Streaming {
		t.Fatal("adapter does not advertise streaming")
	}

	streamBody := &trackedBody{Reader: strings.NewReader("data: [DONE]\n\n")}
	stream, err := out.ParseStream(t.Context(), &http.Response{StatusCode: http.StatusOK, Body: streamBody})
	if err != nil {
		t.Fatalf("ParseStream failed: %v", err)
	}
	if streamBody.closed {
		t.Fatal("ParseStream closed body before stream.Close()")
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if !streamBody.closed {
		t.Fatal("stream.Close() did not close stream body")
	}

	errorBody := &trackedBody{Reader: strings.NewReader(`{"error":{"message":"bad request","type":"invalid_request_error"}}`)}
	errStream, err := out.ParseStream(t.Context(), &http.Response{StatusCode: http.StatusBadRequest, Body: errorBody})
	if errStream != nil {
		t.Fatal("expected nil stream on HTTP 400")
	}
	var gatewayError *gwerr.Error
	if !errors.As(err, &gatewayError) || gatewayError.Kind != gwerr.KindInvalidRequest {
		t.Fatalf("expected KindInvalidRequest, got %v", err)
	}
	if !errorBody.closed {
		t.Fatal("ParseStream failed to close error response body")
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
