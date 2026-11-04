package routing

import "github.com/minhhhduc/gnvipi/internal/gateway/provider"

// Only the typed provider projection needs unique aliases. Resolve retains the
// original flat entries, including later duplicate aliases' bare-model routes.
func uniqueLegacyAliases(list []provider.LegacyProvider) []provider.LegacyProvider {
	type key struct {
		name, base, credential, alias string
		messages, responses           bool
	}
	seen := make(map[key]bool, len(list))
	out := make([]provider.LegacyProvider, 0, len(list))
	for i := range list {
		p := &list[i]
		identity := key{p.Name, p.BaseURL, p.APIKey, provider.LegacyAlias(p), p.MessagesOnly, p.ResponsesOnly}
		if seen[identity] {
			continue
		}
		seen[identity] = true
		out = append(out, *p)
	}
	return out
}
