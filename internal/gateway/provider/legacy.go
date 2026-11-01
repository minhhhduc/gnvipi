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
	Name          string
	BaseURL       string
	APIKey        string
	Model         string
	Alias         string
	MessagesOnly  bool
	ResponsesOnly bool
}

// FromLegacy converts the endpoints saved by the legacy admin page. Entries
// that share a name, base URL, API key and protocol become one provider with
// one mapping per model; entries that differ in any of those, including only in
// protocol, stay separate and get a numeric ID suffix. Providers are returned
// in the order their first entry appeared.
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
	merged := make(map[identity]int, len(list))
	usedIDs := make(map[string]int, len(list))
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
		mapping := ModelMapping{Alias: legacyAlias(e), UpstreamModel: e.Model}
		key := identity{e.Name, e.BaseURL, e.APIKey, protocol}
		if at, ok := merged[key]; ok {
			out[at].Models = append(out[at].Models, mapping)
			continue
		}

		id := e.Name
		if n := usedIDs[e.Name]; n > 0 {
			id = e.Name + "-" + strconv.Itoa(n+1)
		}
		usedIDs[e.Name]++
		merged[key] = len(out)
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
