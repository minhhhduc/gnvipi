package main

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/minhhhduc/gnvipi/internal/auth"
)

// TestDBAdminProviders_V2AuthPersistsAsProviderKey proves a v2 auth block is
// written as a provider_keys row through the key encryption path, and that a
// later auth update patches that same key instead of dropping it.
func TestDBAdminProviders_V2AuthPersistsAsProviderKey(t *testing.T) {
	h := newDBAdminHarness(t)
	const providerID = "issue204-auth"
	res := makeRequest(t, h.engine, http.MethodPost, providersAPIPath(""), gin.H{
		"id": providerID, "name": "Auth", "protocol": "openai-chat",
		"base_url": "https://issue204.example/v1", "enabled": true,
		"auth":   gin.H{"scheme": "bearer", "key": dbProviderSecret},
		"models": []any{gin.H{"alias": "auth-alias", "upstream_model": "m"}},
	}, bearer(h.token))
	if res.Code != http.StatusCreated {
		t.Fatalf("create with auth = %d, want 201 (body %s)", res.Code, res.Body.String())
	}
	view := decodeBody(t, res)
	authBlock, _ := view["auth"].(map[string]any)
	if authBlock["scheme"] != "bearer" {
		t.Fatalf("created auth = %v, want bearer", view["auth"])
	}
	assertNoKeySecretMaterial(t, view)
	if models, _ := view["models"].([]any); len(models) != 1 {
		t.Fatalf("created models = %v, want the one auth-backed mapping", view["models"])
	}
	ownerID, scheme, keyEnv, hasSecret := providerKeyAuth(t, h, providerID)
	if ownerID != h.owner.ID || scheme != "bearer" || keyEnv != "" || !hasSecret {
		t.Fatalf("stored key owner=%q scheme=%q key_env=%q has_secret=%v", ownerID, scheme, keyEnv, hasSecret)
	}
	var keyID string
	if err := h.pool.QueryRow(t.Context(), `SELECT id::text FROM provider_keys WHERE provider_id = $1`, providerID).Scan(&keyID); err != nil {
		t.Fatalf("read key id: %v", err)
	}
	if count := keyModelCount(t, h, keyID); count != 1 {
		t.Fatalf("key_models = %d, want 1", count)
	}

	res = makeRequest(t, h.engine, http.MethodPut, providersAPIPath(providerID), gin.H{
		"id": providerID, "name": "Auth", "protocol": "openai-chat",
		"base_url": "https://issue204.example/v1", "enabled": true,
		"auth": gin.H{"scheme": "bearer", "key_env": "GNVIPI_KEY_ISSUE204"},
	}, bearer(h.token))
	if res.Code != http.StatusOK {
		t.Fatalf("update auth = %d, want 200 (body %s)", res.Code, res.Body.String())
	}
	ownerID, scheme, keyEnv, hasSecret = providerKeyAuth(t, h, providerID)
	if ownerID != h.owner.ID || scheme != "bearer" || keyEnv != "GNVIPI_KEY_ISSUE204" || hasSecret {
		t.Fatalf("updated key owner=%q scheme=%q key_env=%q has_secret=%v", ownerID, scheme, keyEnv, hasSecret)
	}
	if count := keyModelCount(t, h, keyID); count != 1 {
		t.Fatalf("key_models = %d after an auth-only update, want the mapping kept", count)
	}
}

// TestDBAdminProviders_EchoedModelsAreNotAWrite proves the console echo of a
// multi-key provider's aggregate models is a no-op, not the 409 a real write
// gets.
func TestDBAdminProviders_EchoedModelsAreNotAWrite(t *testing.T) {
	h := newDBAdminHarness(t)
	const providerID = "issue204-echo"
	createDBProvider(t, h, gin.H{
		"id": providerID, "name": "Echo", "protocol": "openai-chat",
		"base_url": "https://issue204.example/v1", "enabled": true,
	})
	first := createDBProviderKey(t, h, providerID, gin.H{"name": "echo one", "secret": dbProviderSecret})
	second := createDBProviderKey(t, h, providerID, gin.H{"name": "echo two", "secret": dbProviderSecret + "-two"})
	firstID, _ := first["id"].(string)
	secondID, _ := second["id"].(string)
	aliases := []string{"echo-a", "echo-b"}
	for i, keyID := range []string{firstID, secondID} {
		res := makeRequest(t, h.engine, http.MethodPut,
			providersAPIPath(providerID)+"/keys/"+keyID+"/models",
			gin.H{"models": []any{gin.H{"alias": aliases[i], "upstream_model": "m"}}},
			bearer(h.token))
		if res.Code != http.StatusOK {
			t.Fatalf("seed key models = %d (body %s)", res.Code, res.Body.String())
		}
	}

	res := makeRequest(t, h.engine, http.MethodGet, providersAPIPath(providerID), nil, bearer(h.token))
	if res.Code != http.StatusOK {
		t.Fatalf("get provider = %d (body %s)", res.Code, res.Body.String())
	}
	view := decodeBody(t, res)
	models, _ := view["models"].([]any)
	if len(models) != 2 {
		t.Fatalf("aggregate models = %v, want two", view["models"])
	}

	// The SPA sends the GET body back on save. The unchanged models list must
	// survive the two-key provider without a 409.
	res = makeRequest(t, h.engine, http.MethodPut, providersAPIPath(providerID), gin.H{
		"id": providerID, "name": "Echo", "protocol": "openai-chat",
		"base_url": "https://issue204.example/v1", "enabled": true,
		"models": models,
	}, bearer(h.token))
	if res.Code != http.StatusOK {
		t.Fatalf("echoed models write = %d, want 200 (body %s)", res.Code, res.Body.String())
	}
	if got := keyModelCount(t, h, firstID); got != 1 {
		t.Fatalf("first key models = %d after an echo, want 1", got)
	}
	if got := keyModelCount(t, h, secondID); got != 1 {
		t.Fatalf("second key models = %d after an echo, want 1", got)
	}
}

// TestDBAdminProviders_ProviderModelsRespectKeyOwnership proves a provider-level
// models write follows the per-key reach rules: an admin may not rewrite a
// foreign key's aliases.
func TestDBAdminProviders_ProviderModelsRespectKeyOwnership(t *testing.T) {
	h := newDBAdminHarness(t)
	const providerID = "issue204-owner-key"
	createDBProvider(t, h, gin.H{
		"id": providerID, "name": "Ownership", "protocol": "openai-chat",
		"base_url": "https://issue204.example/v1", "enabled": true,
		"auth": gin.H{"scheme": "bearer", "key": dbProviderSecret},
	})
	adminID := createContractUser(t, h, "issue204-admin", auth.RoleAdmin)
	clearMustChangePassword(t, h, adminID)
	adminToken, _ := createContractAdminToken(t, h, adminID, bearer(h.token))

	res := makeRequest(t, h.engine, http.MethodPut, providersAPIPath(providerID), gin.H{
		"id": providerID, "name": "Ownership", "protocol": "openai-chat",
		"base_url": "https://issue204.example/v1", "enabled": true,
		"models": []any{gin.H{"alias": "admin-write", "upstream_model": "m"}},
	}, bearer(adminToken))
	if res.Code != http.StatusForbidden {
		t.Fatalf("admin foreign-key models write = %d, want 403 (body %s)", res.Code, res.Body.String())
	}

	// An admin may write models on a key it owns.
	const ownProviderID = "issue204-admin-key"
	res = makeRequest(t, h.engine, http.MethodPost, providersAPIPath(""), gin.H{
		"id": ownProviderID, "name": "Admin key", "protocol": "openai-chat",
		"base_url": "https://issue204.example/v1", "enabled": true,
		"auth": gin.H{"scheme": "bearer", "key": dbProviderSecret + "-admin"},
	}, bearer(adminToken))
	if res.Code != http.StatusCreated {
		t.Fatalf("admin creates provider = %d (body %s)", res.Code, res.Body.String())
	}
	ownerID, _, _, _ := providerKeyAuth(t, h, ownProviderID)
	if ownerID != adminID {
		t.Fatalf("admin-created key owner = %q, want %q", ownerID, adminID)
	}
	res = makeRequest(t, h.engine, http.MethodPut, providersAPIPath(ownProviderID), gin.H{
		"id": ownProviderID, "name": "Admin key", "protocol": "openai-chat",
		"base_url": "https://issue204.example/v1", "enabled": true,
		"models": []any{gin.H{"alias": "admin-own", "upstream_model": "m"}},
	}, bearer(adminToken))
	if res.Code != http.StatusOK {
		t.Fatalf("admin own-key models write = %d, want 200 (body %s)", res.Code, res.Body.String())
	}
}

// TestDBAdminProviders_UserViewOmitsForeignKeys proves a user-role read does not
// project another owner's key material.
func TestDBAdminProviders_UserViewOmitsForeignKeys(t *testing.T) {
	h := newDBAdminHarness(t)
	const providerID = "issue204-user-view"
	res := makeRequest(t, h.engine, http.MethodPost, providersAPIPath(""), gin.H{
		"id": providerID, "name": "User view", "protocol": "openai-chat",
		"base_url": "https://issue204.example/v1", "enabled": true,
		"auth":   gin.H{"scheme": "bearer", "key": dbProviderSecret},
		"models": []any{gin.H{"alias": "owner-only", "upstream_model": "m"}},
	}, bearer(h.token))
	if res.Code != http.StatusCreated {
		t.Fatalf("owner creates provider = %d (body %s)", res.Code, res.Body.String())
	}
	userID := createContractUser(t, h, "issue204-viewer", auth.RoleUser)
	clearMustChangePassword(t, h, userID)
	userToken, _ := createContractAdminToken(t, h, userID, bearer(h.token))

	res = makeRequest(t, h.engine, http.MethodGet, providersAPIPath(providerID), nil, bearer(userToken))
	if res.Code != http.StatusOK {
		t.Fatalf("user get provider = %d (body %s)", res.Code, res.Body.String())
	}
	view := decodeBody(t, res)
	if authBlock, _ := view["auth"].(map[string]any); authBlock["scheme"] != "none" {
		t.Fatalf("user auth = %v, want none for a foreign-only provider", view["auth"])
	}
	if models, _ := view["models"].([]any); len(models) != 0 {
		t.Fatalf("user models = %v, want no foreign aliases", view["models"])
	}

	res = makeRequest(t, h.engine, http.MethodGet, providersAPIPath(providerID), nil, bearer(h.token))
	if authBlock, _ := decodeBody(t, res)["auth"].(map[string]any); authBlock["scheme"] != "bearer" {
		t.Fatalf("owner auth = %v, want the owner's key", authBlock)
	}
}

// TestDBAdminProviders_TestEndpointUsesDatabaseRow proves Test Connection routes
// to the database provider instead of 404ing against the file store.
func TestDBAdminProviders_TestEndpointUsesDatabaseRow(t *testing.T) {
	h := newDBAdminHarness(t)
	const providerID = "issue204-test"
	createDBProvider(t, h, gin.H{
		"id": providerID, "name": "Test", "protocol": "openai-chat",
		"base_url": "http://127.0.0.1:1/v1", "enabled": true,
		"auth": gin.H{"scheme": "bearer", "key": dbProviderSecret},
	})
	res := makeRequest(t, h.engine, http.MethodPost, providersAPIPath(providerID)+"/test", nil, bearer(h.token))
	if res.Code != http.StatusOK {
		t.Fatalf("provider test = %d, want 200 (body %s)", res.Code, res.Body.String())
	}
	if _, ok := decodeBody(t, res)["ok"]; !ok {
		t.Fatalf("provider test response = %s, want a connection result", res.Body.String())
	}
}
