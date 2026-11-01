package main

import "testing"

func TestTokenPreviewDoesNotPanicOnShortTokens(t *testing.T) {
	tests := []struct {
		name  string
		token string
		want  string
	}{
		{"empty", "", ""},
		{"short", "abc", "abc"},
		{"exactly 30", "012345678901234567890123456789", "012345678901234567890123456789"},
		{"long", "0123456789012345678901234567890123", "012345678901234567890123456789"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tokenPreview(tt.token); got != tt.want {
				t.Fatalf("tokenPreview(%q) = %q, want %q", tt.token, got, tt.want)
			}
		})
	}
}
