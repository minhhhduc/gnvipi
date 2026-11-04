package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/gateway/redact"
	"github.com/minhhhduc/gnvipi/internal/gateway/sse"
)

func TestSecretsAbsentFromResponseLogsAndStats(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.jsonl")
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestSecretRedactionChild$", "-test.count=1")
	cmd.Env = append(os.Environ(), "REDACTION_CHILD=1", "GNVIPI_STATS_FILE="+path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("redaction child: %v\n%s", err, output)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "opaque-handler-secret") {
		t.Fatalf("stats exposed credential: %s", data)
	}
	var event map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(data), &event); err != nil {
		t.Fatal(err)
	}
	if event["error"] != true {
		t.Fatalf("recorded event=%v", event)
	}
}

func TestSecretRedactionChild(t *testing.T) {
	if os.Getenv("REDACTION_CHILD") != "1" {
		t.Skip("isolated redaction regression")
	}
	gin.SetMode(gin.TestMode)
	key := "opaque-handler-secret"
	var logs bytes.Buffer
	log.SetOutput(redact.Default.WrapWriter(&logs))
	p := &modelPrefs{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		data, err := json.Marshal(map[string]any{"error": map[string]string{"message": "failed " + key, "url": "https://upstream.example/?key=" + url.QueryEscape(key)}})
		if err != nil {
			t.Error(err)
			return
		}
		if _, err := w.Write(data); err != nil {
			t.Error(err)
		}
	}))
	defer upstream.Close()
	alias := "provider/" + key
	if err := p.setProviders([]customProvider{{Name: "provider", BaseURL: upstream.URL, Model: "m", Alias: alias, APIKey: key}}); err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	router.Use(secretResponseRedaction(), requestLogger(p), customOpenAICompatPassthrough(p))
	router.POST("/v1/chat/completions", func(c *gin.Context) { c.Status(http.StatusNotFound) })
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"`+alias+`","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway || strings.Contains(response.Body.String(), key) || strings.Contains(logs.String(), key) {
		t.Fatalf("response=%s logs=%s", response.Body.String(), logs.String())
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["error"].(map[string]any); !ok {
		t.Fatalf("error shape changed: %v", body)
	}

	redact.Default.Update("new-active-key")
	success := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"sk-public-example\"}}\n\n"
	failure := "event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"" + key + "\"}}\n\n"
	router.GET("/stream", func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		if _, err := c.Writer.WriteString(success); err != nil {
			t.Error(err)
			return
		}
		cut := strings.Index(failure, key) + 5
		if _, err := c.Writer.WriteString(failure[:cut]); err != nil {
			t.Error(err)
			return
		}
		c.Writer.Flush()
		if _, err := c.Writer.WriteString(failure[cut:]); err != nil {
			t.Error(err)
		}
	})
	stream := httptest.NewRecorder()
	router.ServeHTTP(stream, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/stream", http.NoBody))
	if !strings.HasPrefix(stream.Body.String(), success) || strings.Contains(stream.Body.String(), key) {
		t.Fatalf("stream redaction changed success or leaked old credential: %s", stream.Body.String())
	}
}

func TestSecretResponseRedactionResponsesFailureFrames(t *testing.T) {
	const key = "opaque-responses-failure-key"
	redact.Default.Update(key)
	for _, tc := range []struct{ name, event, data string }{
		{"event_failure", "response.failed", `{"response":{"status":"failed","error":{"message":"opaque-responses-failure-key"}}}`},
		{"data_failure", "", `{"type":"response.failed","response":{"error":{"message":"opaque-responses-failure-key"}}}`},
		{"nested_error", "", `{"response":{"error":{"message":"opaque-responses-failure-key"}}}`},
		{"unicode_failure", "response.failed", `{"type":"response.failed","response":{"error":{"message":"\u006fpaque-responses-failure-key"}}}`},
		{"unicode_error", "error", `{"error":{"message":"\u006fpaque-responses-failure-key"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			router := gin.New()
			router.Use(secretResponseRedaction())
			router.GET("/v1/responses", func(c *gin.Context) {
				c.Header("Content-Type", "text/event-stream")
				frame := "id: " + key + "\n"
				if tc.event != "" {
					frame += "event: " + tc.event + "\n"
				}
				frame += "data: " + tc.data + "\n\n"
				cut := strings.Index(frame, "data:") + len("data: ") + len(tc.data)/2
				if _, err := c.Writer.WriteString(frame[:cut]); err != nil {
					t.Fatal(err)
				}
				c.Writer.Flush()
				if response.Body.Len() != 0 {
					t.Fatalf("partial failure was disclosed: %s", response.Body.String())
				}
				if _, err := c.Writer.WriteString(frame[cut:]); err != nil {
					t.Fatal(err)
				}
			})
			router.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/responses", http.NoBody))
			reader := sse.NewReader(bytes.NewReader(response.Body.Bytes()))
			defer reader.Close()
			event, err := reader.Next(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			var diagnostic any
			if err := json.Unmarshal([]byte(event.Data), &diagnostic); err != nil {
				t.Fatal(err)
			}
			decoded, err := json.Marshal(diagnostic)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(decoded), key) || strings.Contains(event.ID, key) {
				t.Fatalf("failure credential disclosed: %s", response.Body.String())
			}
			if event.Type != tc.event || !strings.Contains(string(decoded), "error") {
				t.Fatalf("failure envelope changed: %s", response.Body.String())
			}
		})
	}
}

func TestSecretResponseRedactionDiagnosticJSON(t *testing.T) {
	const key = "opaque-http-unicode-key"
	redact.Default.Update(key)
	const body = `{"error":{"message":"failed \u006fpaque-http-unicode-key","headers":{"\u0041uthorization":"Bearer opaque-http-unregistered-auth","Proxy-Authorization":"Basic dXNlcjpwYXNzd29yZA==","x-api-key":"opaque-http-unregistered-key"}},"count":9007199254740993,"retry":false,"extra":null}`
	for _, streaming := range []bool{false, true} {
		name := "http_error"
		if streaming {
			name = "sse_error"
		}
		t.Run(name, func(t *testing.T) {
			router := gin.New()
			router.Use(secretResponseRedaction())
			router.GET("/diagnostic", func(c *gin.Context) {
				payload := body
				if streaming {
					c.Header("Content-Type", "text/event-stream")
					payload = "event: error\ndata: " + body + "\n\n"
				} else {
					c.Header("Content-Type", "application/json")
					c.Status(http.StatusBadGateway)
				}
				cut := strings.Index(payload, `\u006f`) + 3
				if _, err := c.Writer.WriteString(payload[:cut]); err != nil {
					t.Fatal(err)
				}
				c.Writer.Flush()
				if _, err := c.Writer.WriteString(payload[cut:]); err != nil {
					t.Fatal(err)
				}
			})
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/diagnostic", http.NoBody))
			data := response.Body.Bytes()
			if streaming {
				reader := sse.NewReader(bytes.NewReader(data))
				defer reader.Close()
				event, err := reader.Next(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				data = []byte(event.Data)
			} else if response.Code != http.StatusBadGateway {
				t.Fatalf("status=%d", response.Code)
			}
			var diagnostic struct {
				Error struct {
					Message string            `json:"message"`
					Headers map[string]string `json:"headers"`
				} `json:"error"`
				Count json.Number `json:"count"`
				Retry bool        `json:"retry"`
				Extra any         `json:"extra"`
			}
			if err := json.Unmarshal(data, &diagnostic); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(diagnostic.Error.Message, key) || !strings.HasPrefix(diagnostic.Error.Message, "failed ") {
				t.Fatalf("decoded diagnostic=%s", data)
			}
			for _, name := range []string{"Authorization", "Proxy-Authorization", "x-api-key"} {
				if diagnostic.Error.Headers[name] != "[redacted]" {
					t.Fatalf("authentication property disclosed: %s", data)
				}
			}
			if diagnostic.Count.String() != "9007199254740993" || diagnostic.Retry || diagnostic.Extra != nil {
				t.Fatalf("diagnostic scalars changed: %s", data)
			}
		})
	}
}

func TestSecretResponseRedactionHeadersBeforeStreamCommit(t *testing.T) {
	const key = "opaque-stream-header-key"
	redact.Default.Update(key)
	success := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"" + key + "\"}\n\n"
	failure := "event: error\ndata: {\"error\":{\"message\":\"" + key + "\"}}\n\n"
	for _, mode := range []string{"write", "write_string", "write_header_now", "flush"} {
		t.Run(mode, func(t *testing.T) {
			response := httptest.NewRecorder()
			router := gin.New()
			router.Use(secretResponseRedaction())
			router.GET("/v1/responses", func(c *gin.Context) {
				c.Header("Content-Type", "text/event-stream")
				c.Header("X-Upstream-Diagnostic", "rejected "+key)
				c.Header("X-API-Key", "opaque-unregistered-stream-auth")
				c.Header("X-Request-ID", "public-request-id")
				switch mode {
				case "write":
					if _, err := c.Writer.WriteString(success); err != nil {
						t.Fatal(err)
					}
				case "write_string":
					if _, err := c.Writer.WriteString(success); err != nil {
						t.Fatal(err)
					}
				case "write_header_now":
					c.Writer.WriteHeaderNow()
				case "flush":
					c.Writer.Flush()
				}
				if got := response.Result().Header.Get("X-Upstream-Diagnostic"); strings.Contains(got, key) || !strings.HasPrefix(got, "rejected ") {
					t.Fatalf("first committed headers disclosed key: %q", got)
				}
				if got := response.Result().Header.Get("X-API-Key"); got != "[redacted]" {
					t.Fatalf("first committed authentication header disclosed key: %q", got)
				}
				if mode != "write" && mode != "write_string" {
					if _, err := c.Writer.WriteString(success); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := c.Writer.WriteString(failure); err != nil {
					t.Fatal(err)
				}
			})
			router.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/responses", http.NoBody))
			if response.Code != http.StatusOK || response.Result().Header.Get("X-Request-ID") != "public-request-id" {
				t.Fatalf("stream status or public header changed: %v", response.Result())
			}
			if !strings.HasPrefix(response.Body.String(), success) || strings.Contains(strings.TrimPrefix(response.Body.String(), success), key) {
				t.Fatalf("successful model output changed or failure leaked: %s", response.Body.String())
			}
		})
	}
}

func TestSecretResponseRedactionFlushesLeadingKeepalive(t *testing.T) {
	const key = "opaque-keepalive-metadata-key"
	redact.Default.Update(key)
	response := httptest.NewRecorder()
	router := gin.New()
	router.Use(secretResponseRedaction())
	router.GET("/stream", func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		for _, chunk := range []string{": keep", "alive\n"} {
			if _, err := c.Writer.WriteString(chunk); err != nil {
				t.Fatal(err)
			}
			c.Writer.Flush()
		}
		if !response.Flushed || response.Body.String() != ": keepalive\n" {
			t.Fatalf("keepalive not visible while handler remains open: %q", response.Body.String())
		}
		if _, err := c.Writer.WriteString(": diagnostic " + key + "\r\n"); err != nil {
			t.Fatal(err)
		}
		c.Writer.Flush()
		visible := response.Body.String()
		if strings.Contains(visible, key) || !strings.HasSuffix(visible, "\r\n") {
			t.Fatalf("comment metadata leaked or line ending changed: %q", visible)
		}
		failure := "event: error\ndata: {\"error\":{\"message\":\"" + key + "\"}}\n\n"
		cut := strings.Index(failure, key) + len(key)/2
		if _, err := c.Writer.WriteString(failure[:cut]); err != nil {
			t.Fatal(err)
		}
		c.Writer.Flush()
		if response.Body.String() != visible {
			t.Fatalf("partial diagnostic escaped buffer: %s", response.Body.String())
		}
		if _, err := c.Writer.WriteString(failure[cut:]); err != nil {
			t.Fatal(err)
		}
	})
	router.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/stream", http.NoBody))
	if strings.Contains(response.Body.String(), key) || !strings.Contains(response.Body.String(), "event: error\n") {
		t.Fatalf("completed diagnostic missing or exposed credential: %s", response.Body.String())
	}
}
