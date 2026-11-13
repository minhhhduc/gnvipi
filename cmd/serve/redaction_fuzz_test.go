package main

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/minhhhduc/gnvipi/internal/gateway/redact"
)

func FuzzAdminSecurityRedaction(f *testing.F) {
	for _, secret := range []string{
		"sk-live-opaque-secret",
		"opaque secret/with?encoding+and=delimiters",
		`quote-\"-slash-\\-unicode-雪`,
		"[redacted]",
		"***",
		"a",
	} {
		f.Add(secret)
	}

	f.Fuzz(func(t *testing.T, secret string) {
		if secret == "" || len(secret) > 256 {
			t.Skip()
		}
		var r redact.Redactor
		r.Update(secret)
		encoded, err := json.Marshal(secret)
		if err != nil {
			t.Fatal(err)
		}
		variants := []string{
			secret,
			url.QueryEscape(secret),
			url.PathEscape(secret),
			string(encoded[1 : len(encoded)-1]),
		}
		for _, variant := range variants {
			got := r.Text("before<" + variant + ">after")
			if strings.Contains(got, variant) {
				t.Fatalf("text redaction retained %q in %q", variant, got)
			}
		}

		if !utf8.ValidString(secret) {
			return
		}
		raw, err := json.Marshal(map[string]any{
			"message":       "before<" + secret + ">after",
			"Authorization": secret,
			"nested":        []any{secret},
		})
		if err != nil {
			t.Fatal(err)
		}
		clean, err := r.JSON(raw)
		if err != nil {
			t.Fatal(err)
		}
		var decoded any
		if err := json.Unmarshal(clean, &decoded); err != nil {
			t.Fatalf("redacted JSON is invalid: %v (%s)", err, clean)
		}
		var assertClean func(any)
		assertClean = func(value any) {
			switch value := value.(type) {
			case string:
				if strings.Contains(value, secret) {
					t.Fatalf("JSON redaction retained %q in decoded value %q (%s)", secret, value, clean)
				}
			case []any:
				for _, item := range value {
					assertClean(item)
				}
			case map[string]any:
				for key, item := range value {
					assertClean(key)
					assertClean(item)
				}
			}
		}
		assertClean(decoded)
	})
}
