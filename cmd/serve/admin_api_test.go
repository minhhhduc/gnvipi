package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	gatewayprovider "github.com/minhhhduc/gnvipi/internal/gateway/provider"
)

func setupTestPrefs(t *testing.T) *modelPrefs {
	t.Helper()
	path := filepath.Join(t.TempDir(), "models.json")
	p := &modelPrefs{}
	if err := p.load(path); err != nil {
		t.Fatalf("load test prefs: %v", err)
	}
	return p
}

func testAdminEngine(t *testing.T, p *modelPrefs, token string, adminLimit int64) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	if adminLimit > 0 {
		engine.Use(requestBodyLimit(1<<20, adminLimit))
	}
	engine.Use(adminAccess("127.0.0.1:8080", token))
	registerAdminAPIRoutes(engine, p)
	engine.POST("/admin/providers", func(c *gin.Context) {
		handleAdminProviders(c)
	})
	return engine
}

func makeRequest(t *testing.T, engine *gin.Engine, method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var bodyReader *bytes.Reader
	if body != nil {
		switch v := body.(type) {
		case string:
			bodyReader = bytes.NewReader([]byte(v))
		case []byte:
			bodyReader = bytes.NewReader(v)
		default:
			raw, err := json.Marshal(v)
			if err != nil {
				t.Fatalf("marshal body: %v", err)
			}
			bodyReader = bytes.NewReader(raw)
		}
	} else {
		bodyReader = bytes.NewReader(nil)
	}

	req := httptest.NewRequestWithContext(t.Context(), method, path, bodyReader)
	req.Host = "127.0.0.1:8080"
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}

func TestAdminAPI_ProvidersCRUD_RoundTripAndRedaction(t *testing.T) {
	p := setupTestPrefs(t)
	oldPrefs := prefs
	prefs = p
	t.Cleanup(func() { prefs = oldPrefs })

	engine := testAdminEngine(t, p, "", 0)

	// 1. Initial list should be empty
	res := makeRequest(t, engine, http.MethodGet, "/admin/api/providers", nil, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("GET /admin/api/providers = %d, want %d", res.Code, http.StatusOK)
	}
	var list []gatewayprovider.Provider
	if err := json.Unmarshal(res.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected 0 providers, got %d", len(list))
	}

	// 2. Create a provider with secret key and custom sensitive headers
	newProvider := gatewayprovider.Provider{
		ID:       "openai-prod",
		Name:     "OpenAI Production",
		BaseURL:  "https://api.openai.com/v1",
		Protocol: ir.ProtocolOpenAIChat,
		Auth: gatewayprovider.Auth{
			Scheme: gatewayprovider.AuthBearer,
			Key:    "sk-real-live-secret-key-12345",
		},
		Headers: map[string]string{
			"X-Custom-Env": "production",
		},
		Models: []gatewayprovider.ModelMapping{
			{Alias: "gpt-4o", UpstreamModel: "gpt-4o"},
		},
		Enabled: true,
	}

	res = makeRequest(t, engine, http.MethodPost, "/admin/api/providers", newProvider, nil)
	if res.Code != http.StatusCreated {
		t.Fatalf("POST /admin/api/providers = %d, want %d: %s", res.Code, http.StatusCreated, res.Body.String())
	}
	var created gatewayprovider.Provider
	if err := json.Unmarshal(res.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal created provider: %v", err)
	}

	// Verify secrets are redacted in creation response
	if created.Auth.Key == "sk-real-live-secret-key-12345" {
		t.Fatalf("secret key leaked in creation response: %s", created.Auth.Key)
	}
	if !strings.HasPrefix(created.Auth.Key, "sk-...") || !strings.HasSuffix(created.Auth.Key, "2345") {
		t.Fatalf("unexpected redacted key format: %s", created.Auth.Key)
	}

	// 3. GET /admin/api/providers should return 1 provider, redacted
	res = makeRequest(t, engine, http.MethodGet, "/admin/api/providers", nil, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("GET /admin/api/providers = %d", res.Code)
	}
	if err := json.Unmarshal(res.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 provider, got %d", len(list))
	}
	if list[0].Auth.Key == "sk-real-live-secret-key-12345" {
		t.Fatalf("secret leaked in list: %s", list[0].Auth.Key)
	}

	// 4. GET /admin/api/providers/:id should return the provider, redacted
	res = makeRequest(t, engine, http.MethodGet, "/admin/api/providers/openai-prod", nil, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("GET /admin/api/providers/openai-prod = %d", res.Code)
	}
	var fetched gatewayprovider.Provider
	if err := json.Unmarshal(res.Body.Bytes(), &fetched); err != nil {
		t.Fatalf("unmarshal fetched: %v", err)
	}
	if fetched.ID != "openai-prod" || fetched.Name != "OpenAI Production" {
		t.Fatalf("unexpected fetched provider: %+v", fetched)
	}
	if fetched.Auth.Key == "sk-real-live-secret-key-12345" {
		t.Fatalf("secret leaked in GET /:id: %s", fetched.Auth.Key)
	}

	// 5. PUT update with redacted key -> original secret preserved
	updatePayload := fetched
	updatePayload.Name = "OpenAI Prod Updated"
	// updatePayload.Auth.Key is currently "sk-...2345" (redacted)
	res = makeRequest(t, engine, http.MethodPut, "/admin/api/providers/openai-prod", updatePayload, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("PUT /admin/api/providers/openai-prod = %d: %s", res.Code, res.Body.String())
	}
	var updated gatewayprovider.Provider
	if err := json.Unmarshal(res.Body.Bytes(), &updated); err != nil {
		t.Fatalf("unmarshal updated: %v", err)
	}
	if updated.Name != "OpenAI Prod Updated" {
		t.Fatalf("name was not updated: %s", updated.Name)
	}

	// Verify that in persistence, the real secret was kept!
	persisted, found, err := p.getProviderV2("openai-prod")
	if err != nil || !found {
		t.Fatalf("getProviderV2 failed: found=%v, err=%v", found, err)
	}
	if persisted.Auth.Key != "sk-real-live-secret-key-12345" {
		t.Fatalf("persisted key was overwritten: got %s, want sk-real-live-secret-key-12345", persisted.Auth.Key)
	}

	// 6. PUT update with new actual secret -> secret is updated
	updatePayload.Auth.Key = "sk-brand-new-secret-9999"
	res = makeRequest(t, engine, http.MethodPut, "/admin/api/providers/openai-prod", updatePayload, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("PUT new key = %d: %s", res.Code, res.Body.String())
	}
	persisted, found, err = p.getProviderV2("openai-prod")
	if err != nil || !found {
		t.Fatalf("getProviderV2 failed: found=%v, err=%v", found, err)
	}
	if persisted.Auth.Key != "sk-brand-new-secret-9999" {
		t.Fatalf("persisted key was not updated: %s", persisted.Auth.Key)
	}

	// 7. DELETE /admin/api/providers/:id
	res = makeRequest(t, engine, http.MethodDelete, "/admin/api/providers/openai-prod", nil, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("DELETE = %d: %s", res.Code, res.Body.String())
	}
	var delResp map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &delResp); err != nil {
		t.Fatalf("unmarshal delete response: %v", err)
	}
	if delResp["ok"] != true || delResp["deleted"] != "openai-prod" {
		t.Fatalf("unexpected delete response: %v", delResp)
	}

	// Verify it's gone
	res = makeRequest(t, engine, http.MethodGet, "/admin/api/providers/openai-prod", nil, nil)
	if res.Code != http.StatusNotFound {
		t.Fatalf("GET deleted provider = %d, want 404", res.Code)
	}
}

func TestAdminAPI_ValidationErrorsFieldAddressable(t *testing.T) {
	p := setupTestPrefs(t)
	oldPrefs := prefs
	prefs = p
	t.Cleanup(func() { prefs = oldPrefs })

	engine := testAdminEngine(t, p, "", 0)

	// 1. Missing ID
	badProvider := gatewayprovider.Provider{
		Name:     "Missing ID",
		BaseURL:  "https://api.openai.com/v1",
		Protocol: ir.ProtocolOpenAIChat,
		Auth:     gatewayprovider.Auth{Scheme: gatewayprovider.AuthNone},
		Models:   []gatewayprovider.ModelMapping{{Alias: "a", UpstreamModel: "m"}},
	}
	res := makeRequest(t, engine, http.MethodPost, "/admin/api/providers", badProvider, nil)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("missing ID = %d, want 400", res.Code)
	}
	var errResp struct {
		Error       string            `json:"error"`
		Fields      map[string]string `json:"fields"`
		FieldErrors []struct {
			Field   string `json:"field"`
			Problem string `json:"problem"`
		} `json:"field_errors"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("unmarshal error response: %v", err)
	}
	if errResp.Fields["id"] == "" && len(errResp.FieldErrors) == 0 {
		t.Fatalf("expected field-addressable error for 'id', got: %v", errResp)
	}

	// 2. SSRF / Metadata base_url rejected
	badProvider.ID = "test-ssrf"
	badProvider.BaseURL = "http://169.254.169.254/v1"
	res = makeRequest(t, engine, http.MethodPost, "/admin/api/providers", badProvider, nil)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("SSRF base_url = %d, want 400", res.Code)
	}
	if err := json.Unmarshal(res.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("unmarshal error response: %v", err)
	}
	if errResp.Fields["base_url"] == "" {
		t.Fatalf("expected field-addressable error for 'base_url', got: %v", errResp)
	}

	// 3. Invalid scheme
	badProvider.BaseURL = "ftp://api.example.com/v1"
	res = makeRequest(t, engine, http.MethodPost, "/admin/api/providers", badProvider, nil)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("ftp base_url = %d, want 400", res.Code)
	}
	if err := json.Unmarshal(res.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("unmarshal error response: %v", err)
	}
	if errResp.Fields["base_url"] == "" {
		t.Fatalf("expected field-addressable error for 'base_url', got: %v", errResp)
	}

	// 4. Invalid protocol
	badProvider.BaseURL = "https://api.example.com/v1"
	badProvider.Protocol = "invalid-proto"
	res = makeRequest(t, engine, http.MethodPost, "/admin/api/providers", badProvider, nil)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("invalid protocol = %d, want 400", res.Code)
	}
	if err := json.Unmarshal(res.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("unmarshal error response: %v", err)
	}
	if errResp.Fields["protocol"] == "" {
		t.Fatalf("expected field-addressable error for 'protocol', got: %v", errResp)
	}

	// 5. Duplicate ID on create
	valid := gatewayprovider.Provider{
		ID:       "p-first",
		Name:     "First",
		BaseURL:  "https://api.openai.com/v1",
		Protocol: ir.ProtocolOpenAIChat,
		Auth:     gatewayprovider.Auth{Scheme: gatewayprovider.AuthNone},
		Models:   []gatewayprovider.ModelMapping{{Alias: "first-model", UpstreamModel: "m1"}},
		Enabled:  true,
	}
	res = makeRequest(t, engine, http.MethodPost, "/admin/api/providers", valid, nil)
	if res.Code != http.StatusCreated {
		t.Fatalf("POST valid = %d", res.Code)
	}
	// Post again with same ID
	res = makeRequest(t, engine, http.MethodPost, "/admin/api/providers", valid, nil)
	if res.Code != http.StatusConflict {
		t.Fatalf("POST duplicate ID = %d, want 409", res.Code)
	}
	if err := json.Unmarshal(res.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("unmarshal error response: %v", err)
	}
	if errResp.Fields["id"] == "" {
		t.Fatalf("expected field error on 'id' for duplicate, got: %v", errResp)
	}

	// 6. PUT with ID mismatch in body vs path
	valid.ID = "p-mismatch"
	res = makeRequest(t, engine, http.MethodPut, "/admin/api/providers/p-first", valid, nil)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("PUT mismatch ID = %d, want 400", res.Code)
	}

	// 7. PUT / DELETE non-existent
	res = makeRequest(t, engine, http.MethodPut, "/admin/api/providers/does-not-exist", valid, nil)
	if res.Code != http.StatusNotFound {
		t.Fatalf("PUT non-existent = %d, want 404", res.Code)
	}
	res = makeRequest(t, engine, http.MethodDelete, "/admin/api/providers/does-not-exist", nil, nil)
	if res.Code != http.StatusNotFound {
		t.Fatalf("DELETE non-existent = %d, want 404", res.Code)
	}
}

func TestAdminAPI_TestConnectionDistinguishesOutcomes(t *testing.T) {
	p := setupTestPrefs(t)
	oldPrefs := prefs
	prefs = p
	t.Cleanup(func() { prefs = oldPrefs })

	var mode string
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode {
		case "success-models":
			if r.URL.Path == "/v1/models" || r.URL.Path == "/models" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, err := w.Write([]byte(`{"data":[{"id":"gpt-4o"}]}`))
				if err != nil {
					t.Errorf("write response: %v", err)
				}
				return
			}
			http.NotFound(w, r)
		case "auth-401":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, err := w.Write([]byte(`{"error":{"message":"Invalid API key provided: sk-secret-abc"}}`))
			if err != nil {
				t.Errorf("write response: %v", err)
			}
		case "auth-403":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, err := w.Write([]byte(`{"error":"Forbidden access"}`))
			if err != nil {
				t.Errorf("write response: %v", err)
			}
		case "fallback-chat":
			if r.URL.Path == "/v1/models" || r.URL.Path == "/models" {
				http.NotFound(w, r)
				return
			}
			if r.URL.Path == "/v1/chat/completions" || r.URL.Path == "/chat/completions" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, err := w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
				if err != nil {
					t.Errorf("write response: %v", err)
				}
				return
			}
			http.NotFound(w, r)
		case "upstream-error":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, err := w.Write([]byte(`{"error":"internal server failure"}`))
			if err != nil {
				t.Errorf("write response: %v", err)
			}
		}
	}))
	defer upstreamServer.Close()

	engine := testAdminEngine(t, p, "", 0)

	// Save a test provider pointing to upstreamServer
	testProv := gatewayprovider.Provider{
		ID:       "test-prov",
		Name:     "Test Upstream",
		BaseURL:  upstreamServer.URL + "/v1",
		Protocol: ir.ProtocolOpenAIChat,
		Auth: gatewayprovider.Auth{
			Scheme: gatewayprovider.AuthBearer,
			Key:    "sk-secret-abc",
		},
		Models: []gatewayprovider.ModelMapping{
			{Alias: "gpt-4o", UpstreamModel: "gpt-4o"},
		},
		Enabled: true,
	}
	if err := p.upsertProviderV2(&testProv); err != nil {
		t.Fatalf("save provider: %v", err)
	}

	// 1. Success case
	mode = "success-models"
	res := makeRequest(t, engine, http.MethodPost, "/admin/api/providers/test-prov/test", nil, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("test success = %d: %s", res.Code, res.Body.String())
	}
	var testRes ConnectionTestResult
	if err := json.Unmarshal(res.Body.Bytes(), &testRes); err != nil {
		t.Fatalf("unmarshal test result: %v", err)
	}
	if !testRes.OK || testRes.Status != "success" || testRes.StatusCode != http.StatusOK {
		t.Fatalf("expected success, got: %+v", testRes)
	}

	// 2. Auth failure (401)
	mode = "auth-401"
	res = makeRequest(t, engine, http.MethodPost, "/admin/api/providers/test-prov/test", nil, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("test 401 = %d", res.Code)
	}
	if err := json.Unmarshal(res.Body.Bytes(), &testRes); err != nil {
		t.Fatalf("unmarshal test result: %v", err)
	}
	if testRes.OK || testRes.Status != "auth_failure" || testRes.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected auth_failure with 401, got: %+v", testRes)
	}
	// Check secret was redacted in error message!
	if strings.Contains(testRes.Error, "sk-secret-abc") {
		t.Fatalf("secret leaked in error: %s", testRes.Error)
	}

	// 3. Auth failure (403)
	mode = "auth-403"
	res = makeRequest(t, engine, http.MethodPost, "/admin/api/providers/test-prov/test", nil, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("test 403 = %d", res.Code)
	}
	if err := json.Unmarshal(res.Body.Bytes(), &testRes); err != nil {
		t.Fatalf("unmarshal test result: %v", err)
	}
	if testRes.OK || testRes.Status != "auth_failure" || testRes.StatusCode != http.StatusForbidden {
		t.Fatalf("expected auth_failure with 403, got: %+v", testRes)
	}

	// 4. Fallback to 1-token /chat/completions when /models is 404
	mode = "fallback-chat"
	res = makeRequest(t, engine, http.MethodPost, "/admin/api/providers/test-prov/test", nil, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("test fallback = %d", res.Code)
	}
	if err := json.Unmarshal(res.Body.Bytes(), &testRes); err != nil {
		t.Fatalf("unmarshal test result: %v", err)
	}
	if !testRes.OK || testRes.Status != "success" || testRes.StatusCode != http.StatusOK {
		t.Fatalf("expected fallback success, got: %+v", testRes)
	}

	// 5. DNS / connect failure
	dnsFailProv := testProv
	dnsFailProv.BaseURL = "http://non-existent-subdomain-12345.invalid:8080/v1"
	if err := p.upsertProviderV2(&dnsFailProv); err != nil {
		t.Fatalf("upsert dns fail provider: %v", err)
	}
	res = makeRequest(t, engine, http.MethodPost, "/admin/api/providers/test-prov/test", nil, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("test dns fail = %d", res.Code)
	}
	if err := json.Unmarshal(res.Body.Bytes(), &testRes); err != nil {
		t.Fatalf("unmarshal test result: %v", err)
	}
	if testRes.OK || testRes.Status != "connect_failure" {
		t.Fatalf("expected connect_failure, got: %+v", testRes)
	}

	// 6. Direct test endpoint `POST /admin/api/providers/test` with payload
	mode = "success-models"
	directPayload := gatewayprovider.Provider{
		Name:     "Direct Test",
		BaseURL:  upstreamServer.URL + "/v1",
		Protocol: ir.ProtocolOpenAIChat,
		Auth: gatewayprovider.Auth{
			Scheme: gatewayprovider.AuthBearer,
			Key:    "sk-direct-test",
		},
		Models: []gatewayprovider.ModelMapping{
			{Alias: "gpt-4o", UpstreamModel: "gpt-4o"},
		},
	}
	res = makeRequest(t, engine, http.MethodPost, "/admin/api/providers/test", directPayload, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("POST /admin/api/providers/test = %d", res.Code)
	}
	if err := json.Unmarshal(res.Body.Bytes(), &testRes); err != nil {
		t.Fatalf("unmarshal test result: %v", err)
	}
	if !testRes.OK || testRes.Status != "success" {
		t.Fatalf("expected direct test success, got: %+v", testRes)
	}

	// 7. Non-existent provider test
	res = makeRequest(t, engine, http.MethodPost, "/admin/api/providers/does-not-exist/test", nil, nil)
	if res.Code != http.StatusNotFound {
		t.Fatalf("test non-existent = %d, want 404", res.Code)
	}
}

func TestAdminAPI_AnthropicAndResponsesTestConnection(t *testing.T) {
	p := setupTestPrefs(t)
	oldPrefs := prefs
	prefs = p
	t.Cleanup(func() { prefs = oldPrefs })

	var lastVersion string
	var lastKey string
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastVersion = r.Header.Get("anthropic-version")
		lastKey = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, err := w.Write([]byte(`{"id":"msg_123","type":"message"}`))
		if err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	defer upstreamServer.Close()

	engine := testAdminEngine(t, p, "", 0)

	// Test Anthropic Messages protocol
	anthropicProv := gatewayprovider.Provider{
		ID:       "claude-prov",
		Name:     "Anthropic Upstream",
		BaseURL:  upstreamServer.URL + "/v1",
		Protocol: ir.ProtocolAnthropicMessages,
		Auth: gatewayprovider.Auth{
			Scheme: gatewayprovider.AuthAPIKey,
			Key:    "sk-ant-test-key",
		},
		Models: []gatewayprovider.ModelMapping{
			{Alias: "claude-3-5-sonnet", UpstreamModel: "claude-3-5-sonnet-20241022"},
		},
		Enabled: true,
	}
	if err := p.upsertProviderV2(&anthropicProv); err != nil {
		t.Fatalf("save anthropic provider: %v", err)
	}

	res := makeRequest(t, engine, http.MethodPost, "/admin/api/providers/claude-prov/test", nil, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("anthropic test = %d: %s", res.Code, res.Body.String())
	}
	var testRes ConnectionTestResult
	if err := json.Unmarshal(res.Body.Bytes(), &testRes); err != nil {
		t.Fatalf("unmarshal test result: %v", err)
	}
	if !testRes.OK || testRes.Status != "success" {
		t.Fatalf("expected anthropic success, got: %+v", testRes)
	}
	if lastVersion != "2023-06-01" {
		t.Fatalf("expected anthropic-version 2023-06-01, got %q", lastVersion)
	}
	if lastKey != "sk-ant-test-key" {
		t.Fatalf("expected x-api-key sk-ant-test-key, got %q", lastKey)
	}

	// Test OpenAI Responses protocol
	responsesProv := gatewayprovider.Provider{
		ID:       "responses-prov",
		Name:     "Responses Upstream",
		BaseURL:  upstreamServer.URL + "/v1",
		Protocol: ir.ProtocolOpenAIResponses,
		Auth: gatewayprovider.Auth{
			Scheme: gatewayprovider.AuthNone,
		},
		Models: []gatewayprovider.ModelMapping{
			{Alias: "resp-model", UpstreamModel: "resp-model"},
		},
		Enabled: true,
	}
	if err := p.upsertProviderV2(&responsesProv); err != nil {
		t.Fatalf("save responses provider: %v", err)
	}

	res = makeRequest(t, engine, http.MethodPost, "/admin/api/providers/responses-prov/test", nil, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("responses test = %d", res.Code)
	}
	if err := json.Unmarshal(res.Body.Bytes(), &testRes); err != nil {
		t.Fatalf("unmarshal test result: %v", err)
	}
	if !testRes.OK || testRes.Status != "success" {
		t.Fatalf("expected responses success, got: %+v", testRes)
	}
}

func TestAdminAPI_AuthAndBodyLimits(t *testing.T) {
	p := setupTestPrefs(t)
	oldPrefs := prefs
	prefs = p
	t.Cleanup(func() { prefs = oldPrefs })

	// Engine with admin token and 256 bytes body limit
	const token = "super-secret-admin-token"
	engine := testAdminEngine(t, p, token, 256)

	// 1. Without auth header -> 401
	res := makeRequest(t, engine, http.MethodGet, "/admin/api/providers", nil, nil)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("GET without auth = %d, want 401", res.Code)
	}

	// 2. With wrong auth header -> 401
	res = makeRequest(t, engine, http.MethodGet, "/admin/api/providers", nil, map[string]string{
		"Authorization": "Bearer wrong-token",
	})
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("GET with wrong auth = %d, want 401", res.Code)
	}

	// 3. With valid Bearer auth -> 200
	res = makeRequest(t, engine, http.MethodGet, "/admin/api/providers", nil, map[string]string{
		"Authorization": "Bearer " + token,
	})
	if res.Code != http.StatusOK {
		t.Fatalf("GET with valid Bearer = %d, want 200", res.Code)
	}

	// 4. With valid X-Admin-Token header -> 200
	res = makeRequest(t, engine, http.MethodGet, "/admin/api/providers", nil, map[string]string{
		"X-Admin-Token": token,
	})
	if res.Code != http.StatusOK {
		t.Fatalf("GET with X-Admin-Token = %d, want 200", res.Code)
	}

	// 5. Body limit enforcement (> 256 bytes) -> 413
	largeBody := strings.Repeat("A", 1024)
	res = makeRequest(t, engine, http.MethodPost, "/admin/api/providers", `{"data":"`+largeBody+`"}`, map[string]string{
		"Authorization": "Bearer " + token,
	})
	if res.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("large body = %d, want 413", res.Code)
	}
}

func TestAdminAPI_LegacyEndpointRemainsWorking(t *testing.T) {
	p := setupTestPrefs(t)
	oldPrefs := prefs
	prefs = p
	t.Cleanup(func() { prefs = oldPrefs })

	engine := testAdminEngine(t, p, "", 0)

	legacyList := []customProvider{
		{
			Name:    "legacy-provider",
			BaseURL: "https://api.openai.com/v1",
			APIKey:  "sk-legacy-key",
			Model:   "gpt-4",
			Alias:   "legacy-gpt-4",
		},
	}
	res := makeRequest(t, engine, http.MethodPost, "/admin/providers", legacyList, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("POST /admin/providers = %d: %s", res.Code, res.Body.String())
	}

	// Verify legacy provider is also queryable through /admin/api/providers (migrated view)
	res = makeRequest(t, engine, http.MethodGet, "/admin/api/providers", nil, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("GET /admin/api/providers = %d", res.Code)
	}
	var providers []gatewayprovider.Provider
	if err := json.Unmarshal(res.Body.Bytes(), &providers); err != nil {
		t.Fatalf("unmarshal providers: %v", err)
	}
	if len(providers) == 0 {
		t.Fatalf("expected legacy provider to appear in v2 view, got 0")
	}
}

func TestAdminAPI_ManualDispatchRoutes(t *testing.T) {
	p := setupTestPrefs(t)
	oldPrefs := prefs
	prefs = p
	t.Cleanup(func() { prefs = oldPrefs })

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	// Test mounting via adminRoutes directly (using the fallback check in adminRoutes)
	engine.Any("/*path", func(c *gin.Context) {
		adminRoutes(c, nil)
	})

	res := makeRequest(t, engine, http.MethodGet, "/admin/api/providers", nil, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("GET through adminRoutes = %d, want 200", res.Code)
	}
}

func TestAdminAPI_SensitiveHeaderPreservation(t *testing.T) {
	p := setupTestPrefs(t)
	oldPrefs := prefs
	prefs = p
	t.Cleanup(func() { prefs = oldPrefs })

	var mu sync.Mutex
	var lastHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		lastHeaders = r.Header.Clone()
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte(`{"data":[]}`)); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	defer server.Close()

	engine := testAdminEngine(t, p, "", 0)

	// 1. Create provider with sensitive and normal headers
	initial := gatewayprovider.Provider{
		ID:       "sensitive-hdr-prov",
		Name:     "Sensitive Headers Provider",
		BaseURL:  server.URL + "/v1",
		Protocol: ir.ProtocolOpenAIChat,
		Auth: gatewayprovider.Auth{
			Scheme: gatewayprovider.AuthNone,
		},
		Headers: map[string]string{
			"Api-Key":       "real-api-key-12345",
			"ApiKey":        "real-apikey-67890",
			"Custom-Config": "initial-value",
		},
		Models: []gatewayprovider.ModelMapping{
			{Alias: "gpt-4o", UpstreamModel: "gpt-4o"},
		},
		Enabled: true,
	}

	res := makeRequest(t, engine, http.MethodPost, "/admin/api/providers", initial, nil)
	if res.Code != http.StatusCreated {
		t.Fatalf("POST /admin/api/providers = %d: %s", res.Code, res.Body.String())
	}

	// 2. GET provider -> sensitive headers must be redacted
	res = makeRequest(t, engine, http.MethodGet, "/admin/api/providers/sensitive-hdr-prov", nil, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("GET /admin/api/providers/sensitive-hdr-prov = %d", res.Code)
	}
	var fetched gatewayprovider.Provider
	if err := json.Unmarshal(res.Body.Bytes(), &fetched); err != nil {
		t.Fatalf("unmarshal fetched: %v", err)
	}
	if fetched.Headers["Api-Key"] != "[REDACTED]" {
		t.Fatalf("Api-Key not redacted in GET: %s", fetched.Headers["Api-Key"])
	}
	if fetched.Headers["Apikey"] != "[REDACTED]" {
		t.Fatalf("Apikey not redacted in GET: %s", fetched.Headers["Apikey"])
	}
	if fetched.Headers["Custom-Config"] != "initial-value" {
		t.Fatalf("Custom-Config altered: %s", fetched.Headers["Custom-Config"])
	}

	// 3. PUT with redacted headers and case variation -> must preserve original secret headers
	updatePayload := fetched
	updatePayload.Headers["Custom-Config"] = "updated-value"
	// Change casing for api-key to test case-insensitive matching
	delete(updatePayload.Headers, "Api-Key")
	updatePayload.Headers["api-key"] = "[REDACTED]"
	delete(updatePayload.Headers, "Apikey")
	updatePayload.Headers["apikey"] = "[REDACTED]"

	res = makeRequest(t, engine, http.MethodPut, "/admin/api/providers/sensitive-hdr-prov", updatePayload, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("PUT /admin/api/providers/sensitive-hdr-prov = %d: %s", res.Code, res.Body.String())
	}

	persisted, found, err := p.getProviderV2("sensitive-hdr-prov")
	if err != nil || !found {
		t.Fatalf("getProviderV2: found=%v err=%v", found, err)
	}
	if persisted.Headers["Api-Key"] != "real-api-key-12345" && persisted.Headers["api-key"] != "real-api-key-12345" {
		t.Fatalf("persisted Api-Key lost: %+v", persisted.Headers)
	}
	if persisted.Headers["Apikey"] != "real-apikey-67890" && persisted.Headers["apikey"] != "real-apikey-67890" {
		t.Fatalf("persisted Apikey lost: %+v", persisted.Headers)
	}
	if persisted.Headers["Custom-Config"] != "updated-value" {
		t.Fatalf("persisted Custom-Config not updated: %s", persisted.Headers["Custom-Config"])
	}

	// 4. POST /admin/api/providers/:id/test with overlay containing redacted headers
	testOverlay := map[string]any{
		"headers": map[string]string{
			"Api-Key":     "[REDACTED]",
			"apikey":      "[REDACTED]",
			"Overlay-Hdr": "overlay-val",
		},
	}
	res = makeRequest(t, engine, http.MethodPost, "/admin/api/providers/sensitive-hdr-prov/test", testOverlay, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("POST /sensitive-hdr-prov/test = %d: %s", res.Code, res.Body.String())
	}
	var testRes ConnectionTestResult
	if err := json.Unmarshal(res.Body.Bytes(), &testRes); err != nil {
		t.Fatalf("unmarshal test result: %v", err)
	}
	if !testRes.OK || testRes.Status != "success" {
		t.Fatalf("expected test success, got: %+v", testRes)
	}

	mu.Lock()
	defer mu.Unlock()
	if lastHeaders.Get("Api-Key") != "real-api-key-12345" {
		t.Fatalf("upstream received wrong Api-Key: %q", lastHeaders.Get("Api-Key"))
	}
	if lastHeaders.Get("Apikey") != "real-apikey-67890" {
		t.Fatalf("upstream received wrong Apikey: %q", lastHeaders.Get("Apikey"))
	}
	if lastHeaders.Get("Overlay-Hdr") != "overlay-val" {
		t.Fatalf("upstream received wrong Overlay-Hdr: %q", lastHeaders.Get("Overlay-Hdr"))
	}

	// 5. Test preserveHeaders directly with all sensitive header names and casing
	existingH := map[string]string{
		"Authorization":       "Bearer secret-auth",
		"Proxy-Authorization": "Basic proxy-auth",
		"X-Api-Key":           "x-key-val",
		"Api-Key":             "api-key-val",
		"ApiKey":              "apikey-val",
		"Non-Sensitive":       "orig-val",
	}
	incomingH := map[string]string{
		"authorization":       "[REDACTED]",
		"proxy-authorization": "sk-...1234",
		"x-api-key":           "[REDACTED]",
		"api-key":             "[REDACTED]",
		"apikey":              "[REDACTED]",
		"Non-Sensitive":       "new-val",
	}
	merged := preserveHeaders(incomingH, existingH)
	if merged["authorization"] != "Bearer secret-auth" {
		t.Fatalf("preserveHeaders failed for authorization: %q", merged["authorization"])
	}
	if merged["proxy-authorization"] != "Basic proxy-auth" {
		t.Fatalf("preserveHeaders failed for proxy-authorization: %q", merged["proxy-authorization"])
	}
	if merged["x-api-key"] != "x-key-val" {
		t.Fatalf("preserveHeaders failed for x-api-key: %q", merged["x-api-key"])
	}
	if merged["api-key"] != "api-key-val" {
		t.Fatalf("preserveHeaders failed for api-key: %q", merged["api-key"])
	}
	if merged["apikey"] != "apikey-val" {
		t.Fatalf("preserveHeaders failed for apikey: %q", merged["apikey"])
	}
	if merged["Non-Sensitive"] != "new-val" {
		t.Fatalf("preserveHeaders overwrote non-sensitive header: %q", merged["Non-Sensitive"])
	}
}

func TestAdminAPI_UnsetKeyEnvAuthFailure(t *testing.T) {
	p := setupTestPrefs(t)
	oldPrefs := prefs
	prefs = p
	t.Cleanup(func() { prefs = oldPrefs })

	const testEnvVar = "GNVIPI_TEST_UNSET_KEY_ENV_XYZ"
	if err := os.Unsetenv(testEnvVar); err != nil {
		t.Fatalf("unsetenv %s: %v", testEnvVar, err)
	}

	engine := testAdminEngine(t, p, "", 0)

	prov := gatewayprovider.Provider{
		ID:       "unset-keyenv-prov",
		Name:     "Unset KeyEnv Provider",
		BaseURL:  "https://api.openai.com/v1",
		Protocol: ir.ProtocolOpenAIChat,
		Auth: gatewayprovider.Auth{
			Scheme: gatewayprovider.AuthBearer,
			KeyEnv: testEnvVar,
		},
		Models: []gatewayprovider.ModelMapping{
			{Alias: "gpt-4o", UpstreamModel: "gpt-4o"},
		},
		Enabled: true,
	}

	if err := p.upsertProviderV2(&prov); err != nil {
		t.Fatalf("upsert provider: %v", err)
	}

	// 1. POST /admin/api/providers/:id/test should return auth_failure immediately
	res := makeRequest(t, engine, http.MethodPost, "/admin/api/providers/unset-keyenv-prov/test", nil, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("test returned %d, want 200", res.Code)
	}
	var testRes ConnectionTestResult
	if err := json.Unmarshal(res.Body.Bytes(), &testRes); err != nil {
		t.Fatalf("unmarshal test result: %v", err)
	}
	if testRes.OK {
		t.Fatalf("expected test failure, got ok=true")
	}
	if testRes.Status != "auth_failure" {
		t.Fatalf("expected status auth_failure, got %q", testRes.Status)
	}
	expectedErr := fmt.Sprintf("environment variable %q is not set", testEnvVar)
	if testRes.Error != expectedErr {
		t.Fatalf("expected error %q, got %q", expectedErr, testRes.Error)
	}

	// 2. Direct POST /admin/api/providers/test should also return auth_failure immediately
	res = makeRequest(t, engine, http.MethodPost, "/admin/api/providers/test", prov, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("direct test returned %d, want 200", res.Code)
	}
	var directRes ConnectionTestResult
	if err := json.Unmarshal(res.Body.Bytes(), &directRes); err != nil {
		t.Fatalf("unmarshal direct test result: %v", err)
	}
	if directRes.OK || directRes.Status != "auth_failure" || directRes.Error != expectedErr {
		t.Fatalf("expected direct test auth_failure, got: %+v", directRes)
	}
}

func TestAdminAPI_RedirectAndSSRFGuards(t *testing.T) {
	p := setupTestPrefs(t)
	oldPrefs := prefs
	prefs = p
	t.Cleanup(func() { prefs = oldPrefs })

	// Server that redirects to link-local/metadata address
	redirectServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/cross") {
			http.Redirect(w, r, "https://api.openai.com/v1/models", http.StatusFound)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v1/loop") {
			http.Redirect(w, r, r.URL.String(), http.StatusFound)
			return
		}
		if r.URL.Path == "/v1/models" {
			http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
			return
		}
		http.NotFound(w, r)
	}))
	defer redirectServer.Close()

	engine := testAdminEngine(t, p, "", 0)

	// 1. Connection test redirecting to SSRF target (169.254.169.254) must fail
	prov := gatewayprovider.Provider{
		ID:       "ssrf-redirect-prov",
		Name:     "SSRF Redirect Provider",
		BaseURL:  redirectServer.URL + "/v1",
		Protocol: ir.ProtocolOpenAIChat,
		Auth:     gatewayprovider.Auth{Scheme: gatewayprovider.AuthNone},
		Models:   []gatewayprovider.ModelMapping{{Alias: "gpt-4o", UpstreamModel: "gpt-4o"}},
		Enabled:  true,
	}

	if err := p.upsertProviderV2(&prov); err != nil {
		t.Fatalf("upsert provider: %v", err)
	}

	res := makeRequest(t, engine, http.MethodPost, "/admin/api/providers/ssrf-redirect-prov/test", nil, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("test returned %d, want 200", res.Code)
	}
	var testRes ConnectionTestResult
	if err := json.Unmarshal(res.Body.Bytes(), &testRes); err != nil {
		t.Fatalf("unmarshal test result: %v", err)
	}
	if testRes.OK || testRes.Status != "connect_failure" {
		t.Fatalf("expected connect_failure for SSRF redirect, got: %+v", testRes)
	}
	if !strings.Contains(testRes.Error, "metadata") && !strings.Contains(testRes.Error, "link-local") && !strings.Contains(testRes.Error, "redirect") {
		t.Fatalf("expected error mentioning metadata/link-local/redirect, got: %s", testRes.Error)
	}

	// 2. Redirect loop should be blocked after 10 redirects
	loopProv := prov
	loopProv.ID = "loop-prov"
	loopProv.BaseURL = redirectServer.URL + "/v1/loop"
	loopProv.Models = []gatewayprovider.ModelMapping{{Alias: "loop-model", UpstreamModel: "loop-model"}}
	if err := p.upsertProviderV2(&loopProv); err != nil {
		t.Fatalf("upsert loop provider: %v", err)
	}
	res = makeRequest(t, engine, http.MethodPost, "/admin/api/providers/loop-prov/test", nil, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("test returned %d, want 200", res.Code)
	}
	var loopRes ConnectionTestResult
	if err := json.Unmarshal(res.Body.Bytes(), &loopRes); err != nil {
		t.Fatalf("unmarshal loop test result: %v", err)
	}
	if loopRes.OK || loopRes.Status != "connect_failure" {
		t.Fatalf("expected connect_failure for redirect loop, got: %+v", loopRes)
	}
	if !strings.Contains(loopRes.Error, "too many upstream redirects") {
		t.Fatalf("expected too many upstream redirects, got: %s", loopRes.Error)
	}

	// 3. Cross-origin redirect should be blocked
	crossProv := prov
	crossProv.ID = "cross-prov"
	crossProv.BaseURL = redirectServer.URL + "/v1/cross"
	crossProv.Models = []gatewayprovider.ModelMapping{{Alias: "cross-model", UpstreamModel: "cross-model"}}
	if err := p.upsertProviderV2(&crossProv); err != nil {
		t.Fatalf("upsert cross provider: %v", err)
	}
	res = makeRequest(t, engine, http.MethodPost, "/admin/api/providers/cross-prov/test", nil, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("test returned %d, want 200", res.Code)
	}
	var crossRes ConnectionTestResult
	if err := json.Unmarshal(res.Body.Bytes(), &crossRes); err != nil {
		t.Fatalf("unmarshal cross test result: %v", err)
	}
	if crossRes.OK || crossRes.Status != "connect_failure" {
		t.Fatalf("expected connect_failure for cross-origin redirect, got: %+v", crossRes)
	}
	if !strings.Contains(crossRes.Error, "cross-origin upstream redirect is forbidden") {
		t.Fatalf("expected cross-origin redirect forbidden, got: %s", crossRes.Error)
	}

	// 4. Direct SSRF guard in testProviderConnection when DenyPrivate is enabled
	gatewayprovider.SetDefaultURLPolicy(gatewayprovider.URLPolicy{DenyPrivate: true})
	t.Cleanup(func() { gatewayprovider.SetDefaultURLPolicy(gatewayprovider.URLPolicy{}) })

	denyRes := testProviderConnection(t.Context(), &prov)
	if denyRes.OK || denyRes.Status != "connect_failure" {
		t.Fatalf("expected connect_failure with DenyPrivate, got: %+v", denyRes)
	}
	if !strings.Contains(denyRes.Error, "private/local") && !strings.Contains(denyRes.Error, "base_url") {
		t.Fatalf("expected error mentioning private/local address, got: %s", denyRes.Error)
	}
}

func TestAdminAPI_ProviderV2EditorFeaturesAndDirectTesting(t *testing.T) {
	p := setupTestPrefs(t)
	oldPrefs := prefs
	prefs = p
	t.Cleanup(func() { prefs = oldPrefs })

	engine := testAdminEngine(t, p, "", 0)

	// 1. Create provider with "api-key" scheme (alias for x-api-key) and advanced options
	provPayload := map[string]any{
		"id":       "v2-full-featured",
		"name":     "Full Featured V2",
		"base_url": "https://api.v2provider.example/v1",
		"protocol": "openai-chat",
		"auth": map[string]any{
			"scheme": "api-key", // alias tested here
			"key":    "sk-test-secret-key-12345",
		},
		"headers": map[string]string{
			"HTTP-Referer": "https://myapp.example",
			"X-Title":      "Gateway Admin",
		},
		"models": []map[string]any{
			{"alias": "fast-model", "upstream_model": "vendor/fast-v1"},
			{"alias": "backup-model", "upstream_model": "vendor/backup-v1", "hidden": true, "fallbacks": []string{"fallback-1"}},
		},
		"max_concurrency": 16,
		"timeouts": map[string]any{
			"connect": 10000000000, // 10s
			"idle":    30000000000, // 30s
		},
		"queue_timeout": 5000000000, // 5s
		"tls":           "chrome",
		"enabled":       true,
	}

	res := makeRequest(t, engine, http.MethodPost, "/admin/api/providers", provPayload, nil)
	if res.Code != http.StatusCreated {
		t.Fatalf("POST /admin/api/providers = %d, want 201: %s", res.Code, res.Body.String())
	}
	var created gatewayprovider.Provider
	if err := json.Unmarshal(res.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal created: %v", err)
	}
	if created.Auth.Scheme != gatewayprovider.AuthAPIKey {
		t.Fatalf("expected auth.scheme normalized to %q, got %q", gatewayprovider.AuthAPIKey, created.Auth.Scheme)
	}
	if created.MaxConcurrency != 16 {
		t.Fatalf("expected MaxConcurrency 16, got %d", created.MaxConcurrency)
	}
	if created.TLS != gatewayprovider.TLSChrome {
		t.Fatalf("expected TLS chrome, got %s", created.TLS)
	}

	// 2. Direct test connection endpoint for unsaved provider
	directPayload := map[string]any{
		"base_url": "https://api.v2provider.example/v1",
		"protocol": "openai-chat",
		"auth": map[string]any{
			"scheme": "api-key",
			"key":    "sk-direct-test-key",
		},
	}
	res = makeRequest(t, engine, http.MethodPost, "/admin/api/providers/test", directPayload, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("POST /admin/api/providers/test = %d, want 200: %s", res.Code, res.Body.String())
	}
	var directResult ConnectionTestResult
	if err := json.Unmarshal(res.Body.Bytes(), &directResult); err != nil {
		t.Fatalf("unmarshal directResult: %v", err)
	}
	if directResult.Status == "" {
		t.Fatalf("expected non-empty status in direct test result")
	}

	// 3. Test connection on existing provider with overlay
	overlayPayload := map[string]any{
		"auth": map[string]any{
			"scheme": "api-key",
			"key":    "sk-overlay-test-key",
		},
	}
	res = makeRequest(t, engine, http.MethodPost, "/admin/api/providers/v2-full-featured/test", overlayPayload, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("POST /admin/api/providers/v2-full-featured/test with overlay = %d: %s", res.Code, res.Body.String())
	}
}

func TestAdminAPI_TestConnectionOverlayAuthSwitching(t *testing.T) {
	p := setupTestPrefs(t)
	oldPrefs := prefs
	prefs = p
	t.Cleanup(func() { prefs = oldPrefs })

	engine := testAdminEngine(t, p, "", 0)

	// Create provider with literal key
	provPayload := map[string]any{
		"id":       "test-auth-switch",
		"name":     "Test Auth Switch",
		"base_url": "https://api.switchtest.example/v1",
		"protocol": "openai-chat",
		"auth": map[string]any{
			"scheme": "bearer",
			"key":    "sk-initial-secret-key",
		},
		"enabled": true,
	}
	res := makeRequest(t, engine, http.MethodPost, "/admin/api/providers", provPayload, nil)
	if res.Code != http.StatusCreated {
		t.Fatalf("POST /admin/api/providers = %d, want 201: %s", res.Code, res.Body.String())
	}

	// 1. Overlay changes scheme to "none" -> target.Auth.Key / KeyEnv / KeyRef must be cleared
	// It should NOT fail validation with "auth.key: must be empty when auth.scheme is none".
	overlayNone := map[string]any{
		"auth": map[string]any{
			"scheme": "none",
		},
	}
	res = makeRequest(t, engine, http.MethodPost, "/admin/api/providers/test-auth-switch/test", overlayNone, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("POST /admin/api/providers/test-auth-switch/test overlay scheme=none failed: %d, body: %s", res.Code, res.Body.String())
	}

	// 2. Overlay changes to KeyEnv -> target.Auth.Key must be cleared
	// It should NOT fail validation with "set either auth.key or auth.key_env, not both".
	t.Setenv("TEST_AUTH_SWITCH_ENV", "sk-from-env-12345")
	overlayKeyEnv := map[string]any{
		"auth": map[string]any{
			"scheme":  "bearer",
			"key_env": "TEST_AUTH_SWITCH_ENV",
		},
	}
	res = makeRequest(t, engine, http.MethodPost, "/admin/api/providers/test-auth-switch/test", overlayKeyEnv, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("POST /admin/api/providers/test-auth-switch/test overlay key_env failed: %d, body: %s", res.Code, res.Body.String())
	}

	// 3. Switch stored provider to use KeyEnv
	updatePayload := map[string]any{
		"id":       "test-auth-switch",
		"name":     "Test Auth Switch",
		"base_url": "https://api.switchtest.example/v1",
		"protocol": "openai-chat",
		"auth": map[string]any{
			"scheme":  "bearer",
			"key_env": "TEST_AUTH_SWITCH_ENV",
		},
		"enabled": true,
	}
	res = makeRequest(t, engine, http.MethodPut, "/admin/api/providers/test-auth-switch", updatePayload, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("PUT /admin/api/providers/test-auth-switch failed: %d, body: %s", res.Code, res.Body.String())
	}

	// 4. Overlay from KeyEnv provider with a new literal Key -> target.Auth.KeyEnv must be cleared
	// It should NOT fail validation with "set either auth.key or auth.key_env, not both".
	overlayKey := map[string]any{
		"auth": map[string]any{
			"scheme": "bearer",
			"key":    "sk-new-literal-secret",
		},
	}
	res = makeRequest(t, engine, http.MethodPost, "/admin/api/providers/test-auth-switch/test", overlayKey, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("POST /admin/api/providers/test-auth-switch/test overlay literal key failed: %d, body: %s", res.Code, res.Body.String())
	}

	// 5. Overlay with both Key and KeyEnv provided simultaneously -> validation must reject with 400
	overlayBoth := map[string]any{
		"auth": map[string]any{
			"scheme":  "bearer",
			"key":     "sk-literal",
			"key_env": "TEST_AUTH_SWITCH_ENV",
		},
	}
	res = makeRequest(t, engine, http.MethodPost, "/admin/api/providers/test-auth-switch/test", overlayBoth, nil)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request when overlay specifies both key and key_env, got %d", res.Code)
	}
}
