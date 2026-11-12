package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
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

// allowedAuditKeys is an allowlist of metadata keys. Audit details are
// long-lived and owner-readable, so anything not explicitly safe is dropped
// instead of being persisted.
var allowedAuditKeys = map[string]struct{}{
	"id": {}, "user_id": {}, "key_id": {}, "provider_id": {}, "request_id": {},
	"target_id": {}, "actor_id": {},
	"name": {}, "username": {}, "display_name": {}, "provider_name": {},
	"key_name": {}, "target_name": {}, "model": {}, "alias": {},
	"action": {}, "status": {}, "role": {}, "kind": {}, "scope": {},
	"enabled": {}, "disabled": {}, "success": {}, "hidden": {}, "deleted": {},
	"must_change": {}, "active": {},
	"count": {}, "total": {}, "revoked_sessions": {}, "revoked_tokens": {},
}

// SanitizeDetails returns only allowlisted scalar metadata. Unknown keys and
// objects, arrays and other non-scalar values are dropped, so a caller cannot
// persist a header, body, prompt, token or other arbitrary value.
func SanitizeDetails(details map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(details))
	for k, v := range details {
		if _, allowed := allowedAuditKeys[strings.ToLower(k)]; !allowed {
			continue
		}
		scalar, ok := auditScalar(v)
		if !ok {
			continue
		}
		out[k] = scalar
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

func auditScalar(v any) (any, bool) {
	switch x := v.(type) {
	case string, bool, json.Number,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64:
		return x, true
	case float32:
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return nil, false
		}
		return x, true
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil, false
		}
		return x, true
	default:
		return nil, false
	}
}
