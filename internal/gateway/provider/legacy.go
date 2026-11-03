package provider

import (
	"errors"
	"strconv"
	"strings"

	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
)

// LegacyProvider mirrors one entry of the pre-v2 admin format, which stored a
// single model per entry and encoded the protocol as two exclusive flags.
type LegacyProvider struct {
	Name          string `json:"name"`
	BaseURL       string `json:"base_url"`
	APIKey        string `json:"api_key"`
	Model         string `json:"model"`
	Alias         string `json:"alias,omitempty"`
	MessagesOnly  bool   `json:"messages_only,omitempty"`
	ResponsesOnly bool   `json:"responses_only,omitempty"`
}

// FromLegacy converts the endpoints saved by the legacy admin page. Entries
// that share a name, base URL, API key and protocol become one provider with
// one mapping per model; repeated aliases use separate providers so bare model
// routes survive. Different identities get collision-free numeric ID suffixes.
// Mapping positions preserve flat entry order independently of provider grouping.
//
// Every converted provider is validated, and the conversion returns a
// *ValidationError if any of them is invalid, including the case of an entry
// with both MessagesOnly and ResponsesOnly set.
func FromLegacy(list []LegacyProvider) ([]Provider, error) {
	type identity struct {
		name, baseURL, apiKey string
		protocol              ir.Protocol
	}

	out := make([]Provider, 0, len(list))
	merged := make(map[identity][]int, len(list))
	usedIDs := make(map[string]bool, len(list))
	reservedNames := make(map[string]bool, len(list))
	for _, e := range list {
		reservedNames[e.Name] = true
	}
	var errs []*FieldError

	for i := range list {
		e := &list[i]
		protocol, ok := legacyProtocol(e)
		if !ok {
			errs = append(errs, &FieldError{
				Provider: "legacy[" + strconv.Itoa(i) + "]",
				Field:    "protocol",
				Problem:  "messages_only and responses_only are mutually exclusive",
			})
			continue
		}
		mapping := ModelMapping{Alias: legacyAlias(e), UpstreamModel: e.Model, LegacyPosition: i + 1}
		key := identity{e.Name, e.BaseURL, e.APIKey, protocol}
		at := -1
		for _, candidate := range merged[key] {
			duplicate := false
			for _, m := range out[candidate].Models {
				if m.Alias == mapping.Alias {
					duplicate = true
					break
				}
			}
			if !duplicate {
				at = candidate
				break
			}
		}
		if at >= 0 {
			out[at].Models = append(out[at].Models, mapping)
			continue
		}

		id := nextLegacyID(e.Name, usedIDs, reservedNames)
		merged[key] = append(merged[key], len(out))
		out = append(out, Provider{
			ID:       id,
			Name:     e.Name,
			BaseURL:  e.BaseURL,
			Protocol: protocol,
			Auth:     legacyAuth(e.APIKey),
			Models:   []ModelMapping{mapping},
			Enabled:  true,
		})
	}

	for i := range out {
		var invalid *ValidationError
		if err := out[i].Validate(); errors.As(err, &invalid) {
			errs = append(errs, invalid.Errors...)
		}
	}
	if len(errs) > 0 {
		return nil, &ValidationError{Errors: errs}
	}
	return out, nil
}

func nextLegacyID(name string, usedIDs, reservedNames map[string]bool) string {
	id := name
	for suffix := 2; usedIDs[id] || (id != name && reservedNames[id]); suffix++ {
		id = name + "-" + strconv.Itoa(suffix)
	}
	usedIDs[id] = true
	return id
}

func legacyProtocol(e *LegacyProvider) (ir.Protocol, bool) {
	switch {
	case e.MessagesOnly && e.ResponsesOnly:
		return "", false
	case e.MessagesOnly:
		return ir.ProtocolAnthropicMessages, true
	case e.ResponsesOnly:
		return ir.ProtocolOpenAIResponses, true
	default:
		return ir.ProtocolOpenAIChat, true
	}
}

// legacyAuth keeps the legacy behavior: a configured key is sent as a bearer
// token, the same way CLIProxyAPI's api-key-entries are.
func legacyAuth(apiKey string) Auth {
	if apiKey == "" {
		return Auth{Scheme: AuthNone}
	}
	return Auth{Scheme: AuthBearer, Key: apiKey}
}

// legacyAlias reproduces customProvider.alias(): an explicit alias wins, then a
// model already namespaced by the provider name, otherwise name/model.
func legacyAlias(e *LegacyProvider) string {
	if e.Alias != "" {
		return e.Alias
	}
	prefix := e.Name + "/"
	if strings.HasPrefix(e.Model, prefix) {
		return e.Model
	}
	return prefix + e.Model
}
