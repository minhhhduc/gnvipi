package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestSanitizeDetailsAllowlist(t *testing.T) {
	clean, err := SanitizeDetails(map[string]any{
		"user_id":          "11111111-1111-1111-1111-111111111111",
		"name":             "Alice",
		"role":             "owner",
		"status":           "active",
		"action":           "user.update",
		"enabled":          true,
		"count":            3,
		"revoked_sessions": int64(2),
		"note":             "<secret>",
		"secret":           "super-secret",
		"password":         "hunter2hunter2",
		"headers":          map[string]any{"Authorization": "Bearer abc"},
		"request": map[string]any{
			"body":    `{"prompt":"hello"}`,
			"api_key": "sk-live",
			"model":   "nvidia/llama",
		},
		"display_name": map[string]any{"value": "nested"},
	})
	if err != nil {
		t.Fatalf("sanitize: %v", err)
	}
	for _, key := range []string{"note", "secret", "password", "headers", "request", "display_name"} {
		if _, kept := clean[key]; kept {
			t.Errorf("non-allowlisted key %q persisted: %#v", key, clean[key])
		}
	}
	if clean["role"] != "owner" || clean["count"] != 3 ||
		clean["enabled"] != true || clean["name"] != "Alice" {
		t.Fatalf("allowed metadata changed or dropped: %#v", clean)
	}
	raw, err := json.Marshal(clean)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, forbidden := range []string{"<secret>", "super-secret", "hunter2hunter2", "Bearer abc", "sk-live", `"prompt"`} {
		if bytes.Contains(raw, []byte(forbidden)) {
			t.Fatalf("sanitized audit details contain %q: %s", forbidden, raw)
		}
	}
}

func TestSanitizeDetailsDropsUnsupportedAndRejectsOversized(t *testing.T) {
	clean, err := SanitizeDetails(map[string]any{
		"count": make(chan int), // allowed key, non-scalar value
		"meta":  map[string]any{"user_id": "x"},
	})
	if err != nil {
		t.Fatalf("sanitize: %v", err)
	}
	if len(clean) != 0 {
		t.Fatalf("non-scalar values persisted: %#v", clean)
	}
	if _, err := SanitizeDetails(map[string]any{"name": strings.Repeat("x", 9000)}); !errors.Is(err, ErrAuditDetailsTooLarge) {
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
