package models

import (
	"errors"
	"testing"
)

const (
	testModel = "acme/synthetic-model"
	testSlug  = "synthetic-model"
)

func seedRegistry(t *testing.T, entries map[string]ModelInfo) {
	t.Helper()
	saved := Snapshot()
	t.Cleanup(func() { Replace(saved) })
	Replace(entries)
}

func TestRegistryStartsEmpty(t *testing.T) {
	seedRegistry(t, map[string]ModelInfo{})
	if got := len(Snapshot()); got != 0 {
		t.Fatalf("registry entries = %d, want 0", got)
	}
	_, err := Lookup("")
	var unknown *ErrUnknownModel
	if !errors.As(err, &unknown) {
		t.Fatalf("Lookup(\"\") error = %v, want *ErrUnknownModel", err)
	}
}

func TestLookupKnown(t *testing.T) {
	seedRegistry(t, map[string]ModelInfo{
		testModel: {Slug: testSlug, Namespace: Namespace, FunctionID: testFunctionID},
	})
	info, err := Lookup(testModel)
	if err != nil {
		t.Fatalf("Lookup(%q): %v", testModel, err)
	}
	if info.Slug != testSlug {
		t.Errorf("Lookup(%q) slug = %q want %q", testModel, info.Slug, testSlug)
	}
	if info.Namespace != Namespace {
		t.Errorf("Lookup(%q) namespace = %q want %q", testModel, info.Namespace, Namespace)
	}
}

func TestLookupUnknown(t *testing.T) {
	seedRegistry(t, map[string]ModelInfo{})
	_, err := Lookup("no-such-org/no-such-model")
	if err == nil {
		t.Fatal("expected error for unknown model")
	}
	var uerr *ErrUnknownModel
	if !errors.As(err, &uerr) {
		t.Fatalf("error type = %T want *ErrUnknownModel", err)
	}
	if uerr.Model != "no-such-org/no-such-model" {
		t.Errorf("err model = %q", uerr.Model)
	}
}

func TestPredictEndpoint(t *testing.T) {
	seedRegistry(t, map[string]ModelInfo{
		testModel: {Slug: testSlug, Namespace: Namespace, FunctionID: testFunctionID},
	})
	info, err := Lookup(testModel)
	if err != nil {
		t.Fatal(err)
	}
	want := "https://buildapi.ngc.nvidia.com/v2/predict/models/" + Namespace + "/" + testSlug
	if got := info.PredictEndpoint(); got != want {
		t.Fatalf("PredictEndpoint() = %q want %q", got, want)
	}
}

func TestReplaceIsolatesSnapshots(t *testing.T) {
	seedRegistry(t, map[string]ModelInfo{
		testModel: {Slug: testSlug, Namespace: Namespace, FunctionID: testFunctionID},
	})
	snapshot := Snapshot()
	snapshot["acme/added-later"] = ModelInfo{Slug: "added-later", Namespace: Namespace, FunctionID: testFunctionID}
	if _, ok := Get("acme/added-later"); ok {
		t.Fatal("mutating a snapshot changed the registry")
	}
}
