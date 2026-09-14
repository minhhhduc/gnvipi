package models

import "testing"

func TestLookupDefault(t *testing.T) {
	info, err := Lookup("")
	if err != nil {
		t.Fatalf("Lookup(\"\"): %v", err)
	}
	if info.Slug != "deepseek-v4-flash-0731" {
		t.Fatalf("default slug = %q want deepseek-v4-flash-0731", info.Slug)
	}
	if info.FunctionID != "281478d0-f307-49f4-9e0f-080b63b16c47" {
		t.Fatalf("default function id = %q want 281478d0-…", info.FunctionID)
	}
}

func TestLookupKnown(t *testing.T) {
	cases := map[string]string{
		"deepseek-ai/deepseek-v4-pro":       "deepseek-v4-pro",
		"nvidia/nemotron-3-ultra-550b-a55b": "nemotron-3-ultra-550b-a55b",
	}
	for model, wantSlug := range cases {
		info, err := Lookup(model)
		if err != nil {
			t.Errorf("Lookup(%q): %v", model, err)
			continue
		}
		if info.Slug != wantSlug {
			t.Errorf("Lookup(%q) slug = %q want %q", model, info.Slug, wantSlug)
		}
		if info.Namespace != Namespace {
			t.Errorf("Lookup(%q) namespace = %q want %q", model, info.Namespace, Namespace)
		}
	}
}

func TestLookupUnknown(t *testing.T) {
	_, err := Lookup("no-such-org/no-such-model")
	if err == nil {
		t.Fatal("expected error for unknown model")
	}
	uerr, ok := err.(*ErrUnknownModel)
	if !ok {
		t.Fatalf("error type = %T want *ErrUnknownModel", err)
	}
	if uerr.Model != "no-such-org/no-such-model" {
		t.Errorf("err model = %q", uerr.Model)
	}
}

func TestPredictEndpoint(t *testing.T) {
	info, _ := Lookup("deepseek-ai/deepseek-v4-pro")
	want := "https://buildapi.ngc.nvidia.com/v2/predict/models/" + Namespace + "/deepseek-v4-pro"
	if got := info.PredictEndpoint(); got != want {
		t.Fatalf("PredictEndpoint() = %q want %q", got, want)
	}
}

// Registry invariants: every entry has a UUID-shaped function id and the shared
// namespace. Function ids are *usually* unique per model, but NVIDIA does alias
// some backend versions to the same NVCF function (e.g. the ising-calibration
// variants share 499210d3…). We log duplicates instead of failing so a legit
// alias is not mistaken for a scrape bug; the endpoint path (namespace/slug) is
// what actually distinguishes models, and that IS unique per registry key.

func uuid42(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				return false
			}
		}
	}
	return true
}
