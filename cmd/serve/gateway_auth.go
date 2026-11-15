package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/auth"
	"github.com/minhhhduc/gnvipi/internal/gateway/core"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
)

// internalGatewayUserKey marks an in-process gateway call whose identity was
// already resolved by the admin console. A Go context value cannot arrive
// over the network, so this never widens the authentication surface.
type internalGatewayUserKey struct{}

func withInternalGatewayUser(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, internalGatewayUserKey{}, userID)
}

// gatewayAuth protects client API routes. Static gateway keys keep their
// legacy behavior; when a database authState is supplied, an unexpired
// gateway API token is also accepted and its user is attached to the request
// context for key-aware routing. Conflicts are rejected, never resolved by
// trying both credentials.
// gatewayTokenCache avoids one database round trip per authenticated request.
var gatewayTokenCache = auth.NewTokenCache(auth.TokenCacheTTL, 4096)

func gatewayAuth(keyList string, states ...*authState) gin.HandlerFunc {
	var keys [][sha256.Size]byte
	for _, key := range strings.Split(keyList, ",") {
		if key = strings.TrimSpace(key); key != "" {
			keys = append(keys, sha256.Sum256([]byte(key)))
		}
	}
	var db *authState
	if len(states) > 0 {
		db = states[0]
	}
	// The private, middleware-scoped key survives cycleRouter's request clones
	// without retaining a credential or trusting another gateway's middleware.
	authenticatedKey := new(int)

	return func(c *gin.Context) {
		path := c.Request.URL.Path
		if !clientAPIPath(path) && path != "/mcp" && path != "/mcp/" {
			c.Next()
			return
		}
		// The admin console forwards an already-authenticated request through
		// the gateway in-process. Trust only the context marker created by that
		// handler; no header or request field can produce it.
		if userID, ok := c.Request.Context().Value(internalGatewayUserKey{}).(string); ok && userID != "" {
			c.Request.Header.Del("Authorization")
			c.Request.Header.Del("x-api-key")
			if rc := core.RequestContextFrom(c.Request.Context()); rc != nil {
				rc.UserID = userID
			}
			c.Next()
			return
		}
		if len(keys) == 0 && db == nil {
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
		consistent := true
		if len(authHeaders) == 1 && len(apiHeaders) == 1 {
			apiDigest := sha256.Sum256([]byte(strings.TrimSpace(apiHeaders[0])))
			compared := subtle.ConstantTimeCompare(digest[:], apiDigest[:])
			consistent = compared == 1
			matched &= compared
		}
		if valid && key != "" && matched == 1 {
			c.Request.Header.Del("Authorization")
			c.Request.Header.Del("x-api-key")
			c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), authenticatedKey, true))
			c.Next()
			return
		}

		// A database gateway token authenticates to its user. A conflicting
		// Authorization/x-api-key pair never reaches this path.
		if valid && key != "" && consistent && db != nil && db.pool != nil {
			// Repeated requests with the same token reuse the cached identity for
			// at most TokenCacheTTL; revoke/expiry apply within that window.
			identity, cached := gatewayTokenCache.Lookup(key, auth.TokenKindGateway)
			if !cached {
				if validated, err := auth.ValidateAPIToken(c.Request.Context(), db.pool, key, auth.TokenKindGateway, time.Now()); err == nil {
					identity = *validated
					gatewayTokenCache.Store(key, auth.TokenKindGateway, identity)
					cached = true
				}
			}
			if cached {
				if identity.MustChangePassword {
					// Same code and envelope as the admin API: a minted token
					// must not route traffic before the password changes.
					adminAPIError(c, http.StatusForbidden, "password_change_required", "change the password before using the gateway")
					return
				}
				c.Request.Header.Del("Authorization")
				c.Request.Header.Del("x-api-key")
				if rc := core.RequestContextFrom(c.Request.Context()); rc != nil {
					rc.UserID = identity.UserID
				}
				c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), authenticatedKey, true))
				c.Next()
				return
			}
		}

		// File mode without static keys stays anonymous even when a stray
		// credential is present, exactly as before. Database auth must fail
		// closed: falling through would let a caller reach the gateway's own
		// providers while authenticated users are confined to database keys.
		if len(keys) == 0 && db == nil {
			c.Next()
			return
		}

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
	}
}
