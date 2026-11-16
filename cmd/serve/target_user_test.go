package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/minhhhduc/gnvipi/internal/auth"
)

// Both console surfaces that address "another account" share one rule: owner
// level may name any id, everyone else may only name themselves under pain of
// the cross-subject 404.
func TestTargetUserSelectionByRole(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const self, other = "u-self", "u-other"
	cases := []struct {
		role      auth.Role
		requested string
		wantID    string
		wantOK    bool
	}{
		{auth.RoleUser, "", self, true},
		{auth.RoleUser, other, "", false},
		{auth.RoleAdmin, other, "", false},
		{auth.RoleSuperuser, "", self, true},
		{auth.RoleSuperuser, other, other, true},
		{auth.RoleRoot, "", self, true},
		{auth.RoleRoot, other, other, true},
	}
	for _, tc := range cases {
		t.Run(string(tc.role)+"/"+tc.requested, func(t *testing.T) {
			identity := &adminIdentity{UserID: self, Role: tc.role}

			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/?user_id="+url.QueryEscape(tc.requested), http.NoBody)
			got, ok := playgroundTargetUser(c, identity)
			if ok != tc.wantOK || got != tc.wantID {
				t.Fatalf("playground = (%q, %v), want (%q, %v)", got, ok, tc.wantID, tc.wantOK)
			}
			if !tc.wantOK && rec.Code != http.StatusNotFound {
				t.Fatalf("playground status = %d, want 404", rec.Code)
			}

			rec = httptest.NewRecorder()
			c, _ = gin.CreateTestContext(rec)
			c.Request = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
			got, ok = routingTargetUser(c, identity, tc.requested)
			if ok != tc.wantOK || got != tc.wantID {
				t.Fatalf("routing = (%q, %v), want (%q, %v)", got, ok, tc.wantID, tc.wantOK)
			}
			if !tc.wantOK && rec.Code != http.StatusNotFound {
				t.Fatalf("routing status = %d, want 404", rec.Code)
			}

			rec = httptest.NewRecorder()
			c, _ = gin.CreateTestContext(rec)
			c.Request = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/?user_id="+url.QueryEscape(tc.requested), http.NoBody)
			got, ok = adminTokenTarget(c, identity)
			if ok != tc.wantOK || got != tc.wantID {
				t.Fatalf("token = (%q, %v), want (%q, %v)", got, ok, tc.wantID, tc.wantOK)
			}
			if !tc.wantOK && rec.Code != http.StatusNotFound {
				t.Fatalf("token status = %d, want 404", rec.Code)
			}
		})
	}
}

func TestSameUserID(t *testing.T) {
	const canonical = "11111111-2222-3333-4444-555555555555"
	const upper = "11111111-2222-3333-4444-555555555555"
	const other = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	cases := []struct {
		name string
		a, b string
		want bool
	}{
		{"identical", canonical, canonical, true},
		{"uppercase", strings.ToUpper(upper), canonical, true},
		{"dashless", strings.ReplaceAll(canonical, "-", ""), canonical, true},
		{"uppercase dashless", strings.ToUpper(strings.ReplaceAll(canonical, "-", "")), canonical, true},
		{"different uuids", canonical, other, false},
		{"non-uuid equal", "u-self", "u-self", true},
		{"non-uuid different", "u-self", "u-other", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameUserID(tc.a, tc.b); got != tc.want {
				t.Fatalf("sameUserID(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestTargetUserSelectionCanonicalUUIDVariants(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const self = "123e4567-e89b-12d3-a456-426614174000"
	const other = "999e4567-e89b-12d3-a456-426614174000"
	upper := strings.ToUpper(self)
	dashless := strings.ReplaceAll(self, "-", "")

	for _, role := range []auth.Role{auth.RoleUser, auth.RoleAdmin, auth.RoleSuperuser, auth.RoleRoot} {
		for _, requested := range []string{upper, dashless} {
			identity := &adminIdentity{UserID: self, Role: role}

			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/?user_id="+url.QueryEscape(requested), http.NoBody)
			if got, ok := playgroundTargetUser(c, identity); !ok || got != self {
				t.Fatalf("role=%s requested=%q playground = (%q, %v), want (%q, true)", role, requested, got, ok, self)
			}

			rec = httptest.NewRecorder()
			c, _ = gin.CreateTestContext(rec)
			c.Request = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
			if got, ok := routingTargetUser(c, identity, requested); !ok || got != self {
				t.Fatalf("role=%s requested=%q routing = (%q, %v), want (%q, true)", role, requested, got, ok, self)
			}

			rec = httptest.NewRecorder()
			c, _ = gin.CreateTestContext(rec)
			c.Request = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/?user_id="+url.QueryEscape(requested), http.NoBody)
			if got, ok := adminTokenTarget(c, identity); !ok || got != self {
				t.Fatalf("role=%s requested=%q token = (%q, %v), want (%q, true)", role, requested, got, ok, self)
			}
		}
	}

	// Non-superuser naming a foreign uppercase UUID must still receive 404.
	identity := &adminIdentity{UserID: self, Role: auth.RoleUser}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/?user_id="+url.QueryEscape(strings.ToUpper(other)), http.NoBody)
	if _, ok := playgroundTargetUser(c, identity); ok || rec.Code != http.StatusNotFound {
		t.Fatalf("foreign uppercase playground = (%v, %d), want (false, 404)", ok, rec.Code)
	}
}

func TestAdminPasswordChangeAllowedCanonicalUUID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const self = "123e4567-e89b-12d3-a456-426614174000"
	const other = "999e4567-e89b-12d3-a456-426614174000"
	identity := &adminIdentity{UserID: self, Role: auth.RoleUser}

	for _, id := range []string{self, strings.ToUpper(self), strings.ReplaceAll(self, "-", "")} {
		var allowed bool
		engine := gin.New()
		engine.POST("/admin/api/users/:id/password", func(c *gin.Context) {
			allowed = adminPasswordChangeAllowed(c, identity)
			c.Status(http.StatusNoContent)
		})
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/admin/api/users/"+id+"/password", http.NoBody)
		engine.ServeHTTP(httptest.NewRecorder(), req)
		if !allowed {
			t.Fatalf("adminPasswordChangeAllowed(%q) = false, want true", id)
		}
	}

	var allowed bool
	engine := gin.New()
	engine.POST("/admin/api/users/:id/password", func(c *gin.Context) {
		allowed = adminPasswordChangeAllowed(c, identity)
		c.Status(http.StatusNoContent)
	})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/admin/api/users/"+strings.ToUpper(other)+"/password", http.NoBody)
	engine.ServeHTTP(httptest.NewRecorder(), req)
	if allowed {
		t.Fatal("adminPasswordChangeAllowed(foreign) = true, want false")
	}
}

func TestHandleAdminSetUserPasswordRequiresCurrentPasswordForCanonicalSelf(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const self = "123e4567-e89b-12d3-a456-426614174000"
	identity := &adminIdentity{UserID: self, Role: auth.RoleSuperuser}

	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Set(adminIdentityKey, identity)
		c.Next()
	})
	// Nil pool guarantees that falling through to auth.GetUserByID / ResetPasswordBy would panic.
	engine.POST("/admin/api/users/:id/password", handleAdminSetUserPassword(&authState{}))

	for _, tc := range []struct {
		name string
		id   string
	}{
		{"uppercase", strings.ToUpper(self)},
		{"dashless", strings.ReplaceAll(self, "-", "")},
		{"uppercase dashless", strings.ToUpper(strings.ReplaceAll(self, "-", ""))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]string{"password": "Valid-New-Password9!"})
			if err != nil {
				t.Fatalf("marshal body: %v", err)
			}
			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/admin/api/users/"+tc.id+"/password", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			engine.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d %s, want 400", rec.Code, rec.Body.String())
			}
			var decoded map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if decoded["field"] != "current_password" {
				t.Fatalf("field = %v, want current_password (body %s)", decoded["field"], rec.Body.String())
			}
		})
	}
}
