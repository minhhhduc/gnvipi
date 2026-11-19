package dispatch_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/minhhhduc/gnvipi/internal/gateway/dispatch"
	"github.com/minhhhduc/gnvipi/internal/gateway/gatewaytest"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"github.com/minhhhduc/gnvipi/internal/gateway/relay"
	"github.com/minhhhduc/gnvipi/internal/gateway/routing"
)

// runCrossProtocol drives one translated request and returns the JSON the
// upstream received.
func runCrossProtocol(t *testing.T, protocol ir.Protocol, inboundPath, clientPayload string) map[string]json.RawMessage {
	t.Helper()
	var route, canned string
	switch protocol {
	case ir.ProtocolOpenAIResponses:
		route, canned = gatewaytest.RouteResponses, `{"id":"resp_1","object":"response","status":"completed","model":"upstream-model","output":[{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`
	case ir.ProtocolAnthropicMessages:
		route, canned = gatewaytest.RouteMessages, `{"id":"msg_1","type":"message","role":"assistant","model":"upstream-model","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`
	default:
		route, canned = gatewaytest.RouteChat, `{"id":"chatcmpl_1","object":"chat.completion","model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
	}
	upstream := gatewaytest.New(t)
	upstream.Script(route, gatewaytest.Step{Status: http.StatusOK, Body: canned})

	prov := provider.Provider{
		ID:       "issue-provider",
		Name:     "Issue Provider",
		Protocol: protocol,
		BaseURL:  upstream.URL(),
		Auth:     provider.Auth{Scheme: provider.AuthBearer, Key: "secret-key"},
	}
	resolver := staticResolver{
		routes: map[string]routing.Route{
			"issue-model": {Kind: routing.Custom, Provider: &prov, UpstreamModel: "upstream-model", UpstreamProtocol: protocol},
		},
	}
	dispatcher := dispatch.New(resolver, &relay.Options{Recorder: newMatrixRecorder()})
	dispatcher.Registry = setupMatrixRegistry()
	engine := gin.New()
	engine.Use(dispatcher.Handle)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, inboundPath, strings.NewReader(clientPayload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
	reqs := upstream.Requests()
	if len(reqs) != 1 {
		t.Fatalf("upstream requests = %d", len(reqs))
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal([]byte(reqs[0].Body), &body); err != nil {
		t.Fatalf("decode upstream request: %v; body: %s", err, reqs[0].Body)
	}
	return body
}

// #454: a Responses history replayed as Chat must keep its order, its
// parameterless tool call and its mid-turn instruction.
func TestCrossProtocolResponsesHistoryToChat(t *testing.T) {
	body := runCrossProtocol(t, ir.ProtocolOpenAIChat, "/v1/responses",
		`{"model":"issue-model","input":[`+
			`{"type":"message","role":"user","content":[{"type":"input_text","text":"find"}]},`+
			`{"type":"function_call","call_id":"c1","name":"lookup"},`+
			`{"type":"message","role":"system","content":[{"type":"input_text","text":"be terse"}]},`+
			`{"type":"function_call_output","call_id":"c1","output":"found"},`+
			`{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}]}`)

	var messages []struct {
		Role       string            `json:"role"`
		ToolCalls  []json.RawMessage `json:"tool_calls"`
		ToolCallID string            `json:"tool_call_id"`
	}
	if err := json.Unmarshal(body["messages"], &messages); err != nil {
		t.Fatalf("decode messages: %v; body: %s", err, body["messages"])
	}
	roles := make([]string, 0, len(messages))
	for _, message := range messages {
		roles = append(roles, message.Role)
	}
	// The tool result must sit directly behind the assistant turn that issued
	// it, and the mid-turn system message must keep its position.
	if got := strings.Join(roles, ","); got != "user,assistant,tool,system,user" {
		t.Fatalf("upstream roles = %s, want user,assistant,tool,system,user", got)
	}
	if len(messages[1].ToolCalls) != 1 || !strings.Contains(string(messages[1].ToolCalls[0]), `"{}"`) {
		t.Fatalf("parameterless tool call lost its empty arguments: %v", messages[1].ToolCalls)
	}
	if messages[2].ToolCallID != "c1" {
		t.Fatalf("tool result not attached to c1: %+v", messages[2])
	}
}

// #454: Responses-only top-level fields must not reach /chat/completions, and
// every tool-output shape a Responses client replays must land in a valid Chat
// message.
func TestCrossProtocolResponsesFieldsAndToolOutputsToChat(t *testing.T) {
	body := runCrossProtocol(t, ir.ProtocolOpenAIChat, "/v1/responses",
		`{"model":"issue-model","instructions":"be terse","store":false,`+
			`"include":["reasoning.encrypted_content"],"text":{"format":{"type":"json_object"}},`+
			`"input":[`+
			`{"type":"message","role":"user","content":"find"},`+
			`{"type":"function_call","call_id":"c1","name":"lookup","arguments":{"q":"x"}},`+
			`{"type":"function_call_output","call_id":"c1","output":{"hits":[1,2]}},`+
			`{"type":"function_call","call_id":"c2","name":"shot"},`+
			`{"type":"function_call_output","call_id":"c2","output":[{"type":"future_part","payload":true}]}]}`)

	for _, field := range []string{"input", "instructions", "text", "store", "include", "max_output_tokens", "previous_response_id"} {
		if _, leaked := body[field]; leaked {
			t.Fatalf("Responses field %q leaked into the Chat payload: %s", field, body[field])
		}
	}

	var messages []struct {
		Role       string            `json:"role"`
		Content    json.RawMessage   `json:"content"`
		ToolCalls  []json.RawMessage `json:"tool_calls"`
		ToolCallID string            `json:"tool_call_id"`
	}
	if err := json.Unmarshal(body["messages"], &messages); err != nil {
		t.Fatalf("decode messages: %v; body: %s", err, body["messages"])
	}
	roles := make([]string, 0, len(messages))
	for _, message := range messages {
		roles = append(roles, message.Role)
	}
	if got := strings.Join(roles, ","); got != "system,user,assistant,tool,assistant,tool" {
		t.Fatalf("upstream roles = %s, want system,user,assistant,tool,assistant,tool", got)
	}
	if !strings.Contains(string(messages[2].ToolCalls[0]), `"{\"q\":\"x\"}"`) {
		t.Fatalf("object arguments not replayed as a JSON string: %s", messages[2].ToolCalls[0])
	}
	if got := string(messages[3].Content); got != `"{\"hits\":[1,2]}"` {
		t.Fatalf("structured tool output = %s, want its JSON as text", got)
	}
	if !strings.Contains(string(messages[5].Content), `future_part`) {
		t.Fatalf("unmapped tool output part was dropped: %s", messages[5].Content)
	}
}
