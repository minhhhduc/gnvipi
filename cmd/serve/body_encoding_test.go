package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
)

func TestRequestBodyLimitCompressedInput(t *testing.T) {
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	short := []byte(`{"model":"test","input":"hi"}`)
	large := []byte(`{"model":"test","input":"` + strings.Repeat("x", 512) + `"}`)
	for _, tc := range []struct {
		name, encoding string
		wire, decoded  []byte
		status         int
	}{
		{"compressed overflow", "zstd", encoder.EncodeAll(large, nil), nil, http.StatusRequestEntityTooLarge},
		{"small compressed JSON", "zstd", encoder.EncodeAll(short, nil), short, http.StatusOK},
		{"encoding stack", "identity, ZSTD,zstd", encoder.EncodeAll(encoder.EncodeAll(short, nil), nil), short, http.StatusOK},
		{"mislabeled JSON keeps SDK fallback", "zstd", short, short, http.StatusOK},
		{"malformed compression", "zstd", []byte("not compressed JSON"), nil, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.wire) > 128 {
				t.Fatalf("wire fixture exceeds limit: %d", len(tc.wire))
			}
			engine := gin.New()
			engine.Use(requestBodyLimit(128, 64))
			engine.POST("/v1/responses", func(c *gin.Context) {
				body, readErr := io.ReadAll(c.Request.Body)
				if readErr != nil {
					t.Fatal(readErr)
				}
				c.Data(http.StatusOK, "application/json", body)
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(tc.wire))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Content-Encoding", tc.encoding)
			out := httptest.NewRecorder()
			engine.ServeHTTP(out, req)
			if out.Code != tc.status {
				t.Fatalf("status=%d want=%d: %s", out.Code, tc.status, out.Body.String())
			}
			if tc.status == http.StatusOK && !bytes.Equal(out.Body.Bytes(), tc.decoded) {
				t.Fatalf("consumer received encoded/wrong body: %q", out.Body.Bytes())
			}
		})
	}
}

func TestRequestBodyLimitSDKInferenceAliases(t *testing.T) {
	for _, path := range []string{"/backend-api/codex/responses", "/openai/v1/videos", "/v1beta/interactions"} {
		t.Run(path, func(t *testing.T) {
			engine := gin.New()
			engine.Use(requestBodyLimit(16, 8))
			engine.POST(path, func(c *gin.Context) { c.Status(http.StatusOK) })
			out := httptest.NewRecorder()
			engine.ServeHTTP(out, httptest.NewRequest(http.MethodPost, path, strings.NewReader(strings.Repeat("x", 17))))
			if out.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("SDK alias bypassed body limit: %d", out.Code)
			}
		})
	}
}
