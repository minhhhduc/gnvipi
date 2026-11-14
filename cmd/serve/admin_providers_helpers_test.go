package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"

	gatewayprovider "github.com/minhhhduc/gnvipi/internal/gateway/provider"
)

func TestProviderCheckViolationBecomesFieldError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	if !dbCheckFieldError(c, &pgconn.PgError{Code: "23514", ConstraintName: "providers_name_check"}) {
		t.Fatal("providers_name_check was not mapped to a field error")
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["field"] != "name" {
		t.Fatalf("field = %v, want name (body %s)", body["field"], rec.Body.String())
	}
	other, _ := gin.CreateTestContext(httptest.NewRecorder())
	if dbCheckFieldError(other, errors.New("boom")) {
		t.Fatal("a non-check error was mapped to a field error")
	}
}

func TestSameModelMappingsTreatsNilAndEmptySlicesAlike(t *testing.T) {
	a := []gatewayprovider.ModelMapping{{
		Alias: "a", UpstreamModel: "m", Fallbacks: []string{}, FallbackStatuses: []int{},
	}}
	b := []gatewayprovider.ModelMapping{{Alias: "a", UpstreamModel: "m"}}
	if !sameModelMappings(a, b) {
		t.Fatal("empty and nil fallback slices should compare equal")
	}
	b[0].UpstreamModel = "other"
	if sameModelMappings(a, b) {
		t.Fatal("different upstream models compared equal")
	}
	if sameModelMappings(a, append(b, gatewayprovider.ModelMapping{Alias: "extra", UpstreamModel: "m"})) {
		t.Fatal("different lengths compared equal")
	}
}
