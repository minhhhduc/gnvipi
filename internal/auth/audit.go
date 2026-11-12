package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// AuditEntry is one append-only audit row. Subject columns are denormalised
// and intentionally not foreign keys, so rows survive subject deletion.
type AuditEntry struct {
	ActorType  string // user | token | system
	ActorID    string
	UserID     string
	KeyID      string
	ProviderID string
	RequestID  string
	Action     string
	TargetType string
	TargetID   string
	IP         string
	Details    map[string]any
}

// WriteAudit inserts an audit row. It accepts a pool or transaction so callers
// can write audit in the same transaction as the mutation. There is
// deliberately no update or delete function.
func WriteAudit(ctx context.Context, q Querier, e AuditEntry) error {
	if err := e.validate(); err != nil {
		return err
	}
	details, err := SanitizeDetails(e.Details)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(details)
	if err != nil {
		return err
	}
	if len(raw) > 8192 {
		return ErrAuditDetailsTooLarge
	}
	_, err = q.Exec(ctx, `
		INSERT INTO audit_log (actor_type, actor_id, user_id, key_id, provider_id,
		                       request_id, action, target_type, target_id, ip, details)
		VALUES ($1, NULLIF($2, '')::uuid, NULLIF($3, '')::uuid,
		        NULLIF($4, '')::uuid, NULLIF($5, ''), NULLIF($6, '')::uuid,
		        $7, $8, NULLIF($9, ''), $10::inet, $11::jsonb)`,
		e.ActorType, e.ActorID, e.UserID, e.KeyID, e.ProviderID, e.RequestID,
		e.Action, e.TargetType, e.TargetID, nullableIP(e.IP), string(raw))
	return err
}

func (e AuditEntry) validate() error {
	switch e.ActorType {
	case "user", "token", "system":
	default:
		return fmt.Errorf("auth: audit actor_type must be user, token or system")
	}
	if n := utf8.RuneCountInString(e.Action); n < 1 || n > 80 {
		return fmt.Errorf("auth: audit action must be 1-80 characters")
	}
	if n := utf8.RuneCountInString(e.TargetType); n < 1 || n > 40 {
		return fmt.Errorf("auth: audit target_type must be 1-40 characters")
	}
	return nil
}

// Sensitive audit detail keys are removed rather than trusted to callers.
var sensitiveDetailFragments = []string{
	"secret", "password", "passwd", "token", "authorization",
	"cookie", "api_key", "apikey", "credential", "header", "body", "prompt",
}

// SanitizeDetails returns a copy of details with secret-like keys replaced and
// rejects unsupported value shapes. This keeps headers, bodies, prompts,
// tokens, env values and refs out of the long-lived audit table.
func SanitizeDetails(details map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(details))
	for k, v := range details {
		if sensitiveDetailKey(k) {
			out[k] = "[redacted]"
			continue
		}
		clean, err := sanitizeValue(v, 0)
		if err != nil {
			return nil, err
		}
		out[k] = clean
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	if len(raw) > 8192 {
		return nil, ErrAuditDetailsTooLarge
	}
	return out, nil
}

func sensitiveDetailKey(key string) bool {
	k := strings.ToLower(key)
	for _, fragment := range sensitiveDetailFragments {
		if strings.Contains(k, fragment) {
			return true
		}
	}
	return k == "env" || k == "ref" || strings.HasSuffix(k, "_env") || strings.HasSuffix(k, "_ref")
}

func sanitizeValue(v any, depth int) (any, error) {
	if depth > 8 {
		return nil, ErrAuditInvalidDetails
	}
	switch x := v.(type) {
	case nil, bool, string,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64, json.Number:
		return x, nil
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, child := range x {
			if sensitiveDetailKey(k) {
				out[k] = "[redacted]"
				continue
			}
			clean, err := sanitizeValue(child, depth+1)
			if err != nil {
				return nil, err
			}
			out[k] = clean
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, child := range x {
			clean, err := sanitizeValue(child, depth+1)
			if err != nil {
				return nil, err
			}
			out[i] = clean
		}
		return out, nil
	case []string:
		out := make([]any, len(x))
		for i, child := range x {
			out[i] = child
		}
		return out, nil
	default:
		return nil, ErrAuditInvalidDetails
	}
}
