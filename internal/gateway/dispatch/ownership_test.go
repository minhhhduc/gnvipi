package dispatch

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/gateway/routing"
)

func TestUnknownOwnershipLeavesValidationToLegacy(t *testing.T) {
	resolver, err := routing.NewLegacyResolver(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"broken":`, `{"messages":[]}`, `{"model":null}`, `{"model":17}`} {
		engine := gin.New()
		engine.Use(New(resolver, nil).Handle)
		engine.POST("/v1/chat/completions", func(c *gin.Context) {
			original, err := io.ReadAll(c.Request.Body)
			if err != nil {
				t.Fatal(err)
			}
			c.Data(http.StatusOK, "application/json", original)
		})
		out := httptest.NewRecorder()
		engine.ServeHTTP(out, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
		if out.Code != http.StatusOK || out.Body.String() != body {
			t.Fatalf("dispatcher claimed an unowned invalid request: %d %q", out.Code, out.Body.String())
		}
	}
}
