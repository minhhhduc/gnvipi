package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	gatewayprovider "github.com/minhhhduc/gnvipi/internal/gateway/provider"
)

func TestV2PreferencePublicationFiltersUnadvertisedCredentials(t *testing.T) {
	key := "opaque-v2-load-credential"
	p := gatewayprovider.Provider{ID: "disabled", Name: "disabled", BaseURL: "https://up.example/v1", Protocol: ir.ProtocolOpenAIChat, Auth: gatewayprovider.Auth{Scheme: gatewayprovider.AuthBearer, Key: key}, Models: []gatewayprovider.ModelMapping{{Alias: "disabled/m", UpstreamModel: "m"}}}
	path := filepath.Join(t.TempDir(), "models.json")
	if err := gatewayprovider.WriteDocument(path, gatewayprovider.Document{Providers: []gatewayprovider.Provider{p}}); err != nil {
		t.Fatal(err)
	}
	m := &modelPrefs{}
	if err := m.load(path); err != nil {
		t.Fatal(err)
	}
	engine := gin.New()
	engine.Use(secretResponseRedaction())
	engine.GET("/error", func(c *gin.Context) {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": "upstream echoed " + key}})
	})
	assertFiltered := func() {
		response := httptest.NewRecorder()
		request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/error", http.NoBody)
		engine.ServeHTTP(response, request)
		if response.Code != http.StatusBadGateway || strings.Contains(response.Body.String(), key) {
			t.Fatalf("credential escaped through error response: status=%d body=%s", response.Code, response.Body)
		}
	}
	assertFiltered()
	key = "opaque-v2-env-credential"
	t.Setenv("TEST_V2_PROVIDER_CREDENTIAL", key)
	p.Auth = gatewayprovider.Auth{Scheme: gatewayprovider.AuthBearer, KeyEnv: "TEST_V2_PROVIDER_CREDENTIAL"}
	m.onChange = func([]customProvider) { assertFiltered() }
	if err := m.upsertProviderV2(&p); err != nil {
		t.Fatal(err)
	}
	assertFiltered()
}
