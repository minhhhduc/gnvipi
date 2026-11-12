package providerstore

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
)

func cleanDefinition(t *testing.T) provider.Provider {
	t.Helper()
	definition, err := BuildProviderDefinition(validProviderRow("p1", true))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return definition
}

func TestValidateSensitiveConfigClean(t *testing.T) {
	row := validProviderRow("p1", true)
	row.BaseURL = "https://api.example.com/v1?region=us"
	definition := cleanDefinition(t)
	definition.Headers = map[string]string{"X-Client": "gnvipi"}
	definition.ExtraBody = json.RawMessage(`{"metadata":{"team":"core"}}`)
	if problems := ValidateSensitiveConfig(row, definition); len(problems) != 0 {
		t.Fatalf("problems = %+v", problems)
	}
}

func TestValidateSensitiveConfigUserinfo(t *testing.T) {
	row := validProviderRow("p1", true)
	row.BaseURL = "https://user:pass@api.example.com/v1"
	row.ProxyURL = "http://proxy-user:proxy-pass@proxy.example.com:8080"
	problems := ValidateSensitiveConfig(row, cleanDefinition(t))
	if len(problems) != 2 {
		t.Fatalf("problems = %+v", problems)
	}
	if problems[0].Field != "base_url" || problems[1].Field != "proxy_url" {
		t.Fatalf("unexpected fields: %+v", problems)
	}
	for _, problem := range problems {
		if strings.Contains(problem.Problem, "pass") {
			t.Fatalf("problem leaks credential: %+v", problem)
		}
	}
}

func TestValidateSensitiveConfigQueryCredential(t *testing.T) {
	row := validProviderRow("p1", true)
	row.BaseURL = "https://api.example.com/v1?api_key=leaked"
	problems := ValidateSensitiveConfig(row, cleanDefinition(t))
	if len(problems) != 1 || !strings.Contains(problems[0].Problem, "api_key") {
		t.Fatalf("problems = %+v", problems)
	}
}

func TestValidateSensitiveConfigCredentialHeaders(t *testing.T) {
	definition, err := BuildProviderDefinition(validProviderRow("p1", true))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	definition.Headers = map[string]string{
		"Authorization":   "Bearer leaked",
		"X-Api-Key":       "leaked",
		"X-Custom-Token":  "leaked",
		"X-Client-Secret": "leaked",
		"X-Trace":         "ok",
	}
	problems := ValidateSensitiveConfig(ProviderRow{ID: "p1"}, definition)
	if len(problems) != 4 {
		t.Fatalf("problems = %+v", problems)
	}
	for _, problem := range problems {
		if strings.Contains(problem.Problem, "leaked") {
			t.Fatalf("problem leaks value: %+v", problem)
		}
	}
}

func TestValidateSensitiveConfigNestedCredentialKeys(t *testing.T) {
	definition, err := BuildProviderDefinition(validProviderRow("p1", true))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	definition.ExtraBody = json.RawMessage(`{"auth":{"api_key":"leaked","nested":[{"password":"leaked"}]}}`)
	definition.Extensions = map[string]json.RawMessage{
		"policy": json.RawMessage(`{"client_secret":"leaked"}`),
	}
	problems := ValidateSensitiveConfig(ProviderRow{ID: "p1"}, definition)
	if len(problems) != 3 {
		t.Fatalf("problems = %+v", problems)
	}
	joined := formatProblems(problems)
	for _, field := range []string{"extra_body.auth.api_key", "extra_body.auth.nested.password", "extensions.policy.client_secret"} {
		if !strings.Contains(joined, field) {
			t.Fatalf("missing %q in %q", field, joined)
		}
	}
}

func TestRedactSecrets(t *testing.T) {
	got := RedactSecrets("upstream said key sk-abc failed with sk-abc", "sk-abc")
	if strings.Contains(got, "sk-abc") || strings.Count(got, "[REDACTED]") != 2 {
		t.Fatalf("redact = %q", got)
	}
	if got := RedactSecrets("no secret here", ""); got != "no secret here" {
		t.Fatalf("empty secret changed message: %q", got)
	}
}

func TestSanitizeTestDiagnosticTruncates(t *testing.T) {
	long := strings.Repeat("a", 600)
	got, truncated := SanitizeTestDiagnostic(long, "secret")
	if !truncated || len([]rune(got)) != 512 {
		t.Fatalf("truncated=%v len=%d", truncated, len([]rune(got)))
	}
	if got, truncated := SanitizeTestDiagnostic("bad secret secret", "secret"); truncated || strings.Contains(got, "secret") {
		t.Fatalf("got %q truncated=%v", got, truncated)
	}
}

func TestIsCredentialHeaderName(t *testing.T) {
	for _, name := range []string{"authorization", "Proxy-Authorization", "x-api-key", "X-Refresh-Token", "X-Client-Secret", "Password"} {
		if !IsCredentialHeaderName(name) {
			t.Fatalf("%q should be a credential header", name)
		}
	}
	for _, name := range []string{"X-Client", "Accept", "User-Agent"} {
		if IsCredentialHeaderName(name) {
			t.Fatalf("%q should not be a credential header", name)
		}
	}
}
