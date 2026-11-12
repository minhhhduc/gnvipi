package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestSanitizeDetailsRedactsSensitiveValues(t *testing.T) {
	clean, err := SanitizeDetails(map[string]any{
		"user_id":    "11111111-1111-1111-1111-111111111111",
		"role":       "owner",
		"enabled":    true,
		"count":      3,
		"secret":     "super-secret",
		"password":   "hunter2hunter2",
		"token_name": "laptop",
		"headers":    map[string]any{"Authorization": "Bearer abc"},
		"request": map[string]any{
			"body":    `{"prompt":"hello"}`,
			"api_key": "sk-live",
			"model":   "nvidia/llama",
		},
		"key_env": "PROVIDER_KEY",
		"key_ref": "vault://provider",
		"prompt":  "tell me a secret",
	})
	if err != nil {
		t.Fatalf("sanitize: %v", err)
	}
	for _, key := range []string{"secret", "password", "token_name", "headers", "key_env", "key_ref", "prompt"} {
		if clean[key] != "[redacted]" {
			t.Errorf("%s not redacted: %#v", key, clean[key])
		}
	}
	nested := clean["request"].(map[string]any)
	if nested["body"] != "[redacted]" || nested["api_key"] != "[redacted]" {
		t.Fatalf("nested values not redacted: %#v", nested)
	}
	if nested["model"] != "nvidia/llama" {
		t.Fatalf("non-sensitive nested value lost: %#v", nested)
	}
	if clean["role"] != "owner" || clean["count"] != 3 {
		t.Fatalf("plain metadata changed: %#v", clean)
	}
}

func TestSanitizeDetailsRejectsUnsupportedAndOversized(t *testing.T) {
	if _, err := SanitizeDetails(map[string]any{"bad": make(chan int)}); !errors.Is(err, ErrAuditInvalidDetails) {
		t.Fatalf("unsupported type: %v", err)
	}
	if _, err := SanitizeDetails(map[string]any{"note": strings.Repeat("x", 9000)}); !errors.Is(err, ErrAuditDetailsTooLarge) {
		t.Fatalf("oversized details: %v", err)
	}
}

func TestWriteAuditValidatesEntry(t *testing.T) {
	ctx := context.Background()
	if err := WriteAudit(ctx, nil, AuditEntry{ActorType: "robot", Action: "x", TargetType: "user"}); err == nil {
		t.Fatal("invalid actor type accepted")
	}
	if err := WriteAudit(ctx, nil, AuditEntry{ActorType: "system", Action: "", TargetType: "user"}); err == nil {
		t.Fatal("empty action accepted")
	}
	if err := WriteAudit(ctx, nil, AuditEntry{ActorType: "system", Action: "x", TargetType: ""}); err == nil {
		t.Fatal("empty target type accepted")
	}
	if err := WriteAudit(ctx, nil, AuditEntry{ActorType: "system", Action: strings.Repeat("a", 81), TargetType: "user"}); err == nil {
		t.Fatal("oversized action accepted")
	}
}
