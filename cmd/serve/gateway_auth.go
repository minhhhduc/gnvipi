package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
)

// gatewayAuth protects client API routes only when at least one key is set.
// Authorization is authoritative when present: it must be a valid Bearer
// credential. If x-api-key is also supplied, it must identify the same key;
// conflicting or repeated headers are rejected, never silently ignored.
// Successful configured authentication consumes both headers before legacy
// handlers run; provider credentials remain independently configured.
func gatewayAuth(keyList string) gin.HandlerFunc {
	var keys [][sha256.Size]byte
	for _, key := range strings.Split(keyList, ",") {
		if key = strings.TrimSpace(key); key != "" {
			keys = append(keys, sha256.Sum256([]byte(key)))
		}
	}
	// The private, middleware-scoped key survives cycleRouter's request clones
	// without retaining a credential or trusting another gateway's middleware.
	authenticatedKey := new(int)

	return func(c *gin.Context) {
		path := c.Request.URL.Path
		if len(keys) == 0 || (!clientAPIPath(path) && path != "/mcp" && path != "/mcp/") {
			c.Next()
			return
		}
		if c.Request.Context().Value(authenticatedKey) == true {
			c.Request.Header.Del("Authorization")
			c.Request.Header.Del("x-api-key")
			c.Next()
			return
		}

		authHeaders := c.Request.Header.Values("Authorization")
		apiHeaders := c.Request.Header.Values("x-api-key")
		var key string
		valid := len(authHeaders) <= 1 && len(apiHeaders) <= 1
		if len(authHeaders) == 1 {
			parts := strings.Fields(authHeaders[0])
			if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
				valid = false
			} else {
				key = parts[1]
			}
		} else if len(apiHeaders) == 1 {
			key = strings.TrimSpace(apiHeaders[0])
		}

		// Hash first so comparison time does not reveal configured key lengths.
		// Visit every configured key, even after a match, to hide rotation order.
		digest := sha256.Sum256([]byte(key))
		matched := 0
		for i := range keys {
			matched |= subtle.ConstantTimeCompare(digest[:], keys[i][:])
		}
		if len(authHeaders) == 1 && len(apiHeaders) == 1 {
			apiDigest := sha256.Sum256([]byte(strings.TrimSpace(apiHeaders[0])))
			matched &= subtle.ConstantTimeCompare(digest[:], apiDigest[:])
		}
		if !valid || key == "" || matched != 1 {
			c.Header("WWW-Authenticate", `Bearer realm="gateway"`)
			c.Abort()
			const message = "Invalid or missing gateway API key"
			if path == "/mcp" || path == "/mcp/" {
				// Authentication precedes JSON-RPC parsing. Do not read an
				// unauthenticated request body merely to recover its id.
				c.JSON(http.StatusUnauthorized, gin.H{
					"jsonrpc": "2.0",
					"id":      nil,
					"error":   gin.H{"code": -32000, "message": message},
				})
				return
			}
			proto := gwerr.ProtocolOpenAIChat
			if path == "/v1/messages" || strings.HasPrefix(path, "/v1/messages/") {
				proto = gwerr.ProtocolAnthropicMessages
			} else if path == "/v1/responses" || strings.HasPrefix(path, "/v1/responses/") || path == "/backend-api/codex/responses" || strings.HasPrefix(path, "/backend-api/codex/responses/") {
				proto = gwerr.ProtocolOpenAIResponses
			}
			if err := gwerr.Write(c.Writer, proto, &gwerr.Error{Kind: gwerr.KindAuthentication, Message: message}); err != nil {
				log.Printf("write gateway authentication error: %v", err)
			}
			return
		}

		c.Request.Header.Del("Authorization")
		c.Request.Header.Del("x-api-key")
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), authenticatedKey, true))
		c.Next()
	}
}
