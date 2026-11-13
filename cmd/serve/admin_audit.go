package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// auditCursorTimeLayout keeps nanosecond precision so a cursor page never
// repeats or skips rows that share a timestamp.
const auditCursorTimeLayout = time.RFC3339Nano

type auditRecord struct {
	ID         int64           `json:"id"`
	TS         time.Time       `json:"ts"`
	ActorType  string          `json:"actor_type"`
	ActorID    string          `json:"actor_id,omitempty"`
	UserID     string          `json:"user_id,omitempty"`
	KeyID      string          `json:"key_id,omitempty"`
	ProviderID string          `json:"provider_id,omitempty"`
	RequestID  string          `json:"request_id,omitempty"`
	Action     string          `json:"action"`
	TargetType string          `json:"target_type"`
	TargetID   string          `json:"target_id,omitempty"`
	IP         string          `json:"ip,omitempty"`
	Details    json.RawMessage `json:"details"`
}

func registerAdminAuditRoutes(engine *gin.Engine, state *authState) {
	engine.GET("/admin/api/audit", handleAdminAudit(state))
}

func handleAdminAudit(state *authState) gin.HandlerFunc {
	return func(c *gin.Context) {
		limit, _, ok := adminPage(c)
		if !ok {
			return
		}
		var after, before *auditCursor
		if raw := strings.TrimSpace(c.Query("after")); raw != "" {
			cursor, valid := decodeAuditCursor(raw)
			if !valid {
				adminFieldError(c, http.StatusBadRequest, "after", "after must be a cursor returned by this endpoint")
				return
			}
			after = &cursor
		}
		if raw := strings.TrimSpace(c.Query("before")); raw != "" {
			cursor, valid := decodeAuditCursor(raw)
			if !valid {
				adminFieldError(c, http.StatusBadRequest, "before", "before must be a cursor returned by this endpoint")
				return
			}
			before = &cursor
		}
		if after != nil && before != nil {
			adminFieldError(c, http.StatusBadRequest, "after", "pass either after or before, not both")
			return
		}

		query := `SELECT id, ts, actor_type, COALESCE(actor_id::text, ''), COALESCE(user_id::text, ''),
		                 COALESCE(key_id::text, ''), COALESCE(provider_id, ''), COALESCE(request_id::text, ''),
		                 action, target_type, COALESCE(target_id, ''), COALESCE(host(ip), ''), details`
		order := "DESC"
		var where []string
		var args []any
		if after != nil {
			where = append(where, `(ts, id) < ($1::timestamptz, $2::bigint)`)
			args = append(args, after.ts, after.id)
		} else if before != nil {
			where = append(where, `(ts, id) > ($1::timestamptz, $2::bigint)`)
			args = append(args, before.ts, before.id)
			// Page backwards from the cursor: keep the rows nearest it.
			order = "ASC"
		}
		sql := query + ` FROM audit_log`
		if len(where) > 0 {
			sql += ` WHERE ` + strings.Join(where, " AND ")
		}
		sql += ` ORDER BY ts ` + order + `, id ` + order + ` LIMIT $` + strconv.Itoa(len(args)+1)
		args = append(args, limit+1)

		rows, err := state.pool.Query(c.Request.Context(), sql, args...)
		if err != nil {
			adminAPIError(c, http.StatusInternalServerError, "audit_unavailable", "could not read the audit log")
			return
		}
		defer rows.Close()
		records := make([]auditRecord, 0, limit)
		for rows.Next() {
			var record auditRecord
			if err := rows.Scan(&record.ID, &record.TS, &record.ActorType, &record.ActorID,
				&record.UserID, &record.KeyID, &record.ProviderID, &record.RequestID,
				&record.Action, &record.TargetType, &record.TargetID, &record.IP, &record.Details); err != nil {
				adminAPIError(c, http.StatusInternalServerError, "audit_unavailable", "could not read the audit log")
				return
			}
			records = append(records, record)
		}
		if err := rows.Err(); err != nil {
			adminAPIError(c, http.StatusInternalServerError, "audit_unavailable", "could not read the audit log")
			return
		}
		hasMore := len(records) > limit
		if hasMore {
			records = records[:limit]
		}
		if before != nil {
			for i, j := 0, len(records)-1; i < j; i, j = i+1, j-1 {
				records[i], records[j] = records[j], records[i]
			}
		}

		body := gin.H{"entries": records, "has_more": hasMore, "limit": limit}
		if len(records) > 0 {
			oldest := records[len(records)-1]
			newest := records[0]
			body["next_cursor"] = encodeAuditCursor(oldest.TS, oldest.ID)
			body["before_cursor"] = encodeAuditCursor(newest.TS, newest.ID)
		}
		c.JSON(http.StatusOK, body)
	}
}

type auditCursor struct {
	ts time.Time
	id int64
}

func encodeAuditCursor(ts time.Time, id int64) string {
	raw := ts.UTC().Format(auditCursorTimeLayout) + "|" + strconv.FormatInt(id, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeAuditCursor(raw string) (auditCursor, bool) {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return auditCursor{}, false
	}
	parts := strings.Split(string(decoded), "|")
	if len(parts) != 2 {
		return auditCursor{}, false
	}
	ts, err := time.Parse(auditCursorTimeLayout, parts[0])
	if err != nil {
		return auditCursor{}, false
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || id < 1 {
		return auditCursor{}, false
	}
	return auditCursor{ts: ts, id: id}, true
}
