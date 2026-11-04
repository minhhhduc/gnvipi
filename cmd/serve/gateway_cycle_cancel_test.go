package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestGatewayV2CycleCancellationRecordsOnce(t *testing.T) {
	before := cycleModelStats()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cancel()
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer upstream.Close()
	prefs := &modelPrefs{providers: []customProvider{{Name: "vendor", Model: "first", BaseURL: upstream.URL}}, cycle: &cycleConfig{Name: "cycle", Models: []string{"vendor/first"}}}
	dispatcher, err := gatewayV2Middleware(prefs, true, "")
	if err != nil {
		t.Fatal(err)
	}
	engine := gin.New()
	engine.Use(gatewayRequestContext(), cycleRouter(prefs, engine), dispatcher)
	engine.POST("/v1/chat/completions", func(c *gin.Context) { t.Error("canceled owned request reached downstream") })
	out := httptest.NewRecorder()
	engine.ServeHTTP(out, httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"cycle","messages":[]}`)))
	after := cycleModelStats()
	if after.Requests-before.Requests != 1 || after.Errors-before.Errors != 1 {
		t.Fatalf("canceled cycle completion was missing or duplicated: before=%+v after=%+v", before, after)
	}
}
