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
	status, response, bodies := crossProtocolExchange(t, protocol, inboundPath, clientPayload)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body: %s", status, response)
	}
	if len(bodies) != 1 {
		t.Fatalf("upstream requests = %d", len(bodies))
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal([]byte(bodies[0]), &body); err != nil {
		t.Fatalf("decode upstream request: %v; body: %s", err, bodies[0])
	}
	return body
}

// crossProtocolExchange drives one translated request and returns the client
// status, its response body and every request body the upstream received.
func crossProtocolExchange(t *testing.T, protocol ir.Protocol, inboundPath, clientPayload string) (status int, body string, upstreamBodies []string) {
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
	bodies := make([]string, 0, len(upstream.Requests()))
	for _, forwarded := range upstream.Requests() {
		bodies = append(bodies, forwarded.Body)
	}
	return w.Code, w.Body.String(), bodies
}

// #456: a Chat history replayed to Messages must not ship thinking blocks or
// Chat-only wire keys, and a parameterless call must still be a JSON object.
func TestCrossProtocolChatHistoryToMessages(t *testing.T) {
	body := runCrossProtocol(t, ir.ProtocolAnthropicMessages, "/v1/chat/completions",
		`{"model":"issue-model","max_completion_tokens":64,"response_format":{"type":"json_object"},"stream_options":{"include_usage":true},"messages":[`+
			`{"role":"developer","content":"be terse"},`+
			`{"role":"user","content":"find"},`+
			`{"role":"assistant","content":"looking","reasoning_content":"plan","tool_calls":[`+
			`{"id":"c1","type":"function","function":{"name":"lookup","arguments":""}}]},`+
			`{"role":"tool","tool_call_id":"c1","content":"found"},`+
			`{"role":"system","content":"stay terse"},`+
			`{"role":"user","content":"thanks"}]}`)

	for _, name := range []string{"response_format", "stream_options", "max_completion_tokens"} {
		if _, leaked := body[name]; leaked {
			t.Fatalf("Chat field %q leaked into the Messages request: %s", name, body[name])
		}
	}
	if string(body["max_tokens"]) != "64" {
		t.Fatalf("max_completion_tokens not translated: %s", body["max_tokens"])
	}
	for _, text := range []string{"be terse", "stay terse"} {
		if !strings.Contains(string(body["system"]), text) {
			t.Fatalf("instruction %q missing from the system prompt: %s", text, body["system"])
		}
	}
	var messages []struct {
		Role    string            `json:"role"`
		Content []json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(body["messages"], &messages); err != nil {
		t.Fatalf("decode messages: %v; body: %s", err, body["messages"])
	}
	if len(messages) != 3 || messages[0].Role != "user" || messages[1].Role != "assistant" || messages[2].Role != "user" {
		t.Fatalf("upstream messages = %s", body["messages"])
	}
	for _, block := range messages[1].Content {
		if strings.Contains(string(block), `"thinking"`) {
			t.Fatalf("unsigned Chat reasoning reached Anthropic as thinking: %s", block)
		}
		if strings.Contains(string(block), `"tool_use"`) && !strings.Contains(string(block), `"input":{}`) {
			t.Fatalf("parameterless tool call lost its object input: %s", block)
		}
	}
	if !strings.Contains(string(messages[1].Content[0]), "looking") {
		t.Fatalf("assistant answer lost: %s", body["messages"])
	}
}

// #456: a Chat request that carries only instructions has no Messages turn to
// translate, and must fail as a client error rather than a server fault.
func TestCrossProtocolChatInstructionsOnlyToMessages(t *testing.T) {
	status, response, bodies := crossProtocolExchange(t, ir.ProtocolAnthropicMessages, "/v1/chat/completions",
		`{"model":"issue-model","messages":[{"role":"system","content":"be terse"}]}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, body: %s", status, response)
	}
	if len(bodies) != 0 {
		t.Fatalf("invalid request still reached the upstream: %v", bodies)
	}
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

// #455: a Chat body must not carry its own wire keys into the Responses API.
func TestCrossProtocolChatFieldsDoNotLeakIntoResponses(t *testing.T) {
	body := runCrossProtocol(t, ir.ProtocolOpenAIResponses, "/v1/chat/completions",
		`{"model":"issue-model","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"stream_options":{"include_usage":true},"response_format":{"type":"json_object"},"logit_bias":{},"frequency_penalty":0.5}`)

	for _, name := range []string{"messages", "max_tokens", "stream_options", "response_format", "logit_bias", "frequency_penalty"} {
		if _, found := body[name]; found {
			t.Errorf("Chat field %q leaked into the Responses request", name)
		}
	}
	if string(body["max_output_tokens"]) != "64" {
		t.Errorf("max_tokens not translated: %s", body["max_output_tokens"])
	}
	if !strings.Contains(string(body["text"]), `"format"`) || !strings.Contains(string(body["text"]), `"json_object"`) {
		t.Errorf("response_format not translated into text.format: %s", body["text"])
	}
}

// #455: a Chat history that another protocol already accepted must not produce
// a Responses payload the upstream rejects.
func TestCrossProtocolChatHistoryToResponses(t *testing.T) {
	t.Run("history-and-tools", func(t *testing.T) {
		body := runCrossProtocol(t, ir.ProtocolOpenAIResponses, "/v1/chat/completions",
			`{"model":"issue-model","reasoning_effort":"none","stop":["END"],"messages":[`+
				`{"role":"user","content":"find"},`+
				`{"role":"assistant","content":"looking","reasoning_content":"plan","tool_calls":[`+
				`{"id":"c1","type":"function","function":{"name":"lookup","arguments":""}}]},`+
				`{"role":"tool","tool_call_id":"c1","content":"found"},`+
				`{"role":"user","content":"thanks"}],`+
				`"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`)

		for _, name := range []string{"messages", "stop", "reasoning", "reasoning_effort"} {
			if _, found := body[name]; found {
				t.Fatalf("Chat field %q leaked into the Responses request: %s", name, body[name])
			}
		}
		var input []struct {
			Type      string          `json:"type"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(body["input"], &input); err != nil {
			t.Fatalf("decode input: %v; body: %s", err, body["input"])
		}
		for _, item := range input {
			if item.Type == "reasoning" {
				t.Fatalf("translated history synthesized a reasoning item: %s", body["input"])
			}
			if item.Type == "function_call" && string(item.Arguments) != `"{}"` {
				t.Fatalf("parameterless call arguments = %s, want a JSON object string", item.Arguments)
			}
		}
		var tools []struct {
			Parameters json.RawMessage `json:"parameters"`
		}
		if err := json.Unmarshal(body["tools"], &tools); err != nil || len(tools) != 1 {
			t.Fatalf("decode tools: %v; body: %s", err, body["tools"])
		}
		if !strings.Contains(string(tools[0].Parameters), `"properties"`) {
			t.Fatalf("object schema without properties reached the upstream: %s", tools[0].Parameters)
		}
	})

	t.Run("vendor-effort", func(t *testing.T) {
		body := runCrossProtocol(t, ir.ProtocolOpenAIResponses, "/v1/chat/completions",
			`{"model":"issue-model","reasoning_effort":"xhigh","messages":[{"role":"user","content":"hi"}]}`)
		if got := string(body["reasoning"]); got != `{"effort":"high"}` {
			t.Fatalf("reasoning = %s, want the documented high effort", got)
		}
	})
}

// #457: an explicitly disabled thinking config must not reach the Chat body.
func TestCrossProtocolAnthropicThinking(t *testing.T) {
	payload := `{"model":"issue-model","max_tokens":64,"thinking":{"type":"disabled"},"output_config":{"effort":"high"},` +
		`"messages":[{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"secret plan"},{"type":"text","text":"ok"}]},` +
		`{"role":"user","content":"again"}]}`

	chat := runCrossProtocol(t, ir.ProtocolOpenAIChat, "/v1/messages", payload)
	for _, name := range []string{"thinking", "output_config", "reasoning_effort"} {
		if _, found := chat[name]; found {
			t.Errorf("disabled thinking still reached the Chat upstream as %q", name)
		}
	}
}

func TestAnthropicUserIdentityFitsOpenAIWireLimit(t *testing.T) {
	{
		prefix := strings.Repeat("a", 64)
		users := []string{prefix, prefix + strings.Repeat("b", 86), prefix + strings.Repeat("c", 86), prefix + strings.Repeat("b", 86)}
		got := make([]string, len(users))
		for i, user := range users {
			payload, err := json.Marshal(map[string]any{
				"model": "issue-model", "max_tokens": 64,
				"messages": []map[string]string{{"role": "user", "content": "hi"}},
				"metadata": map[string]string{"user_id": user},
			})
			if err != nil {
				t.Fatal(err)
			}
			body := runCrossProtocol(t, ir.ProtocolOpenAIChat, "/v1/messages", string(payload))
			if err := json.Unmarshal(body["user"], &got[i]); err != nil {
				t.Fatal(err)
			}
			if got[i] == "" || len(got[i]) > 64 {
				t.Fatalf("upstream user length = %d, allowed 1..64", len(got[i]))
			}
		}
		if got[0] != prefix || got[1] == got[2] || got[1] != got[3] {
			t.Fatalf("identity changed, collided, or was unstable: %q", got)
		}
	}
}

// #457: an adaptive thinking config without an explicit effort, and a turn that
// holds nothing but unsigned thinking, must both produce a valid Chat request.
func TestCrossProtocolAnthropicThinkingEdges(t *testing.T) {
	body := runCrossProtocol(t, ir.ProtocolOpenAIChat, "/v1/messages",
		`{"model":"issue-model","max_tokens":64,"thinking":{"type":"adaptive"},`+
			`"messages":[{"role":"user","content":"hi"},`+
			`{"role":"assistant","content":[{"type":"thinking","thinking":""},{"type":"thinking","thinking":"unsigned plan"}]},`+
			`{"role":"user","content":"again"}]}`)

	if _, leaked := body["thinking"]; leaked {
		t.Fatalf("adaptive thinking config leaked into the Chat request: %s", body["thinking"])
	}
	if effort, found := body["reasoning_effort"]; found {
		var value string
		if err := json.Unmarshal(effort, &value); err != nil {
			t.Fatalf("reasoning_effort: %v", err)
		}
		switch value {
		case "low", "medium", "high":
		default:
			t.Fatalf("unsupported Chat reasoning_effort %q", value)
		}
	}
	var messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(body["messages"], &messages); err != nil {
		t.Fatalf("decode messages: %v; body: %s", err, body["messages"])
	}
	for _, message := range messages {
		if message.Role == "assistant" {
			t.Fatalf("a turn holding only unsigned thinking survived translation: %s", body["messages"])
		}
	}
}
