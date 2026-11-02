package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRequestBodyLimitBoundaries(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, path  string
		size, limit int
		declared    int64
		status      int
		anthropic   bool
	}{
		{"below", "/v1/chat/completions", 15, 16, 15, http.StatusOK, false},
		{"exact unknown length", "/v1/chat/completions", 16, 16, -1, http.StatusOK, false},
		{"declared overflow", "/v1/chat/completions", 17, 16, 17, http.StatusRequestEntityTooLarge, false},
		{"unknown overflow", "/v1/chat/completions", 17, 16, -1, http.StatusRequestEntityTooLarge, false},
		{"underreported overflow", "/v1/chat/completions", 17, 16, 1, http.StatusRequestEntityTooLarge, false},
		{"messages envelope", "/v1/messages", 17, 16, -1, http.StatusRequestEntityTooLarge, true},
		{"messages trailing slash", "/v1/messages/", 17, 16, 17, http.StatusRequestEntityTooLarge, true},
		{"token count envelope", "/v1/messages/count_tokens", 17, 16, -1, http.StatusRequestEntityTooLarge, true},
		{"protocol prefix boundary", "/v1/messages-extra", 17, 16, -1, http.StatusRequestEntityTooLarge, false},
		{"responses envelope", "/v1/responses", 17, 16, -1, http.StatusRequestEntityTooLarge, false},
		{"v1 root", "/v1", 17, 16, -1, http.StatusRequestEntityTooLarge, false},
		{"admin exact", "/admin/providers", 8, 8, -1, http.StatusOK, false},
		{"admin overflow", "/admin/providers", 9, 8, 9, http.StatusRequestEntityTooLarge, false},
		{"admin root", "/admin", 9, 8, -1, http.StatusRequestEntityTooLarge, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := strings.Repeat("x", tc.size)
			body := &bodyLimitReadCloser{Reader: strings.NewReader(payload)}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, tc.path, body)
			req.ContentLength = tc.declared
			called := false
			engine := gin.New()
			engine.Use(requestBodyLimit(16, 8))
			engine.POST(tc.path, func(c *gin.Context) {
				called = true
				if body.closes != 0 {
					t.Fatal("request body closed before consumer")
				}
				got, err := io.ReadAll(c.Request.Body)
				if err != nil {
					t.Fatal(err)
				}
				if err := c.Request.Body.Close(); err != nil {
					t.Fatal(err)
				}
				c.Data(http.StatusOK, "application/octet-stream", got)
			})
			out := httptest.NewRecorder()
			engine.ServeHTTP(out, req)
			if out.Code != tc.status {
				t.Fatalf("status=%d want=%d: %s", out.Code, tc.status, out.Body.String())
			}
			if tc.status == http.StatusOK {
				if !called || out.Body.String() != payload || body.closes != 1 {
					t.Fatalf("consumer body or Close ownership changed: %q, closes=%d", out.Body.String(), body.closes)
				}
				return
			}
			if called {
				t.Fatal("oversized body reached consumer")
			}
			assertBodyLimitError(t, out, tc.anthropic)
			if tc.declared > int64(tc.limit) && body.readBytes != 0 {
				t.Fatal("read despite oversized Content-Length")
			}
			if body.readBytes > tc.limit+1 {
				t.Fatalf("unbounded read: %d bytes", body.readBytes)
			}
			if body.closes != 0 {
				t.Fatal("guard closed body owned by request/server")
			}
			if err := req.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if body.closes != 1 {
				t.Fatal("request owner could not close original body")
			}
		})
	}
}

func TestRequestBodyLimitLeavesUnrelatedRoutesUntouched(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, path := range []string{"/v10/messages", "/v1other", "/administrator", "/adminish/providers", "/mcp", "/healthz"} {
		t.Run(path, func(t *testing.T) {
			payload := strings.Repeat("x", 32)
			body := &bodyLimitReadCloser{Reader: strings.NewReader(payload)}
			engine := gin.New()
			engine.Use(requestBodyLimit(16, 8))
			engine.POST(path, func(c *gin.Context) {
				if body.readBytes != 0 || c.Request.Body != body {
					t.Fatal("middleware touched an unrelated route's body")
				}
				got, err := io.ReadAll(c.Request.Body)
				if err != nil {
					t.Fatal(err)
				}
				c.Data(http.StatusOK, "application/octet-stream", got)
			})
			w := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, body)
			req.ContentLength = int64(len(payload))
			engine.ServeHTTP(w, req)
			if w.Code != http.StatusOK || w.Body.String() != payload {
				t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
			}
		})
	}
}

func TestRequestBodyLimitRejectsReadFailureBeforeDownstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses", "/admin/providers"} {
		t.Run(path, func(t *testing.T) {
			body := &bodyLimitReadCloser{Reader: io.MultiReader(strings.NewReader("partial"), bodyLimitErrorReader{})}
			called := false
			engine := gin.New()
			engine.Use(requestBodyLimit(16, 8))
			engine.POST(path, func(c *gin.Context) { called = true; c.Status(http.StatusOK) })
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, body))
			if called || w.Code != http.StatusBadRequest {
				t.Fatalf("called=%v status=%d body=%s", called, w.Code, w.Body.String())
			}
			assertBodyLimitError(t, w, path == "/v1/messages")
			if strings.Contains(w.Body.String(), "private transport detail") {
				t.Fatal("response leaked the body reader's error")
			}
		})
	}
}

func TestRequestBodyLimitEmptyBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, body := range []io.ReadCloser{nil, http.NoBody} {
		engine := gin.New()
		engine.Use(requestBodyLimit(16, 8))
		engine.GET("/admin", func(c *gin.Context) { c.String(http.StatusOK, "admin") })
		w := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/admin", nil)
		req.Body = body
		engine.ServeHTTP(w, req)
		if w.Code != http.StatusOK || w.Body.String() != "admin" {
			t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
		}
	}
}

func assertBodyLimitError(t *testing.T, w *httptest.ResponseRecorder, anthropic bool) {
	t.Helper()
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type=%q", got)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("invalid JSON error: %v", err)
	}
	var detail map[string]json.RawMessage
	if err := json.Unmarshal(envelope["error"], &detail); err != nil {
		t.Fatalf("invalid error object: %v", err)
	}
	if string(detail["type"]) != `"invalid_request_error"` {
		t.Fatalf("error type=%s", detail["type"])
	}
	var message string
	if err := json.Unmarshal(detail["message"], &message); err != nil || message == "" {
		t.Fatalf("missing error message: %s", w.Body.String())
	}
	if anthropic {
		if string(envelope["type"]) != `"error"` {
			t.Fatalf("missing Anthropic envelope: %s", w.Body.String())
		}
		return
	}
	if _, ok := envelope["type"]; ok {
		t.Fatalf("OpenAI error has Anthropic envelope: %s", w.Body.String())
	}
	if _, ok := detail["code"]; !ok || string(detail["param"]) != "null" {
		t.Fatalf("missing OpenAI code/param fields: %s", w.Body.String())
	}
}

type bodyLimitReadCloser struct {
	io.Reader
	readBytes int
	closes    int
}

func (b *bodyLimitReadCloser) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.readBytes += n
	return n, err
}

func (b *bodyLimitReadCloser) Close() error {
	b.closes++
	return nil
}

type bodyLimitErrorReader struct{}

func (bodyLimitErrorReader) Read([]byte) (int, error) {
	return 0, errors.New("private transport detail")
}
