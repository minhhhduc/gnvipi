package providerstore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
)

// ProviderRow is one providers row. JSONB columns are raw bytes so the
// snapshot builder can validate their exact shape before decoding.
type ProviderRow struct {
	ID             string
	Name           string
	Protocol       string
	BaseURL        string
	ProxyURL       string
	Enabled        bool
	TLS            string
	EndpointPaths  []byte
	ForwardHeaders []byte
	Headers        []byte
	ExtraBody      []byte
	Extensions     []byte
	Timeouts       []byte
	MaxConcurrency int
	QueueTimeout   int64 // nanoseconds
}

// KeyRow is one provider_keys row. SecretEnc is ciphertext including the GCM
// tag; OwnerID is empty for shared keys.
type KeyRow struct {
	ID             string
	ProviderID     string
	OwnerID        string
	Name           string
	Enabled        bool
	AuthScheme     string
	AuthHeaderName string
	KeyEnv         string
	KeyRef         string
	SecretEnc      []byte
	SecretNonce    []byte
	SecretKVer     int
	SecretLast4    string
}

// ScopeOwner returns the generated scope_owner value used in the AAD.
func (k KeyRow) ScopeOwner() string {
	if k.OwnerID == "" {
		return ZeroScopeOwner
	}
	return k.OwnerID
}

// HasSecret reports whether an encrypted secret is stored.
func (k KeyRow) HasSecret() bool { return len(k.SecretEnc) > 0 }

// KeyModelRow is one key_models row.
type KeyModelRow struct {
	KeyID            string
	ProviderID       string
	OwnerID          string
	Alias            string
	UpstreamModel    string
	Hidden           bool
	Fallbacks        []byte
	FallbackStatuses []byte
	AllowShadow      bool
	LegacyPosition   int
}

// ModelStateRow is one model_state row.
type ModelStateRow struct {
	ModelID string
	Hidden  bool
	Deleted bool
}

// SettingRow is one settings row.
type SettingRow struct {
	Key   string
	Value []byte
}

// Rows is the complete configuration read in one snapshot transaction.
type Rows struct {
	Providers  []ProviderRow
	Keys       []KeyRow
	KeyModels  []KeyModelRow
	ModelState []ModelStateRow
	Settings   []SettingRow
}

// Settings holds the validated automode/cycle/search settings.
type Settings struct {
	Automode AutomodeSetting
	Cycle    CycleSetting
	Search   SearchSetting
}

type AutomodeSetting struct {
	Model         string `json:"model"`
	ChatResponses bool   `json:"chat_responses"`
}

type CycleSetting struct {
	Name   string   `json:"name"`
	Models []string `json:"models"`
}

type SearchSetting struct {
	URL string `json:"url"`
}

var (
	envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	headerPattern  = regexp.MustCompile(`^[!#$%&'*+\-.^_` + "`" + `|~0-9A-Za-z]+$`)
)

// BuildProviderDefinition decodes and validates one provider row into the
// existing gateway type. Credentials are not part of the definition and are
// materialized per key.
func BuildProviderDefinition(row ProviderRow) (provider.Provider, error) {
	endpointPaths, err := decodeEndpointPaths(row.EndpointPaths)
	if err != nil {
		return provider.Provider{}, rowErr(row.ID, "endpoint_paths", err)
	}
	forwardHeaders, err := decodeStringArray(row.ForwardHeaders)
	if err != nil {
		return provider.Provider{}, rowErr(row.ID, "forward_headers", err)
	}
	headers, err := decodeStringMap(row.Headers)
	if err != nil {
		return provider.Provider{}, rowErr(row.ID, "headers", err)
	}
	extraBody, err := decodeExtraBodyRaw(row.ExtraBody)
	if err != nil {
		return provider.Provider{}, rowErr(row.ID, "extra_body", err)
	}
	extensions, err := decodeExtensionMap(row.Extensions)
	if err != nil {
		return provider.Provider{}, rowErr(row.ID, "extensions", err)
	}
	timeouts, err := decodeTimeouts(row.Timeouts)
	if err != nil {
		return provider.Provider{}, rowErr(row.ID, "timeouts", err)
	}
	protocol, err := provider.ParseProtocol(row.Protocol)
	if err != nil {
		return provider.Provider{}, rowErr(row.ID, "protocol", err)
	}
	p := provider.Provider{
		ID:             row.ID,
		Name:           row.Name,
		BaseURL:        row.BaseURL,
		ProxyURL:       row.ProxyURL,
		Protocol:       protocol,
		Enabled:        row.Enabled,
		TLS:            provider.TLSProfile(row.TLS),
		EndpointPaths:  endpointPaths,
		ForwardHeaders: forwardHeaders,
		Headers:        headers,
		ExtraBody:      extraBody,
		Extensions:     extensions,
		Timeouts:       timeouts,
		MaxConcurrency: row.MaxConcurrency,
		QueueTimeout:   time.Duration(row.QueueTimeout),
	}
	if err := p.Validate(); err != nil {
		return provider.Provider{}, rowErr(row.ID, "provider", err)
	}
	// Header validation canonicalizes names in p.Headers; keep that result.
	return p, nil
}

func decodeEndpointPaths(raw []byte) (map[ir.Protocol]string, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var paths map[string]string
	if err := json.Unmarshal(raw, &paths); err != nil {
		return nil, fmt.Errorf("must be an object of protocol -> path: %w", err)
	}
	out := make(map[ir.Protocol]string, len(paths))
	for protocol, path := range paths {
		if _, err := ir.ParseProtocol(protocol); err != nil {
			return nil, fmt.Errorf("%q is not a wire protocol", protocol)
		}
		if err := provider.ValidateEndpointPath(path); err != nil {
			return nil, fmt.Errorf("%s: %w", protocol, err)
		}
		out[ir.Protocol(protocol)] = path
	}
	return out, nil
}

func decodeStringArray(raw []byte) ([]string, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("must be an array of strings: %w", err)
	}
	return values, nil
}

func decodeStringMap(raw []byte) (map[string]string, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var values map[string]string
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("must be an object of string values: %w", err)
	}
	return values, nil
}

func decodeExtraBodyRaw(raw []byte) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	if _, err := provider.DecodeExtraBody(json.RawMessage(raw)); err != nil {
		return nil, err
	}
	return append(json.RawMessage(nil), raw...), nil
}

func decodeExtensionMap(raw []byte) (map[string]json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("must be a JSON object: %w", err)
	}
	return values, nil
}

func decodeTimeouts(raw []byte) (provider.Timeouts, error) {
	var timeouts provider.Timeouts
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return timeouts, nil
	}
	if err := json.Unmarshal(raw, &timeouts); err != nil {
		return provider.Timeouts{}, err
	}
	for _, value := range []time.Duration{timeouts.Connect, timeouts.TLS, timeouts.ResponseHeader, timeouts.Idle, timeouts.Total} {
		if value < 0 || value > 24*time.Hour {
			return provider.Timeouts{}, fmt.Errorf("timeouts must be non-negative and at most 24h")
		}
	}
	return timeouts, nil
}

func rowErr(id, field string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: provider %q %s: %w", ErrInvalidConfig, id, field, err)
}

// ValidateKeyRow enforces the credential-source and scheme contracts before a
// row enters a snapshot. It never inspects plaintext.
func ValidateKeyRow(k KeyRow) error {
	if strings.TrimSpace(k.ID) == "" || strings.TrimSpace(k.ProviderID) == "" {
		return fmt.Errorf("%w: key id and provider id are required", ErrInvalidCredentialSource)
	}
	if k.AuthScheme != SchemeNone && k.AuthScheme != SchemeBearer && k.AuthScheme != SchemeHeader && k.AuthScheme != SchemeBasic {
		return fmt.Errorf("%w: unsupported auth_scheme %q", ErrInvalidCredentialSource, k.AuthScheme)
	}
	if k.OwnerID == "" && k.AuthScheme != SchemeNone {
		return fmt.Errorf("%w: shared keys must use auth_scheme none", ErrInvalidCredentialSource)
	}
	sources := 0
	if k.HasSecret() {
		sources++
	}
	if k.KeyEnv != "" {
		sources++
	}
	if k.KeyRef != "" {
		sources++
	}
	if sources > 1 {
		return fmt.Errorf("%w: key %s has %d credential sources", ErrInvalidCredentialSource, k.ID, sources)
	}
	if k.Enabled && k.AuthScheme != SchemeNone && sources != 1 {
		return fmt.Errorf("%w: enabled %s key %s has %d sources", ErrInvalidCredentialSource, k.AuthScheme, k.ID, sources)
	}
	if k.AuthScheme == SchemeNone && (sources != 0 || k.AuthHeaderName != "") {
		return fmt.Errorf("%w: none key %s must not carry credentials", ErrInvalidCredentialSource, k.ID)
	}
	if k.HasSecret() {
		if len(k.SecretNonce) != SecretNonceSize {
			return ErrInvalidNonce
		}
		if k.SecretKVer <= 0 {
			return fmt.Errorf("%w: key %s has key version %d", ErrUnknownKeyVersion, k.ID, k.SecretKVer)
		}
		if k.SecretLast4 == "" || len(k.SecretLast4) > 8 {
			return fmt.Errorf("%w: key %s has invalid secret_last4", ErrInvalidCredentialSource, k.ID)
		}
	} else if len(k.SecretNonce) != 0 || k.SecretKVer != 0 || k.SecretLast4 != "" {
		return fmt.Errorf("%w: key %s has partial secret metadata", ErrInvalidCredentialSource, k.ID)
	}
	if k.AuthScheme == SchemeHeader {
		if k.AuthHeaderName == "" || !headerPattern.MatchString(k.AuthHeaderName) {
			return fmt.Errorf("%w: header key %s requires a valid auth_header_name", ErrInvalidCredentialSource, k.ID)
		}
	} else if k.AuthHeaderName != "" {
		return fmt.Errorf("%w: key %s sets auth_header_name for scheme %s", ErrInvalidCredentialSource, k.ID, k.AuthScheme)
	}
	if k.KeyEnv != "" {
		if err := validateKeyEnvName(k.KeyEnv); err != nil {
			return fmt.Errorf("%w: key %s has invalid key_env: %w", ErrInvalidCredentialSource, k.ID, err)
		}
	}
	if k.KeyRef != "" {
		if len(k.KeyRef) > 512 || strings.ContainsAny(k.KeyRef, "\x00\r\n") {
			return fmt.Errorf("%w: key %s has invalid key_ref", ErrInvalidCredentialSource, k.ID)
		}
	}
	return nil
}

// ValidateKeyModelRow enforces alias, fallback and status contracts.
func ValidateKeyModelRow(m KeyModelRow) error {
	if strings.TrimSpace(m.KeyID) == "" || strings.TrimSpace(m.Alias) == "" {
		return fmt.Errorf("%w: key model requires key id and alias", ErrInvalidConfig)
	}
	if strings.ContainsAny(m.Alias, " \t\r\n") {
		return fmt.Errorf("%w: alias %q contains whitespace", ErrInvalidConfig, m.Alias)
	}
	if strings.Contains(m.Alias, "*") && (!strings.HasSuffix(m.Alias, "/*") || m.Alias == "/*") {
		return fmt.Errorf("%w: alias %q may only use a trailing /*", ErrInvalidConfig, m.Alias)
	}
	if m.UpstreamModel == "" && !strings.HasSuffix(m.Alias, "/*") {
		return fmt.Errorf("%w: exact alias %q requires upstream_model", ErrInvalidConfig, m.Alias)
	}
	fallbacks, err := decodeFallbacks(m.Fallbacks)
	if err != nil {
		return fmt.Errorf("%w: alias %q: %w", ErrInvalidConfig, m.Alias, err)
	}
	for _, fallback := range fallbacks {
		if fallback == m.Alias {
			return fmt.Errorf("%w: alias %q falls back to itself", ErrInvalidConfig, m.Alias)
		}
	}
	if _, err := decodeFallbackStatuses(m.FallbackStatuses); err != nil {
		return fmt.Errorf("%w: alias %q: %w", ErrInvalidConfig, m.Alias, err)
	}
	if m.LegacyPosition < 0 {
		return fmt.Errorf("%w: alias %q has negative legacy_position", ErrInvalidConfig, m.Alias)
	}
	return nil
}

func decodeFallbacks(raw []byte) ([]string, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var fallbacks []string
	if err := json.Unmarshal(raw, &fallbacks); err != nil {
		return nil, fmt.Errorf("fallbacks must be an array of strings: %w", err)
	}
	if len(fallbacks) > 16 {
		return nil, fmt.Errorf("fallbacks exceeds 16 entries")
	}
	seen := make(map[string]bool, len(fallbacks))
	for _, alias := range fallbacks {
		if alias == "" || strings.Contains(alias, "*") {
			return nil, fmt.Errorf("fallback %q must be an exact non-empty alias", alias)
		}
		if seen[alias] {
			return nil, fmt.Errorf("fallback %q is duplicated", alias)
		}
		seen[alias] = true
	}
	return fallbacks, nil
}

func decodeFallbackStatuses(raw []byte) ([]int, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var statuses []int
	if err := json.Unmarshal(raw, &statuses); err != nil {
		return nil, fmt.Errorf("fallback_statuses must be an array of integers: %w", err)
	}
	if len(statuses) > 32 {
		return nil, fmt.Errorf("fallback_statuses exceeds 32 entries")
	}
	seen := make(map[int]bool, len(statuses))
	for _, status := range statuses {
		if status < 100 || status > 599 {
			return nil, fmt.Errorf("fallback status %d is outside 100-599", status)
		}
		if seen[status] {
			return nil, fmt.Errorf("fallback status %d is duplicated", status)
		}
		seen[status] = true
	}
	return statuses, nil
}

// DecodeSettings validates the three settings rows and returns typed settings.
func DecodeSettings(rows []SettingRow) (Settings, error) {
	var settings Settings
	seen := make(map[string]bool, len(rows))
	for _, row := range rows {
		if seen[row.Key] {
			return Settings{}, fmt.Errorf("%w: duplicate setting %q", ErrInvalidConfig, row.Key)
		}
		seen[row.Key] = true
		switch row.Key {
		case "automode":
			if err := decodeSettingObject(row.Value, &settings.Automode); err != nil {
				return Settings{}, fmt.Errorf("%w: settings.automode: %w", ErrInvalidConfig, err)
			}
		case "cycle":
			if err := decodeSettingObject(row.Value, &settings.Cycle); err != nil {
				return Settings{}, fmt.Errorf("%w: settings.cycle: %w", ErrInvalidConfig, err)
			}
			if len(settings.Cycle.Models) == 0 {
				return Settings{}, fmt.Errorf("%w: settings.cycle.models must not be empty", ErrInvalidConfig)
			}
			seenModels := make(map[string]bool, len(settings.Cycle.Models))
			for _, model := range settings.Cycle.Models {
				if model == "" || seenModels[model] {
					return Settings{}, fmt.Errorf("%w: settings.cycle.models must be non-empty and unique", ErrInvalidConfig)
				}
				seenModels[model] = true
			}
		case "search":
			if err := decodeSettingObject(row.Value, &settings.Search); err != nil {
				return Settings{}, fmt.Errorf("%w: settings.search: %w", ErrInvalidConfig, err)
			}
			if settings.Search.URL != "" {
				parsed, err := url.Parse(settings.Search.URL)
				if err != nil || parsed.Scheme != "https" || !strings.Contains(settings.Search.URL, "{query}") {
					return Settings{}, fmt.Errorf("%w: settings.search.url must be HTTPS and contain {query}", ErrInvalidConfig)
				}
			}
		default:
			return Settings{}, fmt.Errorf("%w: unknown setting %q", ErrInvalidConfig, row.Key)
		}
	}
	return settings, nil
}

func decodeSettingObject(raw []byte, target any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return fmt.Errorf("must be a JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return nil
}

// SortedKeys returns key rows in deterministic order: provider, shared first,
// key id.
func SortedKeys(keys []KeyRow) []KeyRow {
	out := append([]KeyRow(nil), keys...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ProviderID != out[j].ProviderID {
			return out[i].ProviderID < out[j].ProviderID
		}
		if (out[i].OwnerID == "") != (out[j].OwnerID == "") {
			return out[i].OwnerID == ""
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// IsCredentialHeaderName reports a header that must never hold provider
// static configuration because it carries a credential.
func IsCredentialHeaderName(name string) bool {
	canonical := http.CanonicalHeaderKey(name)
	switch canonical {
	case "Authorization", "Proxy-Authorization", "Cookie", "Set-Cookie", "Set-Cookie2", "X-Api-Key":
		return true
	}
	lower := strings.ToLower(name)
	for _, needle := range []string{"token", "secret", "password", "credential"} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}
