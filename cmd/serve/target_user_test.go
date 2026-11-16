package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
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
		{auth.RoleOwner, "", self, true},
		{auth.RoleOwner, other, other, true},
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
		})
	}
}
