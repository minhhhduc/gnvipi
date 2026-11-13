package main

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/auth"
)

func responseItems(t *testing.T, resBody map[string]any, key string) []any {
	t.Helper()
	items, ok := resBody[key].([]any)
	if !ok {
		t.Fatalf("%s = %T, want array (body %v)", key, resBody[key], resBody)
	}
	return items
}

func responseStringIDs(t *testing.T, items []any) []string {
	t.Helper()
	ids := make([]string, 0, len(items))
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("item = %T, want object", raw)
		}
		id, ok := item["id"].(string)
		if !ok || id == "" {
			t.Fatalf("item id = %v, want non-empty string", item["id"])
		}
		ids = append(ids, id)
	}
	return ids
}

func responseAuditIDs(t *testing.T, items []any) []int64 {
	t.Helper()
	ids := make([]int64, 0, len(items))
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("audit item = %T, want object", raw)
		}
		id, ok := item["id"].(float64)
		if !ok {
			t.Fatalf("audit id = %v, want number", item["id"])
		}
		ids = append(ids, int64(id))
	}
	return ids
}

func createContractUser(t *testing.T, h *dbAdminHarness, username string, role auth.Role) string {
	t.Helper()
	res := makeRequest(t, h.engine, http.MethodPost, "/admin/api/users", gin.H{
		"username": username,
		"role":     string(role),
		"password": dbUserPassword,
	}, bearer(h.token))
	if res.Code != http.StatusCreated {
		t.Fatalf("create %s = %d, want 201 (body %s)", username, res.Code, res.Body.String())
	}
	id, _ := decodeBody(t, res)["id"].(string)
	if id == "" {
		t.Fatalf("create %s returned no id", username)
	}
	return id
}

func clearMustChangePassword(t *testing.T, h *dbAdminHarness, userID string) {
	t.Helper()
	res := makeRequest(t, h.engine, http.MethodPut, "/admin/api/users/"+userID, gin.H{
		"must_change_password": false,
	}, bearer(h.token))
	if res.Code != http.StatusOK {
		t.Fatalf("clear must_change_password for %s = %d, want 200 (body %s)", userID, res.Code, res.Body.String())
	}
}

func createContractAdminToken(t *testing.T, h *dbAdminHarness, userID string, headers map[string]string) (raw, id string) {
	t.Helper()
	body := gin.H{
		"name":       "contract admin token",
		"kind":       string(auth.TokenKindAdmin),
		"scope":      string(auth.ScopeWrite),
		"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}
	if userID != "" {
		body["user_id"] = userID
	}
	res := makeRequest(t, h.engine, http.MethodPost, "/admin/api/tokens", body, headers)
	if res.Code != http.StatusCreated {
		t.Fatalf("create admin token = %d, want 201 (body %s)", res.Code, res.Body.String())
	}
	response := decodeBody(t, res)
	raw, _ = response["token"].(string)
	id, _ = response["id"].(string)
	if raw == "" || id == "" {
		t.Fatalf("admin token response omitted token or id: %v", response)
	}
	return raw, id
}

func TestDBAdminIdentity_UserCRUDRoleMatrix(t *testing.T) {
	h := newDBAdminHarness(t)

	adminID := createContractUser(t, h, "contract-admin", auth.RoleAdmin)
	clearMustChangePassword(t, h, adminID)
	adminRaw, _ := createContractAdminToken(t, h, adminID, bearer(h.token))

	res := makeRequest(t, h.engine, http.MethodGet, "/admin/api/users/"+adminID, nil, bearer(adminRaw))
	if res.Code != http.StatusOK {
		t.Fatalf("admin read self = %d, want 200 (body %s)", res.Code, res.Body.String())
	}
	for _, mutation := range []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodPost, "/admin/api/users", gin.H{"username": "admin-created", "password": dbUserPassword}},
		{http.MethodPut, "/admin/api/users/" + adminID, gin.H{"display_name": "not allowed"}},
		{http.MethodDelete, "/admin/api/users/" + adminID, nil},
	} {
		res = makeRequest(t, h.engine, mutation.method, mutation.path, mutation.body, bearer(adminRaw))
		if res.Code != http.StatusForbidden {
			t.Fatalf("admin %s %s = %d, want 403 (body %s)", mutation.method, mutation.path, res.Code, res.Body.String())
		}
	}

	userID := createContractUser(t, h, "contract-user", auth.RoleUser)
	session, csrf := h.login(t, "contract-user", dbUserPassword)
	userPassword := "Saffron-Comet8-Harbor!"
	res = makeRequest(t, h.engine, http.MethodPost, "/admin/api/users/"+userID+"/password", gin.H{
		"current_password": dbUserPassword,
		"password":         userPassword,
	}, cookieAuth(session, csrf))
	if res.Code != http.StatusNoContent {
		t.Fatalf("user change own password = %d, want 204 (body %s)", res.Code, res.Body.String())
	}
	res = makeRequest(t, h.engine, http.MethodGet, "/admin/api/users/"+userID, nil, cookieAuth(session, csrf))
	if res.Code != http.StatusOK {
		t.Fatalf("user read self = %d, want 200 (body %s)", res.Code, res.Body.String())
	}
	res = makeRequest(t, h.engine, http.MethodGet, "/admin/api/users/"+h.owner.ID, nil, cookieAuth(session, csrf))
	if res.Code != http.StatusNotFound {
		t.Fatalf("user read owner = %d, want 404 (body %s)", res.Code, res.Body.String())
	}
	for _, mutation := range []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodPost, "/admin/api/users", gin.H{"username": "user-created", "password": dbUserPassword}},
		{http.MethodPut, "/admin/api/users/" + userID, gin.H{"display_name": "not allowed"}},
		{http.MethodDelete, "/admin/api/users/" + userID, nil},
	} {
		res = makeRequest(t, h.engine, mutation.method, mutation.path, mutation.body, cookieAuth(session, csrf))
		if res.Code != http.StatusForbidden {
			t.Fatalf("user %s %s = %d, want 403 (body %s)", mutation.method, mutation.path, res.Code, res.Body.String())
		}
	}

	res = makeRequest(t, h.engine, http.MethodPut, "/admin/api/users/"+userID, gin.H{
		"display_name": "Owner Updated",
	}, bearer(h.token))
	if res.Code != http.StatusOK || decodeBody(t, res)["display_name"] != "Owner Updated" {
		t.Fatalf("owner update user = %d %s, want updated display name", res.Code, res.Body.String())
	}
	doomedID := createContractUser(t, h, "contract-doomed", auth.RoleUser)
	res = makeRequest(t, h.engine, http.MethodDelete, "/admin/api/users/"+doomedID, nil, bearer(h.token))
	if res.Code != http.StatusNoContent {
		t.Fatalf("owner delete user = %d, want 204 (body %s)", res.Code, res.Body.String())
	}
}

func TestDBAdminIdentity_TokenCRUDRoleMatrix(t *testing.T) {
	h := newDBAdminHarness(t)

	adminID := createContractUser(t, h, "token-admin", auth.RoleAdmin)
	clearMustChangePassword(t, h, adminID)
	adminRaw, _ := createContractAdminToken(t, h, adminID, bearer(h.token))

	res := makeRequest(t, h.engine, http.MethodPost, "/admin/api/tokens", gin.H{
		"name":  "admin gateway",
		"kind":  string(auth.TokenKindGateway),
		"scope": "",
	}, bearer(adminRaw))
	if res.Code != http.StatusCreated {
		t.Fatalf("admin create own gateway token = %d, want 201 (body %s)", res.Code, res.Body.String())
	}
	adminGatewayID, _ := decodeBody(t, res)["id"].(string)

	userID := createContractUser(t, h, "token-user-api", auth.RoleUser)
	session, csrf := h.login(t, "token-user-api", dbUserPassword)
	res = makeRequest(t, h.engine, http.MethodPost, "/admin/api/users/"+userID+"/password", gin.H{
		"current_password": dbUserPassword,
		"password":         "Topaz-River8-Cinder!",
	}, cookieAuth(session, csrf))
	if res.Code != http.StatusNoContent {
		t.Fatalf("prepare user session = %d, want 204 (body %s)", res.Code, res.Body.String())
	}

	res = makeRequest(t, h.engine, http.MethodPost, "/admin/api/tokens", gin.H{
		"name":      "user gateway",
		"kind":      string(auth.TokenKindGateway),
		"no_expiry": true,
	}, cookieAuth(session, csrf))
	if res.Code != http.StatusCreated {
		t.Fatalf("user create gateway token = %d, want 201 (body %s)", res.Code, res.Body.String())
	}
	userTokenID, _ := decodeBody(t, res)["id"].(string)

	res = makeRequest(t, h.engine, http.MethodPost, "/admin/api/tokens", gin.H{
		"name":  "forbidden admin",
		"kind":  string(auth.TokenKindAdmin),
		"scope": string(auth.ScopeRead),
	}, cookieAuth(session, csrf))
	if res.Code != http.StatusForbidden {
		t.Fatalf("user create admin token = %d, want 403 (body %s)", res.Code, res.Body.String())
	}
	res = makeRequest(t, h.engine, http.MethodPost, "/admin/api/tokens", gin.H{
		"name":    "cross-user",
		"kind":    string(auth.TokenKindGateway),
		"user_id": h.owner.ID,
	}, cookieAuth(session, csrf))
	if res.Code != http.StatusNotFound {
		t.Fatalf("user create cross-user token = %d, want 404 (body %s)", res.Code, res.Body.String())
	}
	res = makeRequest(t, h.engine, http.MethodDelete, "/admin/api/tokens/"+adminGatewayID, nil, cookieAuth(session, csrf))
	if res.Code != http.StatusNotFound {
		t.Fatalf("user revoke admin token = %d, want 404 (body %s)", res.Code, res.Body.String())
	}
	res = makeRequest(t, h.engine, http.MethodDelete, "/admin/api/tokens/"+userTokenID, nil, cookieAuth(session, csrf))
	if res.Code != http.StatusNoContent {
		t.Fatalf("user revoke own token = %d, want 204 (body %s)", res.Code, res.Body.String())
	}

	res = makeRequest(t, h.engine, http.MethodGet, "/admin/api/tokens?user_id="+userID, nil, bearer(adminRaw))
	if res.Code != http.StatusNotFound {
		t.Fatalf("admin list cross-user tokens = %d, want 404 (body %s)", res.Code, res.Body.String())
	}
	res = makeRequest(t, h.engine, http.MethodPost, "/admin/api/tokens", gin.H{
		"name":    "owner managed",
		"kind":    string(auth.TokenKindGateway),
		"user_id": userID,
	}, bearer(h.token))
	if res.Code != http.StatusCreated {
		t.Fatalf("owner create user token = %d, want 201 (body %s)", res.Code, res.Body.String())
	}
	ownerManagedID, _ := decodeBody(t, res)["id"].(string)
	res = makeRequest(t, h.engine, http.MethodDelete, "/admin/api/tokens/"+ownerManagedID, nil, bearer(h.token))
	if res.Code != http.StatusNoContent {
		t.Fatalf("owner revoke user token = %d, want 204 (body %s)", res.Code, res.Body.String())
	}
}

func TestDBAdminIdentity_GatewayTokenExpiryAndLastUsed(t *testing.T) {
	h := newDBAdminHarness(t)
	before := time.Now()
	res := makeRequest(t, h.engine, http.MethodPost, "/admin/api/tokens", gin.H{
		"name": "default gateway",
		"kind": string(auth.TokenKindGateway),
	}, bearer(h.token))
	if res.Code != http.StatusCreated {
		t.Fatalf("create default gateway token = %d, want 201 (body %s)", res.Code, res.Body.String())
	}
	created := decodeBody(t, res)
	defaultID, _ := created["id"].(string)
	defaultRaw, _ := created["token"].(string)
	expiresRaw, _ := created["expires_at"].(string)
	expiresAt, err := time.Parse(time.RFC3339Nano, expiresRaw)
	if err != nil {
		t.Fatalf("parse default expiry %q: %v", expiresRaw, err)
	}
	if expiresAt.Before(before.Add(auth.GatewayTokenDefaultExpiry-time.Minute)) || expiresAt.After(time.Now().Add(auth.GatewayTokenDefaultExpiry+time.Minute)) {
		t.Fatalf("default gateway expiry = %s, want about 365 days", expiresAt)
	}

	res = makeRequest(t, h.engine, http.MethodPost, "/admin/api/tokens", gin.H{
		"name":      "no expiry gateway",
		"kind":      string(auth.TokenKindGateway),
		"no_expiry": true,
	}, bearer(h.token))
	if res.Code != http.StatusCreated {
		t.Fatalf("create no-expiry gateway token = %d, want 201 (body %s)", res.Code, res.Body.String())
	}
	if decodeBody(t, res)["expires_at"] != nil {
		t.Fatalf("no-expiry gateway token returned expires_at: %s", res.Body.String())
	}

	if _, err := auth.ValidateAPIToken(t.Context(), h.pool, defaultRaw, auth.TokenKindGateway, time.Now()); err != nil {
		t.Fatalf("use gateway token: %v", err)
	}
	res = makeRequest(t, h.engine, http.MethodGet, "/admin/api/tokens?kind=gateway", nil, bearer(h.token))
	if res.Code != http.StatusOK {
		t.Fatalf("list gateway tokens = %d, want 200 (body %s)", res.Code, res.Body.String())
	}
	found := false
	for _, raw := range responseItems(t, decodeBody(t, res), "tokens") {
		item := raw.(map[string]any)
		if item["id"] != defaultID {
			continue
		}
		found = true
		if item["last_used_at"] == nil {
			t.Fatalf("used gateway token omitted last_used_at: %v", item)
		}
		if _, leaked := item["token"]; leaked {
			t.Fatalf("token list leaked plaintext token: %v", item)
		}
	}
	if !found {
		t.Fatalf("gateway token %s missing from list", defaultID)
	}
}

func TestDBAdminIdentity_AuditCursorPagination(t *testing.T) {
	h := newDBAdminHarness(t)
	if _, err := h.pool.Exec(t.Context(), `TRUNCATE audit_log RESTART IDENTITY`); err != nil {
		t.Fatalf("reset audit log: %v", err)
	}
	ts := time.Date(2026, 11, 14, 0, 0, 0, 123, time.UTC)
	for i := 1; i <= 5; i++ {
		if _, err := h.pool.Exec(t.Context(), `
			INSERT INTO audit_log (ts, actor_type, action, target_type, target_id)
			VALUES ($1, 'system', 'contract.page', 'test', $2)`, ts, strconv.Itoa(i)); err != nil {
			t.Fatalf("insert audit row %d: %v", i, err)
		}
	}

	res := makeRequest(t, h.engine, http.MethodGet, "/admin/api/audit?limit=2", nil, bearer(h.token))
	if res.Code != http.StatusOK {
		t.Fatalf("audit first page = %d, want 200 (body %s)", res.Code, res.Body.String())
	}
	first := decodeBody(t, res)
	firstIDs := responseAuditIDs(t, responseItems(t, first, "entries"))
	if len(firstIDs) != 2 || firstIDs[0] != 5 || firstIDs[1] != 4 || first["has_more"] != true {
		t.Fatalf("audit first page = ids %v has_more %v, want [5 4] true", firstIDs, first["has_more"])
	}
	next, _ := first["next_cursor"].(string)
	before, _ := first["before_cursor"].(string)
	if next == "" || before == "" {
		t.Fatalf("audit first page omitted cursors: %v", first)
	}

	res = makeRequest(t, h.engine, http.MethodGet, "/admin/api/audit?limit=2&after="+next, nil, bearer(h.token))
	if res.Code != http.StatusOK {
		t.Fatalf("audit second page = %d, want 200 (body %s)", res.Code, res.Body.String())
	}
	second := decodeBody(t, res)
	secondIDs := responseAuditIDs(t, responseItems(t, second, "entries"))
	if len(secondIDs) != 2 || secondIDs[0] != 3 || secondIDs[1] != 2 || second["has_more"] != true {
		t.Fatalf("audit second page = ids %v has_more %v, want [3 2] true", secondIDs, second["has_more"])
	}

	secondBefore, _ := second["before_cursor"].(string)
	res = makeRequest(t, h.engine, http.MethodGet, "/admin/api/audit?limit=2&before="+secondBefore, nil, bearer(h.token))
	if res.Code != http.StatusOK {
		t.Fatalf("audit previous page = %d, want 200 (body %s)", res.Code, res.Body.String())
	}
	previousIDs := responseAuditIDs(t, responseItems(t, decodeBody(t, res), "entries"))
	if len(previousIDs) != 2 || previousIDs[0] != firstIDs[0] || previousIDs[1] != firstIDs[1] {
		t.Fatalf("audit previous page ids = %v, want %v", previousIDs, firstIDs)
	}

	secondNext, _ := second["next_cursor"].(string)
	res = makeRequest(t, h.engine, http.MethodGet, "/admin/api/audit?limit=2&after="+secondNext, nil, bearer(h.token))
	last := decodeBody(t, res)
	lastIDs := responseAuditIDs(t, responseItems(t, last, "entries"))
	if res.Code != http.StatusOK || len(lastIDs) != 1 || lastIDs[0] != 1 || last["has_more"] != false {
		t.Fatalf("audit last page = %d ids %v has_more %v, want 200 [1] false", res.Code, lastIDs, last["has_more"])
	}

	res = makeRequest(t, h.engine, http.MethodGet, "/admin/api/audit?after=not-a-cursor", nil, bearer(h.token))
	if res.Code != http.StatusBadRequest || decodeBody(t, res)["field"] != "after" {
		t.Fatalf("invalid audit cursor = %d %s, want 400 field after", res.Code, res.Body.String())
	}
	res = makeRequest(t, h.engine, http.MethodGet, "/admin/api/audit?after="+next+"&before="+before, nil, bearer(h.token))
	if res.Code != http.StatusBadRequest || decodeBody(t, res)["field"] != "after" {
		t.Fatalf("dual audit cursors = %d %s, want 400 field after", res.Code, res.Body.String())
	}
}

func TestDBAdminIdentity_UserAndTokenPagination(t *testing.T) {
	h := newDBAdminHarness(t)
	for i := 1; i <= 4; i++ {
		createContractUser(t, h, "page-user-"+strconv.Itoa(i), auth.RoleUser)
	}

	res := makeRequest(t, h.engine, http.MethodGet, "/admin/api/users?limit=2&offset=0", nil, bearer(h.token))
	if res.Code != http.StatusOK {
		t.Fatalf("users first page = %d, want 200 (body %s)", res.Code, res.Body.String())
	}
	firstUsers := decodeBody(t, res)
	firstUserIDs := responseStringIDs(t, responseItems(t, firstUsers, "users"))
	if len(firstUserIDs) != 2 || firstUsers["limit"] != float64(2) || firstUsers["offset"] != float64(0) || firstUsers["total"] != float64(5) {
		t.Fatalf("users first page = %v, want 2/0 total 5", firstUsers)
	}
	res = makeRequest(t, h.engine, http.MethodGet, "/admin/api/users?limit=2&offset=2", nil, bearer(h.token))
	secondUserIDs := responseStringIDs(t, responseItems(t, decodeBody(t, res), "users"))
	if res.Code != http.StatusOK || len(secondUserIDs) != 2 || firstUserIDs[0] == secondUserIDs[0] || firstUserIDs[1] == secondUserIDs[1] {
		t.Fatalf("users second page = %d ids %v; first ids %v", res.Code, secondUserIDs, firstUserIDs)
	}
	res = makeRequest(t, h.engine, http.MethodGet, "/admin/api/users?limit=999", nil, bearer(h.token))
	if res.Code != http.StatusOK || decodeBody(t, res)["limit"] != float64(200) {
		t.Fatalf("users max limit = %d %s, want limit 200", res.Code, res.Body.String())
	}
	res = makeRequest(t, h.engine, http.MethodGet, "/admin/api/users?offset=-1", nil, bearer(h.token))
	if res.Code != http.StatusBadRequest || decodeBody(t, res)["field"] != "offset" {
		t.Fatalf("users negative offset = %d %s, want 400 field offset", res.Code, res.Body.String())
	}

	for i := 1; i <= 3; i++ {
		res = makeRequest(t, h.engine, http.MethodPost, "/admin/api/tokens", gin.H{
			"name": "page token " + strconv.Itoa(i),
			"kind": string(auth.TokenKindGateway),
		}, bearer(h.token))
		if res.Code != http.StatusCreated {
			t.Fatalf("create page token %d = %d, want 201 (body %s)", i, res.Code, res.Body.String())
		}
	}
	res = makeRequest(t, h.engine, http.MethodGet, "/admin/api/tokens?kind=gateway&limit=2&offset=0", nil, bearer(h.token))
	firstTokens := decodeBody(t, res)
	firstTokenIDs := responseStringIDs(t, responseItems(t, firstTokens, "tokens"))
	if res.Code != http.StatusOK || len(firstTokenIDs) != 2 || firstTokens["limit"] != float64(2) || firstTokens["offset"] != float64(0) {
		t.Fatalf("tokens first page = %d %v, want 2/0", res.Code, firstTokens)
	}
	res = makeRequest(t, h.engine, http.MethodGet, "/admin/api/tokens?kind=gateway&limit=2&offset=2", nil, bearer(h.token))
	secondTokenIDs := responseStringIDs(t, responseItems(t, decodeBody(t, res), "tokens"))
	if res.Code != http.StatusOK || len(secondTokenIDs) != 1 || secondTokenIDs[0] == firstTokenIDs[0] || secondTokenIDs[0] == firstTokenIDs[1] {
		t.Fatalf("tokens second page = %d ids %v; first ids %v", res.Code, secondTokenIDs, firstTokenIDs)
	}
	res = makeRequest(t, h.engine, http.MethodGet, "/admin/api/tokens?limit=0", nil, bearer(h.token))
	if res.Code != http.StatusBadRequest || decodeBody(t, res)["field"] != "limit" {
		t.Fatalf("tokens zero limit = %d %s, want 400 field limit", res.Code, res.Body.String())
	}
}
