package models

import "testing"

func TestLookupDefault(t *testing.T) {
	info, err := Lookup("")
	if err != nil {
		t.Fatalf("Lookup(\"\"): %v", err)
	}
	if info.Slug != "nemotron-3.5-lightning-30b-a3b" {
		t.Fatalf("default slug = %q want nemotron-3.5-lightning-30b-a3b", info.Slug)
	}
	if info.FunctionID != "0a213807-640b-43fb-bfbf-2919f9b666ad" {
		t.Fatalf("default function id = %q want 281478d0-…", info.FunctionID)
	}
}

func TestLookupKnown(t *testing.T) {
	cases := map[string]string{
		"z-ai/glm-5-3-flash":                    "glm-5-3-flash",
		"nvidia/nemotron-3.5-lightning-30b-a3b": "nemotron-3.5-lightning-30b-a3b",
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
	info, _ := Lookup("nvidia/nemotron-3.5-lightning-30b-a3b")
	want := "https://buildapi.ngc.nvidia.com/v2/predict/models/" + Namespace + "/nemotron-3.5-lightning-30b-a3b"
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
