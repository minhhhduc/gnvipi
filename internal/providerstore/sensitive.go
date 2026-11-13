package providerstore

import (
	"encoding/json"
	"net/url"
	"sort"
	"strings"

	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
)

// ConfigProblem is one validation problem in a non-secret provider field.
type ConfigProblem struct {
	Field   string
	Problem string
}

// ValidateSensitiveConfig rejects plaintext credentials and credential-like
// configuration outside provider_keys. It returns deterministic, non-secret
// problems.
func ValidateSensitiveConfig(row ProviderRow, p provider.Provider) []ConfigProblem {
	var problems []ConfigProblem
	add := func(field, problem string) {
		problems = append(problems, ConfigProblem{Field: field, Problem: problem})
	}
	if raw := strings.TrimSpace(row.BaseURL); raw != "" {
		if parsed, err := url.Parse(raw); err == nil {
			if parsed.User != nil {
				add("base_url", "must not contain userinfo")
			}
			for key := range parsed.Query() {
				if credentialLikeKey(key) {
					add("base_url", "query parameter "+key+" looks like a credential")
				}
			}
		}
	}
	if raw := strings.TrimSpace(row.ProxyURL); raw != "" {
		if parsed, err := url.Parse(raw); err == nil && parsed.User != nil {
			add("proxy_url", "must not contain userinfo; proxy credentials come from the deployment environment")
		}
	}
	headerNames := make([]string, 0, len(p.Headers))
	for name := range p.Headers {
		headerNames = append(headerNames, name)
	}
	sort.Strings(headerNames)
	for _, name := range headerNames {
		if IsCredentialHeaderName(name) {
			add("headers["+name+"]", "credential headers belong in provider_keys")
		}
	}
	if len(p.ExtraBody) > 0 {
		if err := scanCredentialLikeKeys(p.ExtraBody, "extra_body", add); err != nil {
			add("extra_body", "must be a JSON object")
		}
	}
	if len(p.Extensions) > 0 {
		raw, err := json.Marshal(p.Extensions)
		if err == nil {
			if err := scanCredentialLikeKeys(raw, "extensions", add); err != nil {
				add("extensions", "must be a JSON object")
			}
		}
	}
	sort.SliceStable(problems, func(i, j int) bool {
		if problems[i].Field != problems[j].Field {
			return problems[i].Field < problems[j].Field
		}
		return problems[i].Problem < problems[j].Problem
	})
	return problems
}

func scanCredentialLikeKeys(raw json.RawMessage, path string, add func(field, problem string)) error {
	var value any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	var walk func(any, string)
	walk = func(node any, current string) {
		switch typed := node.(type) {
		case map[string]any:
			keys := make([]string, 0, len(typed))
			for key := range typed {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				child := current + "." + key
				if credentialLikeKey(key) {
					add(child, "credential-like keys belong in provider_keys")
				}
				walk(typed[key], child)
			}
		case []any:
			for _, item := range typed {
				walk(item, current)
			}
		}
	}
	walk(value, path)
	return nil
}

func credentialLikeKey(name string) bool {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch r {
		case '_', '-', '.', ' ', '\t':
			continue
		}
		b.WriteRune(r)
	}
	normalized := b.String()
	switch normalized {
	case "apikey", "authorization", "token", "accesstoken", "refreshtoken",
		"secret", "clientsecret", "password", "credential", "credentials":
		return true
	}
	for _, suffix := range []string{"apikey", "token", "secret", "password", "credential"} {
		if strings.HasSuffix(normalized, suffix) {
			return true
		}
	}
	return false
}

// RedactSecrets replaces every non-empty known secret in message. It is the
// only supported way to include upstream text in a diagnostic.
func RedactSecrets(message string, secrets ...string) string {
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		message = strings.ReplaceAll(message, secret, "[REDACTED]")
	}
	return message
}

// SanitizeTestDiagnostic redacts known secrets and truncates an upstream
// provider-test message to 512 runes. The bool reports truncation.
func SanitizeTestDiagnostic(message string, secrets ...string) (string, bool) {
	message = RedactSecrets(message, secrets...)
	const maxRunes = 512
	runes := []rune(message)
	if len(runes) <= maxRunes {
		return message, false
	}
	return string(runes[:maxRunes]), true
}

// MaskSecret returns a redacted display hint, e.g. "sk-...abcd".
func MaskSecret(secret string) string {
	return provider.MaskSecret(secret)
}
