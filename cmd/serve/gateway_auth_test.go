package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// Removing the auth guard, accepting a conflicting credential, or stripping
// credentials in legacy mode must change the consumer-visible response.
func TestGatewayAuthCredentials(t *testing.T) {
	for _, tc := range []struct {
		name          string
		keys          string
		authorization []string
		apiKeys       []string
		wantStatus    int
		wantAuth      string
		wantAPIKey    string
	}{
		{name: "legacy missing", wantStatus: http.StatusOK},
		{name: "legacy provider headers unchanged", authorization: []string{"Bearer provider-secret"}, apiKeys: []string{"provider-secret"}, wantStatus: http.StatusOK, wantAuth: "Bearer provider-secret", wantAPIKey: "provider-secret"},
		{name: "empty key entries preserve legacy", keys: " , , ", authorization: []string{"Basic provider-secret"}, wantStatus: http.StatusOK, wantAuth: "Basic provider-secret"},
		{name: "configured missing", keys: "gateway-one", wantStatus: http.StatusUnauthorized},
		{name: "bearer correct", keys: "gateway-one", authorization: []string{"Bearer gateway-one"}, wantStatus: http.StatusOK},
		{name: "bearer scheme case insensitive", keys: "gateway-one", authorization: []string{"bEaReR gateway-one"}, wantStatus: http.StatusOK},
		{name: "bearer wrong", keys: "gateway-one", authorization: []string{"Bearer incorrect-client-secret"}, wantStatus: http.StatusUnauthorized},
		{name: "bearer empty", keys: "gateway-one", authorization: []string{"Bearer"}, wantStatus: http.StatusUnauthorized},
		{name: "bearer extra token", keys: "gateway-one", authorization: []string{"Bearer gateway-one extra"}, wantStatus: http.StatusUnauthorized},
		{name: "api key correct", keys: "gateway-one", apiKeys: []string{"gateway-one"}, wantStatus: http.StatusOK},
		{name: "api key wrong", keys: "gateway-one", apiKeys: []string{"incorrect-client-secret"}, wantStatus: http.StatusUnauthorized},
		{name: "api key empty", keys: "gateway-one", apiKeys: []string{""}, wantStatus: http.StatusUnauthorized},
		{name: "first rotated key", keys: " gateway-one, , gateway-two ", authorization: []string{"Bearer gateway-one"}, wantStatus: http.StatusOK},
		{name: "second rotated key", keys: " gateway-one, , gateway-two ", apiKeys: []string{"gateway-two"}, wantStatus: http.StatusOK},
		{name: "keys remain case sensitive", keys: "gateway-one", apiKeys: []string{"GATEWAY-ONE"}, wantStatus: http.StatusUnauthorized},
		{name: "both matching", keys: "gateway-one", authorization: []string{"Bearer gateway-one"}, apiKeys: []string{"gateway-one"}, wantStatus: http.StatusOK},
		{name: "wrong bearer cannot be rescued", keys: "gateway-one", authorization: []string{"Bearer incorrect-client-secret"}, apiKeys: []string{"gateway-one"}, wantStatus: http.StatusUnauthorized},
		{name: "wrong api key cannot be ignored", keys: "gateway-one", authorization: []string{"Bearer gateway-one"}, apiKeys: []string{"incorrect-client-secret"}, wantStatus: http.StatusUnauthorized},
		{name: "two valid but conflicting keys", keys: "gateway-one,gateway-two", authorization: []string{"Bearer gateway-one"}, apiKeys: []string{"gateway-two"}, wantStatus: http.StatusUnauthorized},
		{name: "malformed authorization cannot be rescued", keys: "gateway-one", authorization: []string{"Basic gateway-one"}, apiKeys: []string{"gateway-one"}, wantStatus: http.StatusUnauthorized},
		{name: "duplicate authorization rejected", keys: "gateway-one", authorization: []string{"Bearer gateway-one", "Bearer incorrect-client-secret"}, wantStatus: http.StatusUnauthorized},
		{name: "duplicate api keys rejected", keys: "gateway-one", apiKeys: []string{"gateway-one", "incorrect-client-secret"}, wantStatus: http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := gatewayAuthTestEngine(tc.keys)
			engine.POST("/v1/chat/completions", gatewayAuthConsumer)
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"nvidia/test","messages":[]}`))
			for _, value := range tc.authorization {
				req.Header.Add("Authorization", value)
			}
			for _, value := range tc.apiKeys {
				req.Header.Add("x-api-key", value)
			}
			req.Header.Set("anthropic-version", "2023-06-01")
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, req)
			if w.Code != tc.wantStatus {
				t.Fatalf("status=%d, want %d: %s", w.Code, tc.wantStatus, w.Body.String())
			}
			if tc.wantStatus == http.StatusOK {
				var got struct {
					Authorization string `json:"authorization"`
					APIKey        string `json:"api_key"`
					Version       string `json:"version"`
					Body          string `json:"body"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if got.Authorization != tc.wantAuth || got.APIKey != tc.wantAPIKey {
					t.Fatalf("consumer credentials=%q %q, want %q %q", got.Authorization, got.APIKey, tc.wantAuth, tc.wantAPIKey)
				}
				if got.Version != "2023-06-01" || got.Body != `{"model":"nvidia/test","messages":[]}` {
					t.Fatalf("unrelated header or body changed: %+v", got)
				}
			} else if strings.Contains(w.Body.String(), "incorrect-client-secret") || strings.Contains(w.Body.String(), "gateway-one") {
				t.Fatalf("auth error leaked a credential: %s", w.Body.String())
			}
		})
	}
}

// Route-family mistakes must not expose models, token counting, MCP, or the
// NVIDIA fallthrough while protecting unrelated admin/health routes by mistake.
func TestGatewayAuthRouteCoverage(t *testing.T) {
	for _, tc := range []struct {
		method    string
		path      string
		protected bool
	}{
		{http.MethodGet, "/v1/models", true},
		{http.MethodGet, "/v1/models/", true},
		{http.MethodPost, "/v1/chat/completions", true},
		{http.MethodPost, "/v1/messages", true},
		{http.MethodPost, "/v1/messages/count_tokens", true},
		{http.MethodPost, "/v1/responses", true},
		{http.MethodPost, "/v1/responses/compact", true},
		{http.MethodPost, "/backend-api/codex/responses", true},
		{http.MethodGet, "/backend-api/codex/responses", true},
		{http.MethodPost, "/backend-api/codex/responses/compact", true},
		{http.MethodPost, "/backend-api/codex/alpha/search", true},
		{http.MethodPost, "/openai/v1/videos", true},
		{http.MethodGet, "/v1beta/models", true},
		{http.MethodPost, "/v1beta/interactions", true},
		{http.MethodPost, "/v1beta/models/test:generateContent", true},
		{http.MethodGet, "/v1/api/hello", true},
		{http.MethodHead, "/v1/api/hello", true},
		{http.MethodGet, "/v1", true},
		{http.MethodGet, "/mcp", true},
		{http.MethodPost, "/mcp", true},
		{http.MethodDelete, "/mcp", true},
		{http.MethodPost, "/mcp/", true},
		{http.MethodGet, "/healthz", false},
		{http.MethodHead, "/healthz", false},
		{http.MethodGet, "/admin", false},
		{http.MethodGet, "/api/hello", false},
		{http.MethodGet, "/v10/models", false},
		{http.MethodGet, "/v1betaish/models", false},
		{http.MethodPost, "/backend-api/codexish/responses", false},
		{http.MethodGet, "/openai/v10/videos", false},
		{http.MethodGet, "/mcproxy", false},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			engine := gatewayAuthTestEngine("gateway-one")
			engine.Handle(tc.method, tc.path, func(c *gin.Context) {
				c.Header("X-Consumer-Auth", c.GetHeader("Authorization"))
				c.Status(http.StatusNoContent)
			})
			for _, credential := range []string{"", "incorrect-client-secret", "gateway-one"} {
				req := httptest.NewRequestWithContext(t.Context(), tc.method, tc.path, nil)
				wantAuth := ""
				if credential != "" {
					wantAuth = "Bearer " + credential
					req.Header.Set("Authorization", wantAuth)
				}
				w := httptest.NewRecorder()
				engine.ServeHTTP(w, req)
				want := http.StatusNoContent
				if tc.protected && credential != "gateway-one" {
					want = http.StatusUnauthorized
				}
				if w.Code != want {
					t.Fatalf("credential=%q: status=%d, want %d: %s", credential, w.Code, want, w.Body.String())
				}
				if !tc.protected && w.Header().Get("X-Consumer-Auth") != wantAuth {
					t.Fatalf("unrelated route credential stripped")
				}
			}
		})
	}
}

func TestGatewayAuthProtocolErrors(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens", "/v1/chat/completions", "/v1/responses", "/v1/models", "/mcp"} {
		t.Run(path, func(t *testing.T) {
			engine := gatewayAuthTestEngine("gateway-one")
			engine.POST(path, gatewayAuthConsumer)
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader(`{"jsonrpc":"2.0","id":7,"method":"ping"}`)))
			if w.Code != http.StatusUnauthorized || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
				t.Fatalf("response=%d %s", w.Code, w.Body.String())
			}
			if !strings.HasPrefix(w.Header().Get("WWW-Authenticate"), "Bearer ") {
				t.Fatalf("missing Bearer challenge: %v", w.Header())
			}
			var got map[string]json.RawMessage
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			var detail struct {
				Type    string `json:"type"`
				Message string `json:"message"`
				Code    int    `json:"code"`
			}
			if err := json.Unmarshal(got["error"], &detail); err != nil {
				t.Fatalf("invalid error object: %s", w.Body.String())
			}
			if detail.Message == "" {
				t.Fatalf("missing error message: %s", w.Body.String())
			}
			if path == "/mcp" {
				if string(got["jsonrpc"]) != `"2.0"` || string(got["id"]) != "null" || detail.Code < -32099 || detail.Code > -32000 {
					t.Fatalf("invalid JSON-RPC authentication error: %s", w.Body.String())
				}
				return
			}
			if detail.Type != "authentication_error" {
				t.Fatalf("wrong protocol error type: %s", w.Body.String())
			}
			if strings.HasPrefix(path, "/v1/messages") {
				if string(got["type"]) != `"error"` {
					t.Fatalf("missing Anthropic envelope: %s", w.Body.String())
				}
			} else {
				if _, ok := got["type"]; ok {
					t.Fatalf("Anthropic envelope on OpenAI route: %s", w.Body.String())
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(got["error"], &fields); err != nil {
					t.Fatal(err)
				}
				if string(fields["param"]) != "null" {
					t.Fatalf("missing OpenAI error param: %s", w.Body.String())
				}
			}
		})
	}
}

// The NVIDIA model deliberately has no custom provider, so it reaches the
// fake CLIProxyAPI consumer only after passing through the actual middleware.
func TestGatewayAuthNvidiaFallthrough(t *testing.T) {
	for _, header := range []string{"Authorization", "x-api-key"} {
		for _, credential := range []string{"", "incorrect-client-secret", "gateway-one"} {
			t.Run(header+"/"+credential, func(t *testing.T) {
				engine := gatewayAuthTestEngine("gateway-one")
				prefs := &modelPrefs{}
				engine.Use(anthropicResponsesPassthrough(prefs), sanitizeClaudeToolResults(), customOpenAIResponsesPassthrough(prefs), customOpenAICompatPassthrough(prefs), messagesOnlyPassthrough(prefs))
				engine.POST("/v1/chat/completions", gatewayAuthConsumer)
				req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"meta/llama-3.3-70b-instruct","messages":[]}`))
				if credential != "" {
					value := credential
					if header == "Authorization" {
						value = "Bearer " + credential
					}
					req.Header.Set(header, value)
				}
				w := httptest.NewRecorder()
				engine.ServeHTTP(w, req)
				if credential != "gateway-one" {
					if w.Code != http.StatusUnauthorized {
						t.Fatalf("unauthorized NVIDIA response=%d %s", w.Code, w.Body.String())
					}
					return
				}
				var got struct {
					Authorization string `json:"authorization"`
					APIKey        string `json:"api_key"`
					Body          string `json:"body"`
				}
				if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil {
					t.Fatalf("NVIDIA consumer response=%d %s", w.Code, w.Body.String())
				}
				if got.Authorization != "" || got.APIKey != "" || got.Body != `{"model":"meta/llama-3.3-70b-instruct","messages":[]}` {
					t.Fatalf("NVIDIA consumer received credentials or changed body: %+v", got)
				}
			})
		}
	}
}

func TestGatewayAuthProviderSecretsRemainSeparate(t *testing.T) {
	for _, tc := range []struct {
		name       string
		path       string
		provider   customProvider
		body       string
		wantAuth   string
		wantAPIKey string
	}{
		{"chat", "/v1/chat/completions", customProvider{Name: "vendor", Model: "chat-model", BaseURL: "https://up.invalid/v1", APIKey: "provider-secret"}, `{"model":"vendor/chat-model","messages":[]}`, "Bearer provider-secret", ""},
		{"messages", "/v1/messages", customProvider{Name: "vendor", Model: "messages-model", BaseURL: "https://up.invalid/v1", APIKey: "provider-secret", MessagesOnly: true}, `{"model":"vendor/messages-model","max_tokens":16,"messages":[]}`, "", "provider-secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := fakeUpstream(t, `{"id":"upstream-response"}`)
			prefs := &modelPrefs{providers: []customProvider{tc.provider}}
			engine := gatewayAuthTestEngine("gateway-one")
			engine.Use(customOpenAICompatPassthrough(prefs), messagesOnlyPassthrough(prefs))
			engine.POST(tc.path, func(c *gin.Context) { c.String(http.StatusTeapot, "unexpected fallthrough") })
			for _, credential := range []string{"", "incorrect-client-secret", "provider-secret"} {
				rejected := httptest.NewRequestWithContext(t.Context(), http.MethodPost, tc.path, strings.NewReader(tc.body))
				if credential != "" {
					rejected.Header.Set("Authorization", "Bearer "+credential)
				}
				denied := httptest.NewRecorder()
				engine.ServeHTTP(denied, rejected)
				if denied.Code != http.StatusUnauthorized || log.count() != 0 {
					t.Fatalf("unauthorized request reached provider: status=%d calls=%d", denied.Code, log.count())
				}
			}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer gateway-one")
			req.Header.Set("x-api-key", "gateway-one")
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, req)
			if w.Code != http.StatusOK || w.Body.String() != `{"id":"upstream-response"}` {
				t.Fatalf("provider response=%d %s", w.Code, w.Body.String())
			}
			call := log.last(t)
			if call.Header.Get("Authorization") != tc.wantAuth || call.Header.Get("x-api-key") != tc.wantAPIKey {
				t.Fatalf("provider credentials were replaced by gateway credentials: %v", call.Header)
			}
			for _, values := range call.Header {
				for _, value := range values {
					if strings.Contains(value, "gateway-one") {
						t.Fatalf("gateway credential leaked upstream: %v", call.Header)
					}
				}
			}
		})
	}
}

// Stripping credentials must not break cycleRouter's internal engine reentry.
func TestGatewayAuthCycleRetriesWithoutCredentials(t *testing.T) {
	prefs := &modelPrefs{cycle: &cycleConfig{Name: "cycle", Models: []string{"first", "second"}}}
	engine := gatewayAuthTestEngine("gateway-one")
	engine.Use(cycleRouter(prefs, engine))
	engine.POST("/v1/messages", func(c *gin.Context) {
		if c.GetHeader("Authorization") != "" || c.GetHeader("x-api-key") != "" {
			c.String(http.StatusInternalServerError, "credential reached cycle consumer")
			return
		}
		var body struct {
			Model string `json:"model"`
		}
		if c.ShouldBindJSON(&body) != nil {
			c.Status(http.StatusBadRequest)
			return
		}
		if body.Model == "first" {
			c.Status(http.StatusTooManyRequests)
			return
		}
		c.JSON(http.StatusOK, gin.H{"model": body.Model})
	})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"cycle","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gateway-one")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusOK || w.Body.String() != `{"model":"second"}` {
		t.Fatalf("authenticated cycle retry failed: %d %s", w.Code, w.Body.String())
	}
	unauthenticated := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"cycle","messages":[]}`))
	denied := httptest.NewRecorder()
	engine.ServeHTTP(denied, unauthenticated)
	if denied.Code != http.StatusUnauthorized {
		t.Fatalf("authentication leaked across requests: %d %s", denied.Code, denied.Body.String())
	}
}

func TestGatewayAuthTrustIsScopedToMiddleware(t *testing.T) {
	other := gatewayAuthTestEngine("other-gateway")
	other.POST("/v1/chat/completions", gatewayAuthConsumer)
	engine := gatewayAuthTestEngine("gateway-one")
	engine.POST("/v1/chat/completions", func(c *gin.Context) {
		other.ServeHTTP(c.Writer, c.Request.Clone(c.Request.Context()))
	})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"nvidia/test","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gateway-one")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("different middleware accepted another gateway's authentication: %d %s", w.Code, w.Body.String())
	}
}

func TestGatewayAuthMCPPing(t *testing.T) {
	for _, header := range []string{"Authorization", "x-api-key"} {
		t.Run(header, func(t *testing.T) {
			engine := gatewayAuthTestEngine("gateway-one")
			engine.POST("/mcp", searchMCP)
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":7,"method":"ping"}`))
			value := "gateway-one"
			if header == "Authorization" {
				value = "Bearer " + value
			}
			req.Header.Set(header, value)
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, req)
			var response struct {
				JSONRPC string          `json:"jsonrpc"`
				ID      int             `json:"id"`
				Result  json.RawMessage `json:"result"`
			}
			if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &response) != nil || response.JSONRPC != "2.0" || response.ID != 7 || string(response.Result) != "{}" {
				t.Fatalf("authenticated MCP ping failed: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func gatewayAuthTestEngine(keys string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(gatewayAuth(keys))
	return engine
}

func gatewayAuthConsumer(c *gin.Context) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.String(http.StatusBadRequest, "unreadable body")
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"authorization": c.GetHeader("Authorization"),
		"api_key":       c.GetHeader("x-api-key"),
		"version":       c.GetHeader("anthropic-version"),
		"body":          string(body),
	})
}
