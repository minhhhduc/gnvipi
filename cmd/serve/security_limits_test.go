package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	gwprovider "github.com/minhhhduc/gnvipi/internal/gateway/provider"
)

// =============================================================================
// 1. Inbound Gateway Auth
// =============================================================================

func TestSecurityLimits_InboundGatewayAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const gatewayKey = "secret-gateway-token"

	setupEngine := func(key string) *gin.Engine {
		e := gin.New()
		e.Use(gatewayRequestContext())
		e.Use(gatewayAuth(key))
		handler := func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"status": "ok"})
		}
		e.POST("/v1/chat/completions", handler)
		e.POST("/v1/messages", handler)
		e.POST("/v1/responses", handler)
		e.POST("/mcp", handler)
		return e
	}

	t.Run("Configured_MissingOrWrongToken_Returns401_WithCorrectProtocolFormat", func(t *testing.T) {
		engine := setupEngine(gatewayKey)

		testCases := []struct {
			name        string
			path        string
			authHeader  string
			checkFormat func(t *testing.T, body string)
		}{
			{
				name:       "OpenAIChat_MissingToken",
				path:       "/v1/chat/completions",
				authHeader: "",
				checkFormat: func(t *testing.T, body string) {
					var res struct {
						Error struct {
							Message string `json:"message"`
							Type    string `json:"type"`
						} `json:"error"`
					}
					if err := json.Unmarshal([]byte(body), &res); err != nil {
						t.Fatalf("invalid OpenAI error JSON: %v, body=%s", err, body)
					}
					if res.Error.Type != "authentication_error" {
						t.Fatalf("expected authentication_error type, got: %q", res.Error.Type)
					}
					if res.Error.Message == "" {
						t.Fatal("expected non-empty error message")
					}
				},
			},
			{
				name:       "OpenAIChat_WrongToken",
				path:       "/v1/chat/completions",
				authHeader: "Bearer wrong-token",
				checkFormat: func(t *testing.T, body string) {
					var res struct {
						Error struct {
							Type string `json:"type"`
						} `json:"error"`
					}
					if err := json.Unmarshal([]byte(body), &res); err != nil {
						t.Fatalf("invalid OpenAI error JSON: %v", err)
					}
					if res.Error.Type != "authentication_error" {
						t.Fatalf("expected authentication_error type, got: %q", res.Error.Type)
					}
				},
			},
			{
				name:       "AnthropicMessages_MissingToken",
				path:       "/v1/messages",
				authHeader: "",
				checkFormat: func(t *testing.T, body string) {
					var res struct {
						Type  string `json:"type"`
						Error struct {
							Type    string `json:"type"`
							Message string `json:"message"`
						} `json:"error"`
					}
					if err := json.Unmarshal([]byte(body), &res); err != nil {
						t.Fatalf("invalid Anthropic error JSON: %v, body=%s", err, body)
					}
					if res.Type != "error" || res.Error.Type != "authentication_error" {
						t.Fatalf("expected type=error and error.type=authentication_error, got: type=%q error.type=%q", res.Type, res.Error.Type)
					}
					if res.Error.Message == "" {
						t.Fatal("expected non-empty error message")
					}
				},
			},
			{
				name:       "AnthropicMessages_WrongToken",
				path:       "/v1/messages",
				authHeader: "Bearer invalid-bearer-key",
				checkFormat: func(t *testing.T, body string) {
					var res struct {
						Type  string `json:"type"`
						Error struct {
							Type string `json:"type"`
						} `json:"error"`
					}
					if err := json.Unmarshal([]byte(body), &res); err != nil {
						t.Fatalf("invalid Anthropic error JSON: %v", err)
					}
					if res.Type != "error" || res.Error.Type != "authentication_error" {
						t.Fatalf("expected Anthropic error structure, got: %+v", res)
					}
				},
			},
			{
				name:       "OpenAIResponses_MissingToken",
				path:       "/v1/responses",
				authHeader: "",
				checkFormat: func(t *testing.T, body string) {
					var res struct {
						Error struct {
							Message string `json:"message"`
						} `json:"error"`
					}
					if err := json.Unmarshal([]byte(body), &res); err != nil {
						t.Fatalf("invalid Responses error JSON: %v, body=%s", err, body)
					}
					if res.Error.Message == "" {
						t.Fatal("expected non-empty error message")
					}
				},
			},
			{
				name:       "OpenAIResponses_WrongToken",
				path:       "/v1/responses",
				authHeader: "Bearer incorrect-token",
				checkFormat: func(t *testing.T, body string) {
					var res struct {
						Error struct {
							Message string `json:"message"`
						} `json:"error"`
					}
					if err := json.Unmarshal([]byte(body), &res); err != nil {
						t.Fatalf("invalid Responses error JSON: %v", err)
					}
					if res.Error.Message == "" {
						t.Fatal("expected non-empty error message")
					}
				},
			},
			{
				name:       "MCP_MissingToken",
				path:       "/mcp",
				authHeader: "",
				checkFormat: func(t *testing.T, body string) {
					var res struct {
						JSONRPC string `json:"jsonrpc"`
						ID      any    `json:"id"`
						Error   struct {
							Code    int    `json:"code"`
							Message string `json:"message"`
						} `json:"error"`
					}
					if err := json.Unmarshal([]byte(body), &res); err != nil {
						t.Fatalf("invalid JSON-RPC error: %v, body=%s", err, body)
					}
					if res.JSONRPC != "2.0" || res.Error.Code != -32000 {
						t.Fatalf("expected JSON-RPC 2.0 with code -32000, got: %+v", res)
					}
				},
			},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, tc.path, strings.NewReader(`{}`))
				if tc.authHeader != "" {
					req.Header.Set("Authorization", tc.authHeader)
				}
				w := httptest.NewRecorder()
				engine.ServeHTTP(w, req)

				if w.Code != http.StatusUnauthorized {
					t.Fatalf("expected status 401 Unauthorized, got: %d", w.Code)
				}
				if authHeader := w.Header().Get("WWW-Authenticate"); !strings.Contains(authHeader, `Bearer realm="gateway"`) {
					t.Fatalf("expected WWW-Authenticate header, got: %q", authHeader)
				}
				tc.checkFormat(t, w.Body.String())
			})
		}
	})

	t.Run("Configured_ValidBearerToken_Succeeds", func(t *testing.T) {
		engine := setupEngine(gatewayKey)

		endpoints := []string{"/v1/chat/completions", "/v1/messages", "/v1/responses", "/mcp"}
		for _, ep := range endpoints {
			t.Run(ep+"_Bearer", func(t *testing.T) {
				req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, ep, strings.NewReader(`{}`))
				req.Header.Set("Authorization", "Bearer "+gatewayKey)
				w := httptest.NewRecorder()
				engine.ServeHTTP(w, req)

				if w.Code != http.StatusOK {
					t.Fatalf("%s with valid bearer token failed: %d body=%s", ep, w.Code, w.Body.String())
				}
			})

			t.Run(ep+"_ApiKeyHeader", func(t *testing.T) {
				req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, ep, strings.NewReader(`{}`))
				req.Header.Set("x-api-key", gatewayKey)
				w := httptest.NewRecorder()
				engine.ServeHTTP(w, req)

				if w.Code != http.StatusOK {
					t.Fatalf("%s with valid x-api-key failed: %d body=%s", ep, w.Code, w.Body.String())
				}
			})
		}
	})

	t.Run("Unconfigured_RequestsSucceedWithoutAuth", func(t *testing.T) {
		engine := setupEngine("") // Unconfigured

		endpoints := []string{"/v1/chat/completions", "/v1/messages", "/v1/responses"}
		for _, ep := range endpoints {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, ep, strings.NewReader(`{}`))
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("%s without auth should succeed when gateway auth is unconfigured: %d", ep, w.Code)
			}
		}
	})
}

// =============================================================================
// 2. Admin Route Protection & CSRF
// =============================================================================

func TestSecurityLimits_AdminRouteProtectionAndCSRF(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const adminToken = "admin-secret-token-123"

	setupAdminEngine := func(addr, token string) (*gin.Engine, *modelPrefs) {
		p := &modelPrefs{}
		e := gin.New()
		e.Use(adminAccess(addr, token))
		registerAdminAPIRoutes(e, p)
		e.Any("/admin", func(c *gin.Context) {
			handleAdminDashboard(c, nil)
		})
		e.POST("/admin/providers", func(c *gin.Context) {
			c.String(http.StatusOK, "saved")
		})
		return e, p
	}

	t.Run("AdminRoutes_RequireLoopbackOrTokenAuth", func(t *testing.T) {
		// When admin token is configured
		t.Run("ConfiguredToken_EnforcesAuth", func(t *testing.T) {
			engine, _ := setupAdminEngine("127.0.0.1:8080", adminToken)

			// Missing token
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/admin/api/providers", http.NoBody)
			req.Host = "localhost:8080"
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, req)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("API request without token got %d, want 401", w.Code)
			}

			// Wrong token
			req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/admin/api/providers", http.NoBody)
			req.Host = "localhost:8080"
			req.Header.Set("Authorization", "Bearer wrong-token")
			w = httptest.NewRecorder()
			engine.ServeHTTP(w, req)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("API request with wrong token got %d, want 401", w.Code)
			}

			// Valid token via Authorization header
			req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/admin/api/providers", http.NoBody)
			req.Host = "localhost:8080"
			req.Header.Set("Authorization", "Bearer "+adminToken)
			w = httptest.NewRecorder()
			engine.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("API request with valid Bearer token got %d, want 200", w.Code)
			}

			// Valid token via X-Admin-Token header
			req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/admin/api/providers", http.NoBody)
			req.Host = "localhost:8080"
			req.Header.Set("X-Admin-Token", adminToken)
			w = httptest.NewRecorder()
			engine.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("API request with valid X-Admin-Token got %d, want 200", w.Code)
			}
		})

		// When token is NOT configured on non-loopback bind
		t.Run("UnconfiguredToken_NonLoopbackBind_RejectsWith403", func(t *testing.T) {
			engine, _ := setupAdminEngine("0.0.0.0:8080", "")

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/admin", http.NoBody)
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, req)
			if w.Code != http.StatusForbidden {
				t.Fatalf("non-loopback admin without token got %d, want 403", w.Code)
			}
			if !strings.Contains(w.Body.String(), "GNVIPI_ADMIN_TOKEN") {
				t.Fatalf("missing guidance in forbidden error response: %s", w.Body.String())
			}

			req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/admin/api/providers", http.NoBody)
			w = httptest.NewRecorder()
			engine.ServeHTTP(w, req)
			if w.Code != http.StatusForbidden {
				t.Fatalf("non-loopback admin API without token got %d, want 403", w.Code)
			}
		})

		// When token is NOT configured on loopback bind
		t.Run("UnconfiguredToken_LoopbackBind_AllowsLocalRequestsOnly", func(t *testing.T) {
			engine, _ := setupAdminEngine("127.0.0.1:8080", "")

			// Loopback Host -> allowed
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/admin/api/providers", http.NoBody)
			req.Host = "127.0.0.1:8080"
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("loopback host got %d, want 200", w.Code)
			}

			// Non-loopback Host (e.g. DNS rebinding) -> 403
			req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/admin/api/providers", http.NoBody)
			req.Host = "attacker.example:8080"
			w = httptest.NewRecorder()
			engine.ServeHTTP(w, req)
			if w.Code != http.StatusForbidden {
				t.Fatalf("attacker host on loopback listener got %d, want 403", w.Code)
			}
		})
	})

	t.Run("StateModifying_MismatchedOriginReferer_Returns403", func(t *testing.T) {
		engine, _ := setupAdminEngine("127.0.0.1:8080", "")

		mutatingMethods := []string{http.MethodPost, http.MethodPut, http.MethodDelete}
		targets := []string{"/admin/api/providers", "/admin/providers"}

		for _, method := range mutatingMethods {
			for _, target := range targets {
				t.Run(method+"_"+target+"_MismatchedOrigin", func(t *testing.T) {
					req := httptest.NewRequestWithContext(t.Context(), method, target, http.NoBody)
					req.Host = "localhost:8080"
					req.Header.Set("Origin", "http://attacker.example")
					w := httptest.NewRecorder()
					engine.ServeHTTP(w, req)

					if w.Code != http.StatusForbidden {
						t.Fatalf("%s %s with mismatched Origin got %d, want 403", method, target, w.Code)
					}
				})

				t.Run(method+"_"+target+"_CrossSiteSecFetchSite", func(t *testing.T) {
					req := httptest.NewRequestWithContext(t.Context(), method, target, http.NoBody)
					req.Host = "localhost:8080"
					req.Header.Set("Origin", "http://localhost:8080")
					req.Header.Set("Sec-Fetch-Site", "cross-site")
					w := httptest.NewRecorder()
					engine.ServeHTTP(w, req)

					if w.Code != http.StatusForbidden {
						t.Fatalf("%s %s with Sec-Fetch-Site: cross-site got %d, want 403", method, target, w.Code)
					}
				})

				t.Run(method+"_"+target+"_MismatchedRefererWithoutOrigin", func(t *testing.T) {
					req := httptest.NewRequestWithContext(t.Context(), method, target, http.NoBody)
					req.Host = "localhost:8080"
					req.Header.Set("Referer", "http://evil.example/csrf.html")
					w := httptest.NewRecorder()
					engine.ServeHTTP(w, req)

					if w.Code != http.StatusForbidden {
						t.Fatalf("%s %s with mismatched Referer got %d, want 403", method, target, w.Code)
					}
				})

				t.Run(method+"_"+target+"_SameOrigin_Succeeds", func(t *testing.T) {
					req := httptest.NewRequestWithContext(t.Context(), method, target, strings.NewReader(`{}`))
					req.Host = "localhost:8080"
					req.Header.Set("Origin", "http://localhost:8080")
					req.Header.Set("Sec-Fetch-Site", "same-origin")
					w := httptest.NewRecorder()
					engine.ServeHTTP(w, req)

					if w.Code == http.StatusForbidden {
						t.Fatalf("%s %s with same origin was rejected with 403", method, target)
					}
				})
			}
		}
	})
}

// =============================================================================
// 3. Secret Redaction Everywhere
// =============================================================================

func TestSecurityLimits_SecretRedactionEverywhere(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const rawSecretKey = "sk-live-super-secret-key-abcdef-1234567890"
	const rawEnvSecret = "secret-env-var-token-9876543210"

	t.Setenv("SECRET_PROVIDER_ENV_KEY", rawEnvSecret)

	dir := t.TempDir()
	modelsPath := filepath.Join(dir, "models.json")

	m := &modelPrefs{}
	if err := m.load(modelsPath); err != nil {
		t.Fatal(err)
	}
	oldPrefs := prefs
	prefs = m
	t.Cleanup(func() { prefs = oldPrefs })

	prov := gwprovider.Provider{
		ID:       "redaction-test-prov",
		Name:     "RedactionTestProvider",
		BaseURL:  "https://api.redaction.example/v1",
		Protocol: ir.ProtocolOpenAIChat,
		Auth:     gwprovider.Auth{Scheme: gwprovider.AuthBearer, Key: rawSecretKey},
		Headers: map[string]string{
			"X-Custom-Safe": "safe-custom-value",
		},
		Models:  []gwprovider.ModelMapping{{Alias: "redaction/model", UpstreamModel: "m"}},
		Enabled: true,
	}
	if err := m.upsertProviderV2(&prov); err != nil {
		t.Fatal(err)
	}

	envProv := gwprovider.Provider{
		ID:       "redaction-env-prov",
		Name:     "RedactionEnvProvider",
		BaseURL:  "https://api.redaction-env.example/v1",
		Protocol: ir.ProtocolOpenAIChat,
		Auth:     gwprovider.Auth{Scheme: gwprovider.AuthBearer, KeyEnv: "SECRET_PROVIDER_ENV_KEY"},
		Headers: map[string]string{
			"X-Custom-Env": "safe-env-value",
		},
		Models:  []gwprovider.ModelMapping{{Alias: "redaction/env-model", UpstreamModel: "m"}},
		Enabled: true,
	}
	if err := m.upsertProviderV2(&envProv); err != nil {
		t.Fatal(err)
	}

	engine := gin.New()
	engine.Use(secretResponseRedaction())
	engine.Use(adminAccess("127.0.0.1:8080", ""))
	registerAdminAPIRoutes(engine, m)
	engine.GET("/admin", func(c *gin.Context) {
		handleAdminDashboard(c, nil)
	})

	t.Run("AdminJSONResponses_RedactSecrets", func(t *testing.T) {
		endpoints := []string{
			"/admin/api/providers",
			"/admin/api/providers/redaction-test-prov",
			"/admin/api/providers/redaction-env-prov",
		}

		for _, ep := range endpoints {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, ep, http.NoBody)
			req.Host = "localhost:8080"
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("GET %s returned %d: %s", ep, w.Code, w.Body.String())
			}
			body := w.Body.String()
			if strings.Contains(body, rawSecretKey) {
				t.Fatalf("SECURITY LEAK: %s leaked raw API key: %s", ep, body)
			}
			if strings.Contains(body, rawEnvSecret) {
				t.Fatalf("SECURITY LEAK: %s leaked raw env secret: %s", ep, body)
			}
			// Masked key hint should be present for direct key
			if ep == "/admin/api/providers/redaction-test-prov" && !strings.Contains(body, "sk-...7890") {
				t.Fatalf("%s missing masked key hint sk-...7890: %s", ep, body)
			}
		}
	})

	t.Run("AdminHTMLPages_RedactSecrets", func(t *testing.T) {
		pages := []string{
			"/admin",
			"/admin?page=models",
			"/admin?page=config",
			"/admin?page=dashboard",
			"/admin?page=chromes",
			"/admin?page=providers",
		}

		for _, p := range pages {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, p, http.NoBody)
			req.Host = "localhost:8080"
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("GET %s returned %d: %s", p, w.Code, w.Body.String())
			}
			body := w.Body.String()
			if strings.Contains(body, rawSecretKey) {
				t.Fatalf("SECURITY LEAK: rendered HTML %s leaked raw API key: %s", p, body)
			}
			if strings.Contains(body, rawEnvSecret) {
				t.Fatalf("SECURITY LEAK: rendered HTML %s leaked raw env secret: %s", p, body)
			}
		}
	})

	t.Run("SimulatedLogOutputs_RedactSecrets", func(t *testing.T) {
		// Verify response-level redaction middleware filters secrets from error messages
		testRedactEngine := gin.New()
		testRedactEngine.Use(secretResponseRedaction())
		testRedactEngine.GET("/simulated-error", func(c *gin.Context) {
			c.JSON(http.StatusBadGateway, gin.H{
				"error": gin.H{
					"message": fmt.Sprintf("upstream failed with token %s and env token %s", rawSecretKey, rawEnvSecret),
				},
			})
		})

		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/simulated-error", http.NoBody)
		w := httptest.NewRecorder()
		testRedactEngine.ServeHTTP(w, req)

		body := w.Body.String()
		if strings.Contains(body, rawSecretKey) {
			t.Fatalf("secret response redaction failed to scrub rawSecretKey: %s", body)
		}
		if strings.Contains(body, rawEnvSecret) {
			t.Fatalf("secret response redaction failed to scrub rawEnvSecret: %s", body)
		}

		// Verify gwerr.Scrub scrub behavior
		msg := fmt.Sprintf("Error dialing: %s", rawSecretKey)
		scrubbed := gwerr.Scrub(msg, rawSecretKey)
		if strings.Contains(scrubbed, rawSecretKey) {
			t.Fatalf("gwerr.Scrub leaked secret: %s", scrubbed)
		}
		if !strings.Contains(scrubbed, "[REDACTED]") && !strings.Contains(scrubbed, "[redacted]") {
			t.Fatalf("gwerr.Scrub missing redacted sentinel: %s", scrubbed)
		}
	})
}

// =============================================================================
// 4. SSRF Rejection
// =============================================================================

func TestSecurityLimits_SSRFRejection(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("ProviderConnectionTests_RejectMetadataAndRestrictedLoopback", func(t *testing.T) {
		p := &modelPrefs{}
		engine := gin.New()
		engine.Use(adminAccess("127.0.0.1:8080", ""))
		registerAdminAPIRoutes(engine, p)

		// 1. Cloud metadata endpoints must be rejected by default
		metadataTargets := []string{
			"http://169.254.169.254/v1",
			"http://[fd00:ec2::254]/v1",
			"http://metadata.google.internal/v1",
		}

		for _, target := range metadataTargets {
			t.Run(target, func(t *testing.T) {
				prov := gwprovider.Provider{
					ID:       "ssrf-metadata-test",
					Name:     "SSRFTest",
					BaseURL:  target,
					Protocol: ir.ProtocolOpenAIChat,
					Auth:     gwprovider.Auth{Scheme: gwprovider.AuthNone},
					Models:   []gwprovider.ModelMapping{{Alias: "test", UpstreamModel: "m"}},
				}
				body, err := json.Marshal(prov)
				if err != nil {
					t.Fatal(err)
				}
				req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/admin/api/providers/test", bytes.NewReader(body))
				req.Host = "localhost:8080"
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				engine.ServeHTTP(w, req)

				// Must be rejected: either 400 validation error or 200 with ok: false
				if w.Code == http.StatusOK {
					var res ConnectionTestResult
					if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
						t.Fatal(err)
					}
					if res.OK {
						t.Fatalf("SSRF cloud metadata target %s succeeded unexpectedly", target)
					}
				} else if w.Code != http.StatusBadRequest {
					t.Fatalf("unexpected status for SSRF target %s: %d body=%s", target, w.Code, w.Body.String())
				}
			})
		}

		// 2. Cross-origin redirect to metadata must be rejected
		t.Run("RedirectToMetadata_Rejected", func(t *testing.T) {
			redirectServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
			}))
			defer redirectServer.Close()

			prov := gwprovider.Provider{
				ID:       "ssrf-redirect-test",
				Name:     "SSRFRedirectTest",
				BaseURL:  redirectServer.URL,
				Protocol: ir.ProtocolOpenAIChat,
				Auth:     gwprovider.Auth{Scheme: gwprovider.AuthNone},
				Models:   []gwprovider.ModelMapping{{Alias: "test", UpstreamModel: "m"}},
			}
			body, err := json.Marshal(prov)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/admin/api/providers/test", bytes.NewReader(body))
			req.Host = "localhost:8080"
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, req)

			if w.Code == http.StatusOK {
				var res ConnectionTestResult
				if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
					t.Fatal(err)
				}
				if res.OK {
					t.Fatalf("redirect to metadata target succeeded unexpectedly: %+v", res)
				}
			}
		})

		// 3. When DenyPrivate is enabled, loopback is strictly rejected
		t.Run("RestrictedLoopback_Rejected", func(t *testing.T) {
			gwprovider.SetDefaultURLPolicy(gwprovider.URLPolicy{DenyPrivate: true})
			t.Cleanup(func() { gwprovider.SetDefaultURLPolicy(gwprovider.URLPolicy{}) })

			for _, loopbackTarget := range []string{"http://127.0.0.1:8080/v1", "http://[::1]:8080/v1", "http://localhost:8080/v1"} {
				prov := gwprovider.Provider{
					ID:       "ssrf-loopback-test",
					Name:     "SSRFLoopbackTest",
					BaseURL:  loopbackTarget,
					Protocol: ir.ProtocolOpenAIChat,
					Auth:     gwprovider.Auth{Scheme: gwprovider.AuthNone},
					Models:   []gwprovider.ModelMapping{{Alias: "test", UpstreamModel: "m"}},
				}
				body, err := json.Marshal(prov)
				if err != nil {
					t.Fatal(err)
				}
				req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/admin/api/providers/test", bytes.NewReader(body))
				req.Host = "localhost:8080"
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				engine.ServeHTTP(w, req)

				if w.Code == http.StatusOK {
					var res ConnectionTestResult
					if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
						t.Fatal(err)
					}
					if res.OK {
						t.Fatalf("private loopback target %s accepted under DenyPrivate policy", loopbackTarget)
					}
				} else if w.Code != http.StatusBadRequest {
					t.Fatalf("unexpected status for private target %s: %d body=%s", loopbackTarget, w.Code, w.Body.String())
				}
			}
		})
	})

	t.Run("UpstreamDispatch_RejectsMetadataURLs", func(t *testing.T) {
		for _, rawURL := range []string{
			"http://169.254.169.254/v1/chat/completions",
			"http://[fd00:ec2::254]/v1/chat/completions",
			"http://metadata.google.internal/v1/chat/completions",
		} {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, rawURL, http.NoBody)
			if err != nil {
				t.Fatal(err)
			}
			if err := gwprovider.DefaultURLPolicy().ValidateRequest(req); err == nil {
				t.Fatalf("ValidateRequest accepted cloud metadata URL: %s", rawURL)
			}
		}

		// Also verify Control post-resolution hook blocks direct dials
		for _, addr := range []string{"169.254.169.254:80", "[fd00:ec2::254]:80"} {
			if err := gwprovider.DefaultURLPolicy().Control("tcp", addr, nil); err == nil {
				t.Fatalf("Control accepted metadata destination: %s", addr)
			}
		}
	})
}

// =============================================================================
// 5. Request Body Limits
// =============================================================================

func TestSecurityLimits_RequestBodyLimits(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const v1Limit = int64(1024)
	const adminLimit = int64(512)

	setupEngine := func() *gin.Engine {
		e := gin.New()
		e.Use(requestBodyLimit(v1Limit, adminLimit))
		handler := func(c *gin.Context) {
			body, err := io.ReadAll(c.Request.Body)
			if err != nil {
				c.Status(http.StatusInternalServerError)
				return
			}
			c.Data(http.StatusOK, "application/json", body)
		}
		e.POST("/v1/chat/completions", handler)
		e.POST("/v1/messages", handler)
		e.POST("/v1/responses", handler)
		e.POST("/admin/api/providers", handler)
		return e
	}

	t.Run("GatewayEndpoints_ExceedingLimit_Returns413_WithProtocolJSON", func(t *testing.T) {
		engine := setupEngine()
		oversized := strings.Repeat("a", int(v1Limit)+10)

		testCases := []struct {
			name        string
			path        string
			checkFormat func(t *testing.T, body string)
		}{
			{
				name: "OpenAIChat_413",
				path: "/v1/chat/completions",
				checkFormat: func(t *testing.T, body string) {
					var res struct {
						Error struct {
							Message string `json:"message"`
							Type    string `json:"type"`
						} `json:"error"`
					}
					if err := json.Unmarshal([]byte(body), &res); err != nil {
						t.Fatalf("invalid OpenAI error body: %v, body=%s", err, body)
					}
					if res.Error.Message == "" {
						t.Fatal("expected error message in 413 response")
					}
				},
			},
			{
				name: "AnthropicMessages_413",
				path: "/v1/messages",
				checkFormat: func(t *testing.T, body string) {
					var res struct {
						Type  string `json:"type"`
						Error struct {
							Type    string `json:"type"`
							Message string `json:"message"`
						} `json:"error"`
					}
					if err := json.Unmarshal([]byte(body), &res); err != nil {
						t.Fatalf("invalid Anthropic error body: %v, body=%s", err, body)
					}
					if res.Type != "error" || res.Error.Type != "invalid_request_error" {
						t.Fatalf("unexpected Anthropic error format: %+v", res)
					}
				},
			},
			{
				name: "OpenAIResponses_413",
				path: "/v1/responses",
				checkFormat: func(t *testing.T, body string) {
					var res struct {
						Error struct {
							Message string `json:"message"`
						} `json:"error"`
					}
					if err := json.Unmarshal([]byte(body), &res); err != nil {
						t.Fatalf("invalid Responses error body: %v, body=%s", err, body)
					}
					if res.Error.Message == "" {
						t.Fatal("expected error message in 413 response")
					}
				},
			},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, tc.path, strings.NewReader(oversized))
				w := httptest.NewRecorder()
				engine.ServeHTTP(w, req)

				if w.Code != http.StatusRequestEntityTooLarge {
					t.Fatalf("%s got status %d, want 413", tc.path, w.Code)
				}
				tc.checkFormat(t, w.Body.String())
			})
		}

		// Underreported Content-Length must also be rejected
		t.Run("UnderreportedContentLength_StillRejected", func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(oversized))
			req.ContentLength = 10 // Lie about length
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, req)

			if w.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("underreported body got %d, want 413", w.Code)
			}
		})

		// Payloads within limit must succeed
		t.Run("PayloadWithinLimit_Succeeds", func(t *testing.T) {
			validPayload := `{"model":"test","messages":[]}`
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(validPayload))
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("payload within limit failed: %d, body=%s", w.Code, w.Body.String())
			}
		})
	})

	t.Run("AdminEndpoints_ExceedingAdminBodyLimit_Returns413", func(t *testing.T) {
		engine := setupEngine()
		oversized := strings.Repeat("x", int(adminLimit)+10)

		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/admin/api/providers", strings.NewReader(oversized))
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)

		if w.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("admin endpoint with oversized body got %d, want 413", w.Code)
		}

		// Valid admin payload within limit
		req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/admin/api/providers", strings.NewReader(`{"name":"ok"}`))
		w = httptest.NewRecorder()
		engine.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("admin endpoint within limit failed: %d", w.Code)
		}
	})
}

// =============================================================================
// 6. Stalled Upstream & Idle Stream Timeouts
// =============================================================================

func TestSecurityLimits_StalledUpstreamAndIdleStreamTimeouts(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("SlowOrHangingUpstream_TriggersTimeout_Returns504Or502", func(t *testing.T) {
		hangingDone := make(chan struct{})
		// Server that hangs until context cancels or test completes
		hangingServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-hangingDone:
			case <-r.Context().Done():
			}
		}))
		defer func() {
			close(hangingDone)
			hangingServer.CloseClientConnections()
			hangingServer.Close()
		}()

		doc := gwprovider.Document{
			Providers: []gwprovider.Provider{
				{
					ID:       "hanging-provider",
					Name:     "HangingProvider",
					BaseURL:  hangingServer.URL,
					Protocol: ir.ProtocolOpenAIChat,
					Auth:     gwprovider.Auth{Scheme: gwprovider.AuthNone},
					Timeouts: gwprovider.Timeouts{
						Connect:        40 * time.Millisecond,
						ResponseHeader: 40 * time.Millisecond,
						Total:          60 * time.Millisecond,
					},
					Models: []gwprovider.ModelMapping{
						{Alias: "hanging/chat", UpstreamModel: "chat-model"},
					},
					Enabled: true,
				},
				{
					ID:       "refused-provider",
					Name:     "RefusedProvider",
					BaseURL:  "http://127.0.0.1:1", // Port 1 refuses connection
					Protocol: ir.ProtocolOpenAIChat,
					Auth:     gwprovider.Auth{Scheme: gwprovider.AuthNone},
					Timeouts: gwprovider.Timeouts{
						Connect: 50 * time.Millisecond,
					},
					Models: []gwprovider.ModelMapping{
						{Alias: "refused/chat", UpstreamModel: "chat-model"},
					},
					Enabled: true,
				},
			},
		}

		modelsPath := filepath.Join(t.TempDir(), "models.json")
		if err := gwprovider.WriteDocument(modelsPath, doc); err != nil {
			t.Fatal(err)
		}

		p := &modelPrefs{}
		if err := p.load(modelsPath); err != nil {
			t.Fatal(err)
		}

		dispatcher, err := gatewayV2Middleware(p, true, "")
		if err != nil {
			t.Fatal(err)
		}

		engine := gin.New()
		engine.Use(gatewayRequestContext(), dispatcher)

		// 1. Hanging upstream should timeout with 504 Gateway Timeout
		start := time.Now()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"hanging/chat","messages":[{"role":"user","content":"hi"}]}`))
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		duration := time.Since(start)

		if w.Code != http.StatusGatewayTimeout {
			t.Fatalf("hanging upstream returned status %d, want 504 Gateway Timeout. Body: %s", w.Code, w.Body.String())
		}
		if duration > 2*time.Second {
			t.Fatalf("hanging upstream took too long to timeout: %v", duration)
		}

		// 2. Refused connection should respond with 502 Bad Gateway
		reqRefused := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"refused/chat","messages":[{"role":"user","content":"hi"}]}`))
		wRefused := httptest.NewRecorder()
		engine.ServeHTTP(wRefused, reqRefused)

		if wRefused.Code != http.StatusBadGateway {
			t.Fatalf("refused connection returned status %d, want 502 Bad Gateway. Body: %s", wRefused.Code, wRefused.Body.String())
		}
	})

	t.Run("MidStreamSilence_TerminatesAfterIdleTimeout_NoGoroutineLeak", func(t *testing.T) {
		var firstChunkSent atomic.Bool
		upstreamFinished := make(chan struct{})
		streamDone := make(chan struct{})

		// Server that sends initial SSE events, then hangs indefinitely mid-stream
		streamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer close(upstreamFinished)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, ok := w.(http.Flusher)
			if ok {
				flusher.Flush()
			}

			// Send opening chunk
			chunk := `data: {"id":"chatcmpl-1","choices":[{"delta":{"content":"start"},"index":0}]}` + "\n\n"
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
			if ok {
				flusher.Flush()
			}
			firstChunkSent.Store(true)

			// Hang mid-stream without emitting events
			select {
			case <-streamDone:
			case <-r.Context().Done():
			}
		}))
		defer func() {
			close(streamDone)
			streamServer.CloseClientConnections()
			streamServer.Close()
		}()

		doc := gwprovider.Document{
			Providers: []gwprovider.Provider{
				{
					ID:       "idle-stream-prov",
					Name:     "IdleStreamProvider",
					BaseURL:  streamServer.URL,
					Protocol: ir.ProtocolOpenAIChat,
					Auth:     gwprovider.Auth{Scheme: gwprovider.AuthNone},
					Timeouts: gwprovider.Timeouts{
						Idle: 60 * time.Millisecond,
					},
					Models: []gwprovider.ModelMapping{
						{Alias: "idle/stream", UpstreamModel: "chat-model"},
					},
					Enabled: true,
				},
			},
		}

		modelsPath := filepath.Join(t.TempDir(), "models.json")
		if err := gwprovider.WriteDocument(modelsPath, doc); err != nil {
			t.Fatal(err)
		}

		p := &modelPrefs{}
		if err := p.load(modelsPath); err != nil {
			t.Fatal(err)
		}

		dispatcher, err := gatewayV2Middleware(p, true, "")
		if err != nil {
			t.Fatal(err)
		}

		engine := gin.New()
		engine.Use(gatewayRequestContext(), dispatcher)

		baselineGoroutines := runtime.NumGoroutine()

		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"idle/stream","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		w := httptest.NewRecorder()

		done := make(chan struct{})
		go func() {
			engine.ServeHTTP(w, req)
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("ServeHTTP hung mid-stream, idle timeout did not terminate stream")
		}

		// Upstream request context must have been canceled when idle timeout expired
		select {
		case <-upstreamFinished:
		case <-time.After(time.Second):
			t.Fatal("upstream connection was not terminated upon idle timeout")
		}

		if !firstChunkSent.Load() {
			t.Fatal("first chunk was never sent")
		}

		// Assert no goroutine leaks after stream finishes
		for i := 0; i < 20; i++ {
			runtime.GC()
			time.Sleep(10 * time.Millisecond)
			if runtime.NumGoroutine() <= baselineGoroutines+2 {
				break
			}
		}
		if cur := runtime.NumGoroutine(); cur > baselineGoroutines+3 {
			t.Fatalf("goroutine leak after idle stream termination: baseline=%d current=%d", baselineGoroutines, cur)
		}
	})
}
