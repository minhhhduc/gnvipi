package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/models"
)

func stubNvidiaDiscovery(t *testing.T, discover func(context.Context) ([]models.PlaygroundGroup, error)) {
	t.Helper()
	old := discoverNvidiaCatalog
	discoverNvidiaCatalog = discover
	t.Cleanup(func() { discoverNvidiaCatalog = old })
}

func rescanFixture(rows ...models.PlaygroundRow) func(context.Context) ([]models.PlaygroundGroup, error) {
	return func(context.Context) ([]models.PlaygroundGroup, error) {
		byPublisher := make(map[string][]models.PlaygroundRow)
		for _, row := range rows {
			publisher, _, _ := strings.Cut(row.Model, "/")
			byPublisher[publisher] = append(byPublisher[publisher], row)
		}
		groups := make([]models.PlaygroundGroup, 0, len(byPublisher))
		for publisher, entries := range byPublisher {
			groups = append(groups, models.PlaygroundGroup{Publisher: publisher, Models: entries})
		}
		return groups, nil
	}
}

func setupRescanPrefs(t *testing.T) *modelPrefs {
	t.Helper()
	saved := models.Snapshot()
	t.Cleanup(func() { models.Replace(saved) })
	return setupTestPrefs(t)
}

func TestNvidiaRescanReconcilesCatalog(t *testing.T) {
	p := setupRescanPrefs(t)
	models.Replace(map[string]models.ModelInfo{
		"acme/keep":  {Slug: "keep", Namespace: "qc69jvmznzxy", FunctionID: "11111111-1111-1111-1111-111111111111"},
		"acme/keep2": {Slug: "keep2", Namespace: "qc69jvmznzxy", FunctionID: "22222222-2222-2222-2222-222222222222"},
		"acme/gone":  {Slug: "gone", Namespace: "qc69jvmznzxy", FunctionID: "33333333-3333-3333-3333-333333333333"},
	})
	if err := p.setHidden([]string{"acme/keep", "acme/gone"}); err != nil {
		t.Fatalf("set hidden: %v", err)
	}
	if err := p.addDeleted("acme/keep2"); err != nil {
		t.Fatalf("add deleted: %v", err)
	}
	if err := p.addDeleted("acme/gone"); err != nil {
		t.Fatalf("add deleted: %v", err)
	}

	stubNvidiaDiscovery(t, rescanFixture(
		models.PlaygroundRow{Model: "acme/keep", Slug: "keep", Namespace: "qc69jvmznzxy", FunctionID: "11111111-1111-1111-1111-111111111111"},
		models.PlaygroundRow{Model: "acme/keep2", Slug: "keep2", Namespace: "qc69jvmznzxy", FunctionID: "99999999-9999-9999-9999-999999999999"},
		models.PlaygroundRow{Model: "acme/new", Slug: "new", Namespace: "qc69jvmznzxy", FunctionID: "44444444-4444-4444-4444-444444444444"},
	))

	result, err := p.rescanNvidiaCatalog(context.Background())
	if err != nil {
		t.Fatalf("rescan: %v", err)
	}
	if result.Added != 1 || result.Updated != 1 || result.Removed != 1 {
		t.Fatalf("result = %+v, want added=1 updated=1 removed=1", result)
	}
	if result.ScannedAt.IsZero() {
		t.Fatal("scanned_at is zero")
	}
	if _, err := models.Lookup("acme/new"); err != nil {
		t.Fatalf("new model not routable: %v", err)
	}
	updated, err := models.Lookup("acme/keep2")
	if err != nil || updated.FunctionID != "99999999-9999-9999-9999-999999999999" {
		t.Fatalf("updated model = %+v, %v", updated, err)
	}
	var unknown *models.ErrUnknownModel
	if _, err := models.Lookup("acme/gone"); !errors.As(err, &unknown) {
		t.Fatalf("removed model lookup = %v, want *ErrUnknownModel", err)
	}
	if !p.isHidden("acme/keep") {
		t.Fatal("surviving hidden flag was dropped")
	}
	if !p.isDeleted("acme/keep2") {
		t.Fatal("surviving deleted flag was dropped")
	}
	if p.isHidden("acme/gone") || p.isDeleted("acme/gone") {
		t.Fatal("flags for a removed model were kept")
	}

	raw, err := os.ReadFile(p.path)
	if err != nil {
		t.Fatalf("read catalog file: %v", err)
	}
	var file prefsFile
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("decode catalog file: %v", err)
	}
	ids := make(map[string]bool)
	for _, group := range file.Groups {
		for _, item := range group.Models {
			ids[item.Model] = true
		}
	}
	if !ids["acme/new"] || ids["acme/gone"] {
		t.Fatalf("persisted catalog = %v", ids)
	}
	if !reflect.DeepEqual(file.MasksID, []string{"acme/keep"}) {
		t.Fatalf("masks = %v", file.MasksID)
	}
	if !reflect.DeepEqual(file.DeletedID, []string{"acme/keep2"}) {
		t.Fatalf("deleted = %v", file.DeletedID)
	}

	second, err := p.rescanNvidiaCatalog(context.Background())
	if err != nil {
		t.Fatalf("second rescan: %v", err)
	}
	if second.Added != 0 || second.Updated != 0 || second.Removed != 0 {
		t.Fatalf("second scan = %+v, want zeros", second)
	}
}

func TestNvidiaRescanFailureLeavesCatalogUntouched(t *testing.T) {
	p := setupRescanPrefs(t)
	models.Replace(map[string]models.ModelInfo{
		"acme/keep": {Slug: "keep", Namespace: "qc69jvmznzxy", FunctionID: "11111111-1111-1111-1111-111111111111"},
	})
	if err := p.setHidden([]string{"acme/keep"}); err != nil {
		t.Fatalf("set hidden: %v", err)
	}
	beforeRegistry := models.Snapshot()
	beforeFile, err := os.ReadFile(p.path)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}

	stubNvidiaDiscovery(t, func(context.Context) ([]models.PlaygroundGroup, error) {
		return nil, &models.DiscoveryError{Op: "playground page", Err: errors.New("status 503")}
	})
	_, err = p.rescanNvidiaCatalog(context.Background())
	var discoveryErr *models.DiscoveryError
	if !errors.As(err, &discoveryErr) {
		t.Fatalf("error = %v, want *DiscoveryError", err)
	}
	if !reflect.DeepEqual(models.Snapshot(), beforeRegistry) {
		t.Fatal("failed scan changed the registry")
	}
	afterFile, err := os.ReadFile(p.path)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if !bytes.Equal(afterFile, beforeFile) {
		t.Fatal("failed scan changed the catalog file")
	}
	if !p.isHidden("acme/keep") {
		t.Fatal("failed scan changed the masks")
	}
}

func TestNvidiaRescanHandlerSummaryAndError(t *testing.T) {
	p := setupRescanPrefs(t)
	models.Replace(map[string]models.ModelInfo{})
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/admin/api/nvidia/models/rescan", handleAdminNvidiaRescan(p, nil))

	const canary = "captcha-canary-9f2b"
	stubNvidiaDiscovery(t, rescanFixture(
		models.PlaygroundRow{Model: "acme/" + canary, Slug: canary, Namespace: "qc69jvmznzxy", FunctionID: "55555555-5555-5555-5555-555555555555"},
	))
	res := makeRequest(t, engine, "POST", "/admin/api/nvidia/models/rescan", nil, nil)
	if res.Code != 200 {
		t.Fatalf("rescan = %d: %s", res.Code, res.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["added"] != float64(1) || body["updated"] != float64(0) || body["removed"] != float64(0) {
		t.Fatalf("body = %v", body)
	}
	if _, ok := body["scanned_at"]; !ok {
		t.Fatalf("body = %v, want scanned_at", body)
	}
	if strings.Contains(res.Body.String(), canary) {
		t.Fatalf("response leaked a model token: %s", res.Body.String())
	}

	stubNvidiaDiscovery(t, func(context.Context) ([]models.PlaygroundGroup, error) {
		return nil, &models.DiscoveryError{Op: "probe", Err: errors.New(canary)}
	})
	res = makeRequest(t, engine, "POST", "/admin/api/nvidia/models/rescan", nil, nil)
	if res.Code != 502 {
		t.Fatalf("failed rescan = %d, want 502: %s", res.Code, res.Body.String())
	}
	if strings.Contains(res.Body.String(), canary) {
		t.Fatalf("error response leaked the upstream detail: %s", res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "nvidia_catalog_unavailable") {
		t.Fatalf("error body = %s", res.Body.String())
	}
}
