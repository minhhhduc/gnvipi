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

// #456: an Anthropic turn whose thinking is disabled must not ask the Chat
// upstream for reasoning, and an unsigned thinking block must not be replayed
// to an Anthropic upstream that would reject it.
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
	for _, protocol := range []ir.Protocol{ir.ProtocolOpenAIChat, ir.ProtocolOpenAIResponses} {
		t.Run(string(protocol), func(t *testing.T) {
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
				body := runCrossProtocol(t, protocol, "/v1/messages", string(payload))
				if err := json.Unmarshal(body["user"], &got[i]); err != nil {
					t.Fatal(err)
				}
				if len(got[i]) == 0 || len(got[i]) > 64 {
					t.Fatalf("upstream user length = %d, allowed 1..64", len(got[i]))
				}
			}
			if got[0] != prefix || got[1] == got[2] || got[1] != got[3] {
				t.Fatalf("identity changed, collided, or was unstable: %q", got)
			}
		})
	}
}

func TestResponsesCiphertextIsNotAnthropicThinkingSignature(t *testing.T) {
	body := runCrossProtocol(t, ir.ProtocolAnthropicMessages, "/v1/responses",
		`{"model":"issue-model","reasoning":{"effort":"medium"},"input":[`+
			`{"role":"user","content":"hi"},`+
			`{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"plan"}],"encrypted_content":"responses-ciphertext"},`+
			`{"role":"assistant","content":[{"type":"output_text","text":"ok"}]},`+
			`{"role":"user","content":"continue"}]}`)
	var messages []struct {
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body["messages"], &messages); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 3 || messages[1].Role != "assistant" || len(messages[1].Content) != 1 ||
		messages[1].Content[0].Type != "text" || messages[1].Content[0].Text != "ok" {
		t.Fatalf("foreign reasoning replayed or assistant answer lost: %s", body["messages"])
	}
}
