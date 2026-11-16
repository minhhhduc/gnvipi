package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/minhhhduc/gnvipi/internal/auth"
	"github.com/minhhhduc/gnvipi/internal/models"
)

func restoreModels(t *testing.T) {
	t.Helper()
	saved := models.Snapshot()
	t.Cleanup(func() { models.Replace(saved) })
}

func TestDBAdminNvidiaRescanRoleMatrix(t *testing.T) {
	restoreModels(t)
	stubNvidiaDiscovery(t, rescanFixture(
		models.PlaygroundRow{Model: "acme/rescan-matrix", Slug: "rescan-matrix", Namespace: "qc69jvmznzxy", FunctionID: "66666666-6666-6666-6666-666666666666"},
	))
	h := newDBAdminHarness(t)

	adminID := createContractUser(t, h, "rescan-admin", auth.RoleAdmin)
	clearMustChangePassword(t, h, adminID)
	adminToken, _ := createContractAdminToken(t, h, adminID, bearer(h.token))
	userID := createContractUser(t, h, "rescan-user", auth.RoleUser)
	clearMustChangePassword(t, h, userID)
	userSession, userCSRF := h.login(t, "rescan-user", dbUserPassword)

	cases := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"superuser", bearer(h.token), http.StatusOK},
		{"admin", bearer(adminToken), http.StatusOK},
		{"user", cookieAuth(userSession, userCSRF), http.StatusForbidden},
		{"anonymous", nil, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := makeRequest(t, h.engine, http.MethodPost, "/admin/api/nvidia/models/rescan", nil, tc.headers)
			if res.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", res.Code, tc.want, res.Body.String())
			}
		})
	}
}

func TestDBAdminNvidiaRescanAuditCountsOnly(t *testing.T) {
	const canary = "captcha-canary-4d8e"
	restoreModels(t)
	// Start from an empty catalog so the counts under test do not depend on
	// whatever the checkout's model file happens to contain in CI.
	models.Replace(map[string]models.ModelInfo{})
	stubNvidiaDiscovery(t, rescanFixture(
		models.PlaygroundRow{Model: "acme/" + canary, Slug: canary, Namespace: "qc69jvmznzxy", FunctionID: "77777777-7777-7777-7777-777777777777"},
	))
	h := newDBAdminHarness(t)

	res := makeRequest(t, h.engine, http.MethodPost, "/admin/api/nvidia/models/rescan", nil, bearer(h.token))
	if res.Code != http.StatusOK {
		t.Fatalf("rescan = %d: %s", res.Code, res.Body.String())
	}
	if strings.Contains(res.Body.String(), canary) {
		t.Fatalf("response leaked the model token: %s", res.Body.String())
	}

	var count int
	if err := h.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM audit_log WHERE action = 'nvidia.catalog.rescan'`).Scan(&count); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("audit rows = %d, want 1", count)
	}
	var actorType, detailsRaw string
	if err := h.pool.QueryRow(t.Context(),
		`SELECT actor_type, details::text FROM audit_log WHERE action = 'nvidia.catalog.rescan'`).
		Scan(&actorType, &detailsRaw); err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	if actorType != "token" {
		t.Fatalf("actor_type = %q, want token", actorType)
	}
	if strings.Contains(detailsRaw, canary) {
		t.Fatalf("audit details leaked the model token: %s", detailsRaw)
	}
	var details map[string]any
	if err := json.Unmarshal([]byte(detailsRaw), &details); err != nil {
		t.Fatalf("decode audit details: %v", err)
	}
	if details["added"] != float64(1) || details["updated"] != float64(0) || details["removed"] != float64(0) {
		t.Fatalf("audit details = %v", details)
	}
}
