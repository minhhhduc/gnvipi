package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestAdminDeleteRestorePreservesIndependentMasks(t *testing.T) {
	for _, tc := range []struct {
		name          string
		hiddenBefore  bool
		hideWhileGone bool
	}{
		{name: "enabled model is restored"},
		{name: "existing user mask survives", hiddenBefore: true},
		{name: "user mask added while deleted survives", hideWhileGone: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := prefs
			t.Cleanup(func() { prefs = old })
			const id = "restore/model"
			p := &modelPrefs{
				path:      filepath.Join(t.TempDir(), "models.json"),
				providers: []customProvider{{Name: "restore", BaseURL: "https://upstream.invalid/v1", Model: "upstream-model", Alias: id}},
			}
			if tc.hiddenBefore {
				if err := p.setHidden([]string{id}); err != nil {
					t.Fatal(err)
				}
			}
			fakeUpstream(t, `{"id":"reply","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
			var engine *gin.Engine
			mount := func(next *modelPrefs) {
				prefs = next
				engine = mountChain(t, next, func(c *gin.Context) { c.Status(http.StatusTeapot) })
				for _, path := range []string{"/admin/delete", "/admin/models"} {
					engine.POST(path, func(c *gin.Context) { adminRoutes(c, nil) })
				}
			}
			post := func(path, body string, want int) {
				t.Helper()
				req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				out := httptest.NewRecorder()
				engine.ServeHTTP(out, req)
				if out.Code != want {
					t.Fatalf("POST %s = %d, want %d: %s", path, out.Code, want, out.Body.String())
				}
			}
			mount(p)
			post("/admin/delete", fmt.Sprintf(`{"id":%q}`, id), http.StatusOK)
			chat := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hello"}]}`, id)
			post("/v1/chat/completions", chat, http.StatusNotFound)
			if tc.hideWhileGone {
				post("/admin/models", fmt.Sprintf(`{"masks_id":[%q]}`, id), http.StatusOK)
			}
			var deleted modelPrefs
			deleted.load(p.path)
			mount(&deleted)
			post("/v1/chat/completions", chat, http.StatusNotFound)
			post("/admin/delete", fmt.Sprintf(`{"id":%q,"restore":true}`, id), http.StatusOK)
			want := http.StatusOK
			if tc.hiddenBefore || tc.hideWhileGone {
				want = http.StatusNotFound
			}
			post("/v1/chat/completions", chat, want)
			var restored modelPrefs
			restored.load(p.path)
			mount(&restored)
			post("/v1/chat/completions", chat, want)
		})
	}
}
