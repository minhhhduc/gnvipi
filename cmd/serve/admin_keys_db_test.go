package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/minhhhduc/gnvipi/internal/auth"
	"github.com/minhhhduc/gnvipi/internal/providerstore"
)

const (
	keyTestSecret      = "sk-issue203-secret-abcd1234"
	keyBasicSecret     = `{"username":"issue203","password":"hunter2-wxyz"}`
	keyTestProviderID  = "issue203-provider"
	keyTestProviderURL = "https://api.issue203.test"
	keyCSRFPassword    = "Topaz-River8-Cinder!"
)

func newAdminKeyring(t *testing.T) *providerstore.Keyring {
	t.Helper()
	ring, err := providerstore.NewKeyring(1, bytes.Repeat([]byte{0x2b}, providerstore.MasterKeySize), nil)
	if err != nil {
		t.Fatalf("new keyring: %v", err)
	}
	return ring
}

func insertTestProvider(t *testing.T, h *dbAdminHarness, baseURL string) {
	t.Helper()
	if _, err := h.pool.Exec(t.Context(), `
		INSERT INTO providers (id, name, protocol, base_url)
		VALUES ($1, $1, 'openai-chat', $2)`, keyTestProviderID, baseURL); err != nil {
		t.Fatalf("insert provider %s: %v", keyTestProviderID, err)
	}
}

func keysPath(keyID string) string {
	path := "/admin/api/providers/" + keyTestProviderID + "/keys"
	if keyID != "" {
		return path + "/" + keyID
	}
	return path
}

func createTestKey(t *testing.T, h *dbAdminHarness, headers map[string]string, body gin.H) map[string]any {
	t.Helper()
	res := makeRequest(t, h.engine, http.MethodPost, keysPath(""), body, headers)
	if res.Code != http.StatusCreated {
		t.Fatalf("create key = %d, want 201 (body %s)", res.Code, res.Body.String())
	}
	created := decodeBody(t, res)
	if id, _ := created["id"].(string); id == "" {
		t.Fatalf("create key omitted id: %v", created)
	}
	assertNoKeySecretMaterial(t, created)
	return created
}

// assertNoKeySecretMaterial fails when a payload carries ciphertext, nonce, key
// version, secret-shaped fields or any known plaintext secret.
func assertNoKeySecretMaterial(t *testing.T, value any, secrets ...string) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	for _, secret := range append([]string{keyTestSecret, "hunter2-wxyz"}, secrets...) {
		if secret != "" && bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("payload exposed the plaintext secret %q: %s", secret, raw)
		}
	}
	var walk func(any)
	walk = func(value any) {
		switch value := value.(type) {
		case map[string]any:
			for key, nested := range value {
				switch strings.ToLower(key) {
				case "secret", "secret_enc", "secret_nonce", "secret_kver":
					t.Fatalf("payload exposed %q: %#v", key, value)
				}
				walk(nested)
			}
		case []any:
			for _, nested := range value {
				walk(nested)
			}
		}
	}
	walk(value)
}

func fieldErrorFields(body map[string]any) []string {
	raw, _ := body["field_errors"].([]any)
	fields := make([]string, 0, len(raw))
	for _, item := range raw {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if field, ok := entry["field"].(string); ok {
			fields = append(fields, field)
		}
	}
	return fields
}

func assertFieldError(t *testing.T, body map[string]any, want string) {
	t.Helper()
	for _, field := range fieldErrorFields(body) {
		if field == want {
			return
		}
	}
	t.Fatalf("field_errors = %v, want %q (body %v)", fieldErrorFields(body), want, body)
}

func storedKeySecret(t *testing.T, h *dbAdminHarness, keyID string) (enc, nonce []byte, last4 string) {
	t.Helper()
	if err := h.pool.QueryRow(t.Context(), `
		SELECT secret_enc, secret_nonce, COALESCE(secret_last4, '')
		FROM provider_keys WHERE id = $1::uuid`, keyID).Scan(&enc, &nonce, &last4); err != nil {
		t.Fatalf("read stored key secret: %v", err)
	}
	return enc, nonce, last4
}

// keyActor is one authenticated caller with the headers it uses.
type keyActor struct {
	userID  string
	headers map[string]string
}

func prepareKeyActor(t *testing.T, h *dbAdminHarness, username string, role auth.Role) keyActor {
	t.Helper()
	userID := createContractUser(t, h, username, role)
	clearMustChangePassword(t, h, userID)
	if role == auth.RoleUser {
		session, csrf := h.login(t, username, dbUserPassword)
		res := makeRequest(t, h.engine, http.MethodPost, "/admin/api/users/"+userID+"/password", gin.H{
			"current_password": dbUserPassword,
			"password":         keyCSRFPassword,
		}, cookieAuth(session, csrf))
		if res.Code != http.StatusNoContent {
			t.Fatalf("prepare session for %s = %d (body %s)", username, res.Code, res.Body.String())
		}
		return keyActor{userID: userID, headers: cookieAuth(session, csrf)}
	}
	raw, _ := createContractAdminToken(t, h, userID, bearer(h.token))
	return keyActor{userID: userID, headers: bearer(raw)}
}

func TestDBAdminKeys_RoleMatrix(t *testing.T) {
	h := newDBAdminHarness(t)
	insertTestProvider(t, h, keyTestProviderURL)
	admin := prepareKeyActor(t, h, "key-admin", auth.RoleAdmin)
	user := prepareKeyActor(t, h, "key-user", auth.RoleUser)

	userKey := createTestKey(t, h, user.headers, gin.H{"name": "user key", "secret": keyTestSecret})
	if userKey["owner_id"] != user.userID || userKey["has_secret"] != true {
		t.Fatalf("user key = %v, want owner %s with a stored secret", userKey, user.userID)
	}

	otherID := createContractUser(t, h, "key-other", auth.RoleUser)
	res := makeRequest(t, h.engine, http.MethodPost, keysPath(""), gin.H{
		"name": "cross owner", "secret": keyTestSecret, "owner_id": otherID,
	}, user.headers)
	if res.Code != http.StatusForbidden {
		t.Fatalf("user create key for another owner = %d, want 403 (body %s)", res.Code, res.Body.String())
	}
	res = makeRequest(t, h.engine, http.MethodPost, keysPath(""), gin.H{
		"name": "shared", "auth_scheme": "none", "owner_id": nil,
	}, user.headers)
	if res.Code != http.StatusForbidden {
		t.Fatalf("user create shared key = %d, want 403 (body %s)", res.Code, res.Body.String())
	}

	res = makeRequest(t, h.engine, http.MethodGet, keysPath(""), nil, user.headers)
	if res.Code != http.StatusOK {
		t.Fatalf("user list keys = %d, want 200 (body %s)", res.Code, res.Body.String())
	}
	listed := decodeBody(t, res)
	if items, _ := listed["keys"].([]any); len(items) != 1 {
		t.Fatalf("user list = %v, want only the caller's key", listed)
	}
	assertNoKeySecretMaterial(t, listed)

	ownerKey := createTestKey(t, h, bearer(h.token), gin.H{
		"name": "owner managed", "secret": keyTestSecret, "owner_id": user.userID,
	})
	ownerKeyID, _ := ownerKey["id"].(string)
	// The key the owner created for this user belongs to the user, so the user
	// reads it normally. A key owned by the admin stays invisible.
	res = makeRequest(t, h.engine, http.MethodGet, keysPath(ownerKeyID), nil, user.headers)
	if res.Code != http.StatusOK || decodeBody(t, res)["owner_id"] != user.userID {
		t.Fatalf("user read own key = %d %s, want 200", res.Code, res.Body.String())
	}

	adminOwn := createTestKey(t, h, admin.headers, gin.H{"name": "admin private", "secret": keyTestSecret})
	adminOwnID, _ := adminOwn["id"].(string)
	if adminOwn["owner_id"] != admin.userID {
		t.Fatalf("admin own key owner = %v, want %s", adminOwn["owner_id"], admin.userID)
	}
	for _, call := range []struct {
		method string
		body   any
	}{
		{http.MethodGet, nil},
		{http.MethodPut, gin.H{"name": "hijacked"}},
		{http.MethodDelete, nil},
	} {
		res = makeRequest(t, h.engine, call.method, keysPath(adminOwnID), call.body, user.headers)
		if res.Code != http.StatusNotFound {
			t.Fatalf("user %s foreign key = %d, want 404 (body %s)", call.method, res.Code, res.Body.String())
		}
	}
	res = makeRequest(t, h.engine, http.MethodGet, keysPath(""), nil, user.headers)
	if items, _ := decodeBody(t, res)["keys"].([]any); len(items) != 2 {
		t.Fatalf("user list after owner assignment = %v, want the two keys the user owns", items)
	}

	res = makeRequest(t, h.engine, http.MethodGet, keysPath(ownerKeyID), nil, admin.headers)
	if res.Code != http.StatusOK {
		t.Fatalf("admin read user key = %d, want 200 (body %s)", res.Code, res.Body.String())
	}
	assertNoKeySecretMaterial(t, decodeBody(t, res))
	res = makeRequest(t, h.engine, http.MethodPut, keysPath(ownerKeyID), gin.H{"enabled": false}, admin.headers)
	if res.Code != http.StatusOK || decodeBody(t, res)["enabled"] != false {
		t.Fatalf("admin disable user key = %d %s, want 200 disabled", res.Code, res.Body.String())
	}
	for _, body := range []gin.H{
		{"secret": "sk-issue203-admin-replacement"},
		{"name": "admin rename"},
		{"enabled": true},
		{"auth_scheme": "none"},
		{"clear_secret": true},
		{"key_env": "GNVIPI_KEY_ADMIN"},
	} {
		res = makeRequest(t, h.engine, http.MethodPut, keysPath(ownerKeyID), body, admin.headers)
		if res.Code != http.StatusForbidden {
			t.Fatalf("admin update user key with %v = %d, want 403 (body %s)", body, res.Code, res.Body.String())
		}
	}
	res = makeRequest(t, h.engine, http.MethodDelete, keysPath(ownerKeyID), nil, admin.headers)
	if res.Code != http.StatusForbidden {
		t.Fatalf("admin delete user key = %d, want 403 (body %s)", res.Code, res.Body.String())
	}
	res = makeRequest(t, h.engine, http.MethodPost, keysPath(ownerKeyID)+"/test", nil, admin.headers)
	if res.Code != http.StatusForbidden {
		t.Fatalf("admin test user key = %d, want 403 (body %s)", res.Code, res.Body.String())
	}

	res = makeRequest(t, h.engine, http.MethodGet, keysPath(""), nil, bearer(h.token))
	if res.Code != http.StatusOK {
		t.Fatalf("owner list keys = %d, want 200 (body %s)", res.Code, res.Body.String())
	}
	if items, _ := decodeBody(t, res)["keys"].([]any); len(items) != 3 {
		t.Fatalf("owner list = %v, want every key in the deployment", items)
	}
	res = makeRequest(t, h.engine, http.MethodPut, keysPath(ownerKeyID), gin.H{"name": "owner renamed"}, bearer(h.token))
	if res.Code != http.StatusOK || decodeBody(t, res)["name"] != "owner renamed" {
		t.Fatalf("owner update user key = %d %s, want renamed", res.Code, res.Body.String())
	}

	userKeyID, _ := userKey["id"].(string)
	res = makeRequest(t, h.engine, http.MethodDelete, keysPath(userKeyID), nil, user.headers)
	if res.Code != http.StatusNoContent {
		t.Fatalf("user delete own key = %d, want 204 (body %s)", res.Code, res.Body.String())
	}
	res = makeRequest(t, h.engine, http.MethodGet, keysPath(userKeyID), nil, bearer(h.token))
	if res.Code != http.StatusNotFound {
		t.Fatalf("read deleted key = %d, want 404 (body %s)", res.Code, res.Body.String())
	}

	res = makeRequest(t, h.engine, http.MethodDelete, keysPath(adminOwnID), nil, admin.headers)
	if res.Code != http.StatusNoContent {
		t.Fatalf("admin delete own key = %d, want 204 (body %s)", res.Code, res.Body.String())
	}
}

func TestDBAdminKeys_SecretMaskingAndAudit(t *testing.T) {
	h := newDBAdminHarness(t)
	insertTestProvider(t, h, keyTestProviderURL)

	created := createTestKey(t, h, bearer(h.token), gin.H{"name": "masked", "secret": keyTestSecret})
	if created["has_secret"] != true || created["secret_last4"] != "1234" {
		t.Fatalf("created key = %v, want has_secret and last4 1234", created)
	}
	keyID, _ := created["id"].(string)
	enc, nonce, last4 := storedKeySecret(t, h, keyID)
	if len(enc) == 0 || len(nonce) != providerstore.SecretNonceSize || last4 != "1234" {
		t.Fatalf("stored secret = enc %d bytes nonce %d last4 %q, want ciphertext, 12-byte nonce and 1234", len(enc), len(nonce), last4)
	}
	if bytes.Contains(enc, []byte(keyTestSecret)) {
		t.Fatal("stored ciphertext contains the plaintext secret")
	}

	res := makeRequest(t, h.engine, http.MethodGet, keysPath(keyID), nil, bearer(h.token))
	if res.Code != http.StatusOK {
		t.Fatalf("read key = %d, want 200 (body %s)", res.Code, res.Body.String())
	}
	read := decodeBody(t, res)
	assertNoKeySecretMaterial(t, read)
	for _, field := range []string{"id", "provider_id", "owner_id", "name", "enabled", "auth_scheme",
		"auth_header_name", "has_secret", "secret_last4", "has_key_env", "has_key_ref",
		"key_env", "key_ref", "created_at", "updated_at"} {
		if _, ok := read[field]; !ok {
			t.Fatalf("key response omitted %q: %v", field, read)
		}
	}

	var leaked int
	if err := h.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM audit_log WHERE details::text LIKE '%' || $1 || '%'`,
		keyTestSecret).Scan(&leaked); err != nil {
		t.Fatalf("scan audit leakage: %v", err)
	}
	if leaked != 0 {
		t.Fatalf("%d audit rows contain the plaintext secret", leaked)
	}
	var actions []string
	rows, err := h.pool.Query(t.Context(), `SELECT action FROM audit_log WHERE key_id = $1::uuid ORDER BY id`, keyID)
	if err != nil {
		t.Fatalf("read key audit rows: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var action string
		if err := rows.Scan(&action); err != nil {
			t.Fatalf("scan audit action: %v", err)
		}
		actions = append(actions, action)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read key audit rows: %v", err)
	}
	if len(actions) != 1 || actions[0] != "key.create" {
		t.Fatalf("key audit actions = %v, want [key.create]", actions)
	}
}

func TestDBAdminKeys_UpdateKeepsSecretAndExplicitClear(t *testing.T) {
	h := newDBAdminHarness(t)
	insertTestProvider(t, h, keyTestProviderURL)

	created := createTestKey(t, h, bearer(h.token), gin.H{"name": "keep secret", "secret": keyTestSecret})
	keyID, _ := created["id"].(string)
	beforeEnc, beforeNonce, _ := storedKeySecret(t, h, keyID)

	res := makeRequest(t, h.engine, http.MethodPut, keysPath(keyID), gin.H{"name": "renamed"}, bearer(h.token))
	if res.Code != http.StatusOK {
		t.Fatalf("rename = %d, want 200 (body %s)", res.Code, res.Body.String())
	}
	updated := decodeBody(t, res)
	if updated["has_secret"] != true || updated["secret_last4"] != "1234" || updated["name"] != "renamed" {
		t.Fatalf("renamed key = %v, want unchanged secret and new name", updated)
	}
	afterEnc, afterNonce, _ := storedKeySecret(t, h, keyID)
	if !bytes.Equal(beforeEnc, afterEnc) || !bytes.Equal(beforeNonce, afterNonce) {
		t.Fatal("an update without a secret re-encrypted the stored secret")
	}

	res = makeRequest(t, h.engine, http.MethodPut, keysPath(keyID), gin.H{"secret": ""}, bearer(h.token))
	if res.Code != http.StatusOK || decodeBody(t, res)["has_secret"] != true {
		t.Fatalf("empty secret patch = %d %s, want the stored secret kept", res.Code, res.Body.String())
	}

	res = makeRequest(t, h.engine, http.MethodPut, keysPath(keyID), gin.H{"clear_secret": true}, bearer(h.token))
	if res.Code != http.StatusUnprocessableEntity {
		t.Fatalf("clear enabled secret = %d, want 422 (body %s)", res.Code, res.Body.String())
	}
	assertFieldError(t, decodeBody(t, res), "credential")

	res = makeRequest(t, h.engine, http.MethodPut, keysPath(keyID), gin.H{"enabled": false, "clear_secret": true}, bearer(h.token))
	if res.Code != http.StatusOK {
		t.Fatalf("disable and clear = %d, want 200 (body %s)", res.Code, res.Body.String())
	}
	cleared := decodeBody(t, res)
	if cleared["has_secret"] != false || cleared["secret_last4"] != "" || cleared["enabled"] != false {
		t.Fatalf("cleared key = %v, want no secret and disabled", cleared)
	}
	var nulls int
	if err := h.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM provider_keys
		WHERE id = $1::uuid AND secret_enc IS NULL AND secret_nonce IS NULL
		  AND secret_kver IS NULL AND secret_last4 IS NULL`, keyID).Scan(&nulls); err != nil {
		t.Fatalf("check cleared secret columns: %v", err)
	}
	if nulls != 1 {
		t.Fatal("clear_secret left secret columns populated")
	}

	res = makeRequest(t, h.engine, http.MethodPut, keysPath(keyID), gin.H{"enabled": true, "key_env": "GNVIPI_KEY_ISSUE203"}, bearer(h.token))
	if res.Code != http.StatusOK {
		t.Fatalf("key_env replacement = %d, want 200 (body %s)", res.Code, res.Body.String())
	}
	replaced := decodeBody(t, res)
	if replaced["has_key_env"] != true || replaced["key_env"] != "GNVIPI_KEY_ISSUE203" || replaced["has_secret"] != false {
		t.Fatalf("key_env key = %v, want key_env only", replaced)
	}
	res = makeRequest(t, h.engine, http.MethodPut, keysPath(keyID), gin.H{"clear_secret": true}, bearer(h.token))
	if res.Code != http.StatusOK {
		t.Fatalf("clear_secret with key_env = %d, want 200 (body %s)", res.Code, res.Body.String())
	}

	res = makeRequest(t, h.engine, http.MethodPut, keysPath(keyID), gin.H{"secret": "sk-issue203-rotated-9876"}, bearer(h.token))
	if res.Code != http.StatusOK {
		t.Fatalf("rotate secret = %d, want 200 (body %s)", res.Code, res.Body.String())
	}
	rotated := decodeBody(t, res)
	if rotated["has_secret"] != true || rotated["secret_last4"] != "9876" || rotated["has_key_env"] != false {
		t.Fatalf("rotated key = %v, want the new secret and no key_env", rotated)
	}
	assertNoKeySecretMaterial(t, rotated)
}

func TestDBAdminKeys_BasicAuthSecret(t *testing.T) {
	h := newDBAdminHarness(t)
	insertTestProvider(t, h, keyTestProviderURL)

	res := makeRequest(t, h.engine, http.MethodPost, keysPath(""), gin.H{
		"name": "basic", "auth_scheme": "basic", "secret": "not-json",
	}, bearer(h.token))
	if res.Code != http.StatusUnprocessableEntity {
		t.Fatalf("non-JSON basic secret = %d, want 422 (body %s)", res.Code, res.Body.String())
	}
	assertFieldError(t, decodeBody(t, res), "credential")

	created := createTestKey(t, h, bearer(h.token), gin.H{
		"name": "basic", "auth_scheme": "basic", "secret": keyBasicSecret,
	})
	if created["secret_last4"] != "wxyz" || created["has_secret"] != true {
		t.Fatalf("basic key = %v, want last4 of the password", created)
	}
	keyID, _ := created["id"].(string)

	var row providerstore.KeyRow
	var enc, nonce []byte
	var kver int
	var last4 string
	if err := h.pool.QueryRow(t.Context(), `
		SELECT id::text, provider_id, COALESCE(owner_id::text, ''), name, enabled,
		       auth_scheme, COALESCE(auth_header_name, ''), COALESCE(key_env, ''),
		       COALESCE(key_ref, ''), secret_enc, secret_nonce, secret_kver, secret_last4
		FROM provider_keys WHERE id = $1::uuid`, keyID).Scan(
		&row.ID, &row.ProviderID, &row.OwnerID, &row.Name, &row.Enabled,
		&row.AuthScheme, &row.AuthHeaderName, &row.KeyEnv, &row.KeyRef,
		&enc, &nonce, &kver, &last4); err != nil {
		t.Fatalf("read basic key row: %v", err)
	}
	row.SecretEnc, row.SecretNonce, row.SecretKVer, row.SecretLast4 = enc, nonce, kver, last4
	if bytes.Contains(enc, []byte("issue203")) {
		t.Fatal("basic secret ciphertext contains the plaintext username")
	}
	credential, err := providerstore.ResolveKeyCredential(t.Context(), row, h.state.keyring, nil)
	if err != nil {
		t.Fatalf("resolve basic credential: %v", err)
	}
	if credential.Basic.Username != "issue203" || credential.Basic.Password != "hunter2-wxyz" {
		t.Fatalf("resolved basic credential = %+v, want the stored JSON pair", credential.Basic)
	}

	res = makeRequest(t, h.engine, http.MethodGet, keysPath(keyID), nil, bearer(h.token))
	if res.Code != http.StatusOK {
		t.Fatalf("read basic key = %d, want 200 (body %s)", res.Code, res.Body.String())
	}
	assertNoKeySecretMaterial(t, decodeBody(t, res))

	res = makeRequest(t, h.engine, http.MethodPut, keysPath(keyID), gin.H{"auth_scheme": "bearer"}, bearer(h.token))
	if res.Code != http.StatusUnprocessableEntity {
		t.Fatalf("scheme change with stored secret = %d, want 422 (body %s)", res.Code, res.Body.String())
	}
	assertFieldError(t, decodeBody(t, res), "credential")
}

func TestDBAdminKeys_DuplicateNameAndAlias(t *testing.T) {
	h := newDBAdminHarness(t)
	insertTestProvider(t, h, keyTestProviderURL)

	first := createTestKey(t, h, bearer(h.token), gin.H{"name": "duplicate", "secret": keyTestSecret})
	res := makeRequest(t, h.engine, http.MethodPost, keysPath(""), gin.H{
		"name": "duplicate", "secret": keyTestSecret,
	}, bearer(h.token))
	if res.Code != http.StatusConflict {
		t.Fatalf("duplicate key name = %d, want 409 (body %s)", res.Code, res.Body.String())
	}
	assertFieldError(t, decodeBody(t, res), "name")

	userID := createContractUser(t, h, "key-dup-user", auth.RoleUser)
	other := createTestKey(t, h, bearer(h.token), gin.H{
		"name": "duplicate", "secret": keyTestSecret, "owner_id": userID,
	})
	otherID, _ := other["id"].(string)

	res = makeRequest(t, h.engine, http.MethodPost, keysPath(""), gin.H{
		"name": "shared name", "auth_scheme": "none", "owner_id": nil,
	}, bearer(h.token))
	if res.Code != http.StatusCreated {
		t.Fatalf("create shared key = %d, want 201 (body %s)", res.Code, res.Body.String())
	}
	res = makeRequest(t, h.engine, http.MethodPost, keysPath(""), gin.H{
		"name": "shared name", "auth_scheme": "none", "owner_id": nil,
	}, bearer(h.token))
	if res.Code != http.StatusConflict {
		t.Fatalf("duplicate shared key name = %d, want 409 (body %s)", res.Code, res.Body.String())
	}
	assertFieldError(t, decodeBody(t, res), "name")

	firstID, _ := first["id"].(string)
	res = makeRequest(t, h.engine, http.MethodPut, keysPath(firstID)+"/models", gin.H{
		"models": []gin.H{
			{"alias": "issue203/*", "upstream_model": ""},
			{"alias": "gpt-issue203", "upstream_model": "gpt-4o-mini"},
		},
	}, bearer(h.token))
	if res.Code != http.StatusOK {
		t.Fatalf("write models = %d, want 200 (body %s)", res.Code, res.Body.String())
	}
	res = makeRequest(t, h.engine, http.MethodGet, keysPath(firstID)+"/models", nil, bearer(h.token))
	if res.Code != http.StatusOK {
		t.Fatalf("read models = %d, want 200 (body %s)", res.Code, res.Body.String())
	}
	models := decodeBody(t, res)
	if items, _ := models["models"].([]any); len(items) != 2 {
		t.Fatalf("models = %v, want two mappings", models)
	}

	// Aliases live in a per-owner namespace, so a different owner may reuse one.
	res = makeRequest(t, h.engine, http.MethodPut, keysPath(otherID)+"/models", gin.H{
		"models": []gin.H{{"alias": "gpt-issue203", "upstream_model": "gpt-4o"}},
	}, bearer(h.token))
	if res.Code != http.StatusOK {
		t.Fatalf("alias reused in another owner scope = %d, want 200 (body %s)", res.Code, res.Body.String())
	}
	second := createTestKey(t, h, bearer(h.token), gin.H{"name": "second owner key", "secret": keyTestSecret})
	secondID, _ := second["id"].(string)
	res = makeRequest(t, h.engine, http.MethodPut, keysPath(secondID)+"/models", gin.H{
		"models": []gin.H{{"alias": "gpt-issue203", "upstream_model": "gpt-4o"}},
	}, bearer(h.token))
	if res.Code != http.StatusConflict {
		t.Fatalf("duplicate alias in one owner scope = %d, want 409 (body %s)", res.Code, res.Body.String())
	}
	assertFieldError(t, decodeBody(t, res), "alias")

	res = makeRequest(t, h.engine, http.MethodPut, keysPath(firstID)+"/models", gin.H{
		"models": []gin.H{
			{"alias": "dup-alias", "upstream_model": "a"},
			{"alias": "dup-alias", "upstream_model": "b"},
		},
	}, bearer(h.token))
	if res.Code != http.StatusBadRequest {
		t.Fatalf("duplicate alias in one payload = %d, want 400 (body %s)", res.Code, res.Body.String())
	}
	assertFieldError(t, decodeBody(t, res), "alias")

	// The failed write left the previous mappings intact.
	res = makeRequest(t, h.engine, http.MethodGet, keysPath(firstID)+"/models", nil, bearer(h.token))
	if items, _ := decodeBody(t, res)["models"].([]any); len(items) != 2 {
		t.Fatalf("models after refused write = %v, want the two original mappings", items)
	}
}

func TestDBAdminKeys_ValidationAndCredentialSources(t *testing.T) {
	h := newDBAdminHarness(t)
	insertTestProvider(t, h, keyTestProviderURL)

	res := makeRequest(t, h.engine, http.MethodGet, "/admin/api/providers/missing-provider/keys", nil, bearer(h.token))
	if res.Code != http.StatusNotFound {
		t.Fatalf("list keys for unknown provider = %d, want 404 (body %s)", res.Code, res.Body.String())
	}
	res = makeRequest(t, h.engine, http.MethodPost, keysPath(""), gin.H{"name": " ", "secret": keyTestSecret}, bearer(h.token))
	if res.Code != http.StatusBadRequest && res.Code != http.StatusUnprocessableEntity {
		t.Fatalf("empty key name = %d, want a field error (body %s)", res.Code, res.Body.String())
	}
	assertFieldError(t, decodeBody(t, res), "name")
	res = makeRequest(t, h.engine, http.MethodGet, keysPath("not-a-uuid"), nil, bearer(h.token))
	if res.Code != http.StatusNotFound {
		t.Fatalf("malformed key id = %d, want 404 (body %s)", res.Code, res.Body.String())
	}

	for _, body := range []gin.H{
		{"name": "reserved env", "key_env": "GNVIPI_MASTER_KEY"},
		{"name": "both sources", "secret": keyTestSecret, "key_env": "GNVIPI_KEY_X"},
		{"name": "none with secret", "auth_scheme": "none", "secret": keyTestSecret},
		{"name": "header without name", "auth_scheme": "header", "secret": keyTestSecret},
	} {
		res = makeRequest(t, h.engine, http.MethodPost, keysPath(""), body, bearer(h.token))
		if res.Code != http.StatusUnprocessableEntity {
			t.Fatalf("create %v = %d, want 422 (body %s)", body["name"], res.Code, res.Body.String())
		}
		assertFieldError(t, decodeBody(t, res), "credential")
	}

	// An auth header name cannot pick its own scheme: without auth_scheme the
	// request is ambiguous, so it is refused on the auth_scheme field.
	res = makeRequest(t, h.engine, http.MethodPost, keysPath(""), gin.H{
		"name": "header name without scheme", "auth_header_name": "X-Issue203", "secret": keyTestSecret,
	}, bearer(h.token))
	if res.Code != http.StatusBadRequest {
		t.Fatalf("auth_header_name without auth_scheme = %d, want 400 (body %s)", res.Code, res.Body.String())
	}
	assertFieldError(t, decodeBody(t, res), "auth_scheme")

	// A credential with no auth_scheme defaults to bearer.
	defaulted := createTestKey(t, h, bearer(h.token), gin.H{"name": "default bearer", "secret": keyTestSecret})
	if defaulted["auth_scheme"] != providerstore.SchemeBearer {
		t.Fatalf("scheme-less credential = %v, want the bearer default", defaulted["auth_scheme"])
	}

	// An enabled non-none key needs exactly one credential source, so a scheme
	// without a secret is refused on the credential field, not by a raw DB
	// constraint error.
	res = makeRequest(t, h.engine, http.MethodPost, keysPath(""), gin.H{
		"name": "bearer without secret", "auth_scheme": "bearer",
	}, bearer(h.token))
	if res.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bearer key without a credential = %d, want 422 (body %s)", res.Code, res.Body.String())
	}
	assertFieldError(t, decodeBody(t, res), "credential")

	// Switching a key to "none" while handing over a secret must not silently
	// discard the credential.
	live := createTestKey(t, h, bearer(h.token), gin.H{"name": "none transition", "secret": keyTestSecret})
	res = makeRequest(t, h.engine, http.MethodPut, keysPath(live["id"].(string)), gin.H{
		"auth_scheme": "none", "secret": keyTestSecret,
	}, bearer(h.token))
	if res.Code != http.StatusUnprocessableEntity {
		t.Fatalf("update to none with secret = %d, want 422 (body %s)", res.Code, res.Body.String())
	}
	assertFieldError(t, decodeBody(t, res), "credential")

	// key_ref never reaches a regular user, but the owner may store one.
	user := prepareKeyActor(t, h, "key-ref-user", auth.RoleUser)
	res = makeRequest(t, h.engine, http.MethodPost, keysPath(""), gin.H{
		"name": "user ref", "key_ref": "env:ISSUE203",
	}, user.headers)
	if res.Code != http.StatusUnprocessableEntity {
		t.Fatalf("user key_ref create = %d, want 422 (body %s)", res.Code, res.Body.String())
	}
	assertFieldError(t, decodeBody(t, res), "credential")
	refKey := createTestKey(t, h, bearer(h.token), gin.H{"name": "owner ref", "key_ref": "env:ISSUE203"})
	if refKey["has_key_ref"] != true || refKey["key_ref"] != "env:ISSUE203" || refKey["has_secret"] != false {
		t.Fatalf("key_ref key = %v, want the reference only", refKey)
	}

	// A shared key must stay credential-free.
	res = makeRequest(t, h.engine, http.MethodPost, keysPath(""), gin.H{
		"name": "shared bearer", "auth_scheme": "bearer", "secret": keyTestSecret, "owner_id": nil,
	}, bearer(h.token))
	if res.Code != http.StatusUnprocessableEntity {
		t.Fatalf("shared bearer key = %d, want 422 (body %s)", res.Code, res.Body.String())
	}
	assertFieldError(t, decodeBody(t, res), "credential")

	// Unknown provider on create is a 404, not a foreign-key 500.
	res = makeRequest(t, h.engine, http.MethodPost, "/admin/api/providers/ghost/keys", gin.H{"name": "x"}, bearer(h.token))
	if res.Code != http.StatusNotFound {
		t.Fatalf("create key for unknown provider = %d, want 404 (body %s)", res.Code, res.Body.String())
	}
}

func TestDBAdminKeys_TestEndpoint(t *testing.T) {
	h := newDBAdminHarness(t)
	basicAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("issue203:hunter2-wxyz"))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer " + keyTestSecret, basicAuth:
			w.Header().Set("Content-Type", "application/json")
			if _, err := w.Write([]byte(`{"data":[]}`)); err != nil {
				t.Errorf("write upstream response: %v", err)
			}
		default:
			w.WriteHeader(http.StatusUnauthorized)
			if _, err := w.Write([]byte(`{"error":"bad credential"}`)); err != nil {
				t.Errorf("write upstream response: %v", err)
			}
		}
	}))
	t.Cleanup(upstream.Close)
	insertTestProvider(t, h, upstream.URL)

	key := createTestKey(t, h, bearer(h.token), gin.H{"name": "probe", "secret": keyTestSecret})
	keyID, _ := key["id"].(string)
	res := makeRequest(t, h.engine, http.MethodPost, keysPath(keyID)+"/test", nil, bearer(h.token))
	if res.Code != http.StatusOK {
		t.Fatalf("key test = %d, want 200 (body %s)", res.Code, res.Body.String())
	}
	result := decodeBody(t, res)
	if result["ok"] != true || result["status_code"] != float64(http.StatusOK) {
		t.Fatalf("key test result = %v, want ok with status 200", result)
	}
	assertNoKeySecretMaterial(t, result)

	// A rejected credential is a result, not an error, and never echoes the secret.
	wrong := createTestKey(t, h, bearer(h.token), gin.H{"name": "probe wrong", "secret": "sk-issue203-wrong-9999"})
	res = makeRequest(t, h.engine, http.MethodPost, keysPath(wrong["id"].(string))+"/test", nil, bearer(h.token))
	if res.Code != http.StatusOK {
		t.Fatalf("key test rejection = %d, want 200 (body %s)", res.Code, res.Body.String())
	}
	rejected := decodeBody(t, res)
	if rejected["ok"] != false || rejected["status_code"] != float64(http.StatusUnauthorized) || rejected["status"] != "auth_failure" {
		t.Fatalf("rejected key test = %v, want auth_failure 401", rejected)
	}
	assertNoKeySecretMaterial(t, rejected, "sk-issue203-wrong-9999")

	basic := createTestKey(t, h, bearer(h.token), gin.H{
		"name": "probe basic", "auth_scheme": "basic", "secret": keyBasicSecret,
	})
	res = makeRequest(t, h.engine, http.MethodPost, keysPath(basic["id"].(string))+"/test", nil, bearer(h.token))
	if res.Code != http.StatusOK {
		t.Fatalf("basic key test = %d, want 200 (body %s)", res.Code, res.Body.String())
	}
	basicResult := decodeBody(t, res)
	if basicResult["ok"] != true {
		t.Fatalf("basic key test = %v, want ok", basicResult)
	}
	assertNoKeySecretMaterial(t, basicResult, "hunter2-wxyz", "issue203:hunter2-wxyz")

	// A key without any credential cannot be probed.
	noneKey := createTestKey(t, h, bearer(h.token), gin.H{"name": "probe none", "auth_scheme": "none"})
	res = makeRequest(t, h.engine, http.MethodPost, keysPath(noneKey["id"].(string))+"/test", nil, bearer(h.token))
	if res.Code != http.StatusUnprocessableEntity {
		t.Fatalf("key test without credential = %d, want 422 (body %s)", res.Code, res.Body.String())
	}
	assertFieldError(t, decodeBody(t, res), "credential")
}

// TestDBAdminKeys_ModelPayloadCanonicalValidation keeps rows that the snapshot
// loader would reject out of the database: the canonical validator runs inside
// ReplaceKeyModels, not a weaker request-local copy.
func TestDBAdminKeys_ModelPayloadCanonicalValidation(t *testing.T) {
	h := newDBAdminHarness(t)
	insertTestProvider(t, h, keyTestProviderURL)
	key := createTestKey(t, h, bearer(h.token), gin.H{"name": "canonical models", "secret": keyTestSecret})
	keyID, _ := key["id"].(string)

	res := makeRequest(t, h.engine, http.MethodPut, keysPath(keyID)+"/models", gin.H{
		"models": []gin.H{{"alias": "gpt-issue203", "upstream_model": "gpt-4o"}},
	}, bearer(h.token))
	if res.Code != http.StatusOK {
		t.Fatalf("seed models = %d, want 200 (body %s)", res.Code, res.Body.String())
	}

	for _, tc := range []struct {
		name  string
		model gin.H
	}{
		{"alias with space", gin.H{"alias": "bad alias", "upstream_model": "m"}},
		{"empty fallback", gin.H{"alias": "a", "upstream_model": "m", "fallbacks": []string{""}}},
		{"wildcard fallback", gin.H{"alias": "a", "upstream_model": "m", "fallbacks": []string{"*"}}},
		{"status out of range", gin.H{"alias": "a", "upstream_model": "m", "fallback_statuses": []int{99}}},
		{"legacy position overflow", gin.H{"alias": "a", "upstream_model": "m", "legacy_position": 2147483648}},
	} {
		res = makeRequest(t, h.engine, http.MethodPut, keysPath(keyID)+"/models", gin.H{
			"models": []gin.H{tc.model},
		}, bearer(h.token))
		if res.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s = %d, want 422 (body %s)", tc.name, res.Code, res.Body.String())
		}
		assertFieldError(t, decodeBody(t, res), "models")
	}

	res = makeRequest(t, h.engine, http.MethodGet, keysPath(keyID)+"/models", nil, bearer(h.token))
	if items, _ := decodeBody(t, res)["models"].([]any); len(items) != 1 {
		t.Fatalf("models after rejected writes = %v, want the seed mapping", items)
	}
}
