package tokencount

import (
	"testing"
)

func TestEstimateText(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int
	}{
		{name: "empty", input: "", want: 0},
		{name: "short ascii word", input: "test", want: 1},
		{name: "five letter word", input: "hello", want: 2},
		{name: "two words", input: "hello world", want: 4},
		{name: "unicode chars", input: "你好世界", want: 4},
		{name: "mixed ascii and unicode", input: "hello世界", want: 4},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := EstimateText(tc.input)
			if got != tc.want {
				t.Fatalf("EstimateText(%q) = %d, want %d", tc.input, got, tc.want)
			}
		})
	}
}

func TestEstimateJSON(t *testing.T) {
	// String
	if got := EstimateJSON("hello"); got != 2 {
		t.Fatalf("EstimateJSON(string) = %d, want 2", got)
	}

	// Array adds 1 plus elements
	arr := []any{"hello", "world"}
	if got := EstimateJSON(arr); got != 5 {
		t.Fatalf("EstimateJSON(array) = %d, want 5", got)
	}

	// Map adds 2 plus elements, skipping model, role, type
	m := map[string]any{
		"model":   "claude-3",
		"role":    "user",
		"type":    "text",
		"content": "hello",
	}
	if got := EstimateJSON(m); got != 4 {
		t.Fatalf("EstimateJSON(map) = %d, want 4", got)
	}

	// Unhandled types (e.g. int, bool) return 0
	if got := EstimateJSON(12345); got != 0 {
		t.Fatalf("EstimateJSON(int) = %d, want 0", got)
	}
}

func TestEstimateRequest(t *testing.T) {
	// Invalid JSON returns error
	if _, err := EstimateRequest([]byte("invalid json")); err == nil {
		t.Fatal("expected error for invalid JSON")
	}

	// Empty JSON map returns minimum of 1
	got, err := EstimateRequest([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if got < 1 {
		t.Fatalf("expected at least 1, got %d", got)
	}

	// Standard request returns positive estimate
	payload := []byte(`{"model":"alias","messages":[{"role":"user","content":"explain this"}]}`)
	count, err := EstimateRequest(payload)
	if err != nil {
		t.Fatal(err)
	}
	if count <= 0 {
		t.Fatalf("expected positive count, got %d", count)
	}
}
