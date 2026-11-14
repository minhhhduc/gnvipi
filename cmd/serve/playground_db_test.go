package main

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/minhhhduc/gnvipi/internal/auth"
)

// TestPlaygroundAdminContractDB pins the authorization envelope on the real
// admin middleware: anonymous → 401, cookie writes need CSRF, and a non-owner
// may not select another account.
func TestPlaygroundAdminContractDB(t *testing.T) {
	h := newDBAdminHarness(t)
	registerAdminPlaygroundRoutes(h.engine, h.state, nil, setupTestPrefs(t))

	res := makeRequest(t, h.engine, http.MethodGet, "/admin/api/playground/models", nil, nil)
	if res.Code != http.StatusUnauthorized || decodeBody(t, res)["code"] != "authentication_required" {
		t.Fatalf("anonymous models = %d %s, want 401 authentication_required", res.Code, res.Body.String())
	}
	res = makeRequest(t, h.engine, http.MethodPost, "/admin/api/playground/chat", gin.H{"model": "m", "messages": []any{}}, nil)
	if res.Code != http.StatusUnauthorized || decodeBody(t, res)["code"] != "authentication_required" {
		t.Fatalf("anonymous chat = %d %s, want 401 authentication_required", res.Code, res.Body.String())
	}

	ownerSession, ownerCSRF := h.login(t, dbOwnerUsername, dbOwnerPassword)
	// The cookie pair without the double-submit header is rejected.
	res = makeRequest(t, h.engine, http.MethodPost, "/admin/api/playground/chat", gin.H{"model": "m", "messages": []any{}}, map[string]string{
		"Cookie": auth.SessionCookieName + "=" + ownerSession + "; " + auth.CSRFCookieName + "=" + ownerCSRF,
	})
	if res.Code != http.StatusForbidden || decodeBody(t, res)["code"] != "csrf_failed" {
		t.Fatalf("missing CSRF = %d %s, want 403 csrf_failed", res.Code, res.Body.String())
	}
	res = makeRequest(t, h.engine, http.MethodPost, "/admin/api/playground/chat", gin.H{"model": "m", "messages": []any{}}, map[string]string{
		"Cookie":            auth.SessionCookieName + "=" + ownerSession + "; " + auth.CSRFCookieName + "=" + ownerCSRF,
		auth.CSRFHeaderName: "not-the-cookie-value",
	})
	if res.Code != http.StatusForbidden || decodeBody(t, res)["code"] != "csrf_failed" {
		t.Fatalf("invalid CSRF = %d %s, want 403 csrf_failed", res.Code, res.Body.String())
	}

	member, err := auth.CreateUser(t.Context(), h.pool, auth.CreateUserInput{
		Username: "playground-member", Email: "playground-member@example.test", DisplayName: "Playground Member",
		Role: auth.RoleUser, PasswordHash: h.owner.PasswordHash,
	})
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	session, csrf := h.login(t, "playground-member", dbUserPassword)
	res = makeRequest(t, h.engine, http.MethodGet, "/admin/api/playground/models?user_id="+h.owner.ID, nil, cookieAuth(session, csrf))
	if res.Code != http.StatusNotFound {
		t.Fatalf("foreign user_id models = %d %s, want 404", res.Code, res.Body.String())
	}
	res = makeRequest(t, h.engine, http.MethodPost, "/admin/api/playground/chat?user_id="+h.owner.ID, gin.H{"model": "m", "messages": []any{}}, cookieAuth(session, csrf))
	if res.Code != http.StatusNotFound {
		t.Fatalf("foreign user_id chat = %d %s, want 404", res.Code, res.Body.String())
	}
	// A non-owner may still name itself.
	res = makeRequest(t, h.engine, http.MethodGet, "/admin/api/playground/models?user_id="+member.ID, nil, cookieAuth(session, csrf))
	if res.Code == http.StatusNotFound {
		t.Fatal("a user was treated as foreign to its own id")
	}
}
