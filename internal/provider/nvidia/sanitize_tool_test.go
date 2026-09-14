package nvidia

import (
	"encoding/json"
	"testing"
)

func toolContent(t *testing.T, body, path string) any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("output not json: %v", err)
	}
	msgs, ok := v["messages"].([]any)
	if !ok || len(msgs) == 0 {
		t.Fatalf("%s: no messages in %s", path, body)
	}
	msg, _ := msgs[0].(map[string]any)
	return msg["content"]
}

func TestSanitizeToolMessages(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		want  string // expected content after sanitize
		isStr bool  // want content to be a JSON string
	}{
		{"object content wrapped", `{"model":"m","messages":[{"role":"tool","tool_call_id":"t1","content":{"ok":true}}]}`, `{"ok":true}`, true},
		{"string content untouched", `{"messages":[{"role":"tool","content":"plain"}]}`, "plain", true},
		{"bad part type wrapped", `{"messages":[{"role":"tool","content":[{"type":"tool_result","text":"x"}]}]}`, `[{"type":"tool_result","text":"x"}]`, true},
		{"good parts untouched", `{"messages":[{"role":"tool","content":[{"type":"text","text":"a"},{"type":"image_url","image_url":{"url":"u"}}]}]}`, "", false},
		{"null content untouched", `{"messages":[{"role":"tool","content":null}]}`, "", false},
		{"number content wrapped", `{"messages":[{"role":"tool","content":42}]}`, "42", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := sanitizeToolMessages([]byte(c.in))
			if err != nil {
				t.Fatalf("sanitize: %v", err)
			}
			got := toolContent(t, string(out), c.name)
			if c.isStr {
				s, ok := got.(string)
				if !ok || s != c.want {
					t.Fatalf("content = %#v, want string %q", got, c.want)
				}
			} else if got == nil && c.want != "" {
				t.Fatalf("content became nil, want preserved: %s", out)
			}
		})
	}
}
