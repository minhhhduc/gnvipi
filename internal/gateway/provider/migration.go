package provider

import (
	"cmp"
	"os"
	"slices"

	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
)

// LegacyView derives only enabled, visible wire-protocol mappings for the
// existing admin and passthrough consumers. V2 remains the source of truth;
// environment credentials are resolved for this runtime view, never persisted.
// Persisted mapping positions restore legacy precedence; unpositioned v2
// mappings follow in their normal provider/model order.
func LegacyView(list []Provider) []LegacyProvider {
	rows := legacyRows(list)
	if len(rows) == 0 {
		return nil
	}
	out := make([]LegacyProvider, len(rows))
	for i := range rows {
		out[i] = rows[i].entry
	}
	return out
}

type legacyRow struct {
	entry    LegacyProvider
	owner    int
	position int
}

func legacyRows(list []Provider) []legacyRow {
	var rows []legacyRow
	for i := range list {
		p := &list[i]
		if !p.Enabled || !legacyRepresentable(p.Protocol) {
			continue
		}
		key := legacyKey(p)
		for _, m := range p.Models {
			if !m.Hidden {
				rows = append(rows, legacyRow{entry: LegacyProvider{Name: p.Name, BaseURL: p.BaseURL, APIKey: key,
					Model: m.UpstreamModel, Alias: m.Alias,
					MessagesOnly:  p.Protocol == ir.ProtocolAnthropicMessages,
					ResponsesOnly: p.Protocol == ir.ProtocolOpenAIResponses}, owner: i, position: m.LegacyPosition})
			}
		}
	}
	slices.SortStableFunc(rows, func(a, b legacyRow) int {
		if a.position == 0 && b.position != 0 {
			return 1
		}
		if b.position == 0 && a.position != 0 {
			return -1
		}
		return cmp.Compare(a.position, b.position)
	})
	return rows
}

func legacyKey(p *Provider) string {
	if p.Auth.KeyEnv != "" {
		return os.Getenv(p.Auth.KeyEnv)
	}
	return p.Auth.Key
}

func legacyRepresentable(protocol ir.Protocol) bool {
	return protocol == ir.ProtocolOpenAIChat || protocol == ir.ProtocolOpenAIResponses || protocol == ir.ProtocolAnthropicMessages
}

// MergeLegacy reconciles original rows before grouping new entries, so providers
// with identical legacy endpoint identities keep their distinct IDs/v2 settings.
// Edits with indistinguishable owners fail rather than silently discard settings.
// Disabled providers, hidden mappings and non-wire protocols remain untouched.
func MergeLegacy(current []Provider, list []LegacyProvider) ([]Provider, error) {
	list = slices.Clone(list)
	for i := range list {
		list[i].Alias = legacyAlias(&list[i])
	}
	if slices.Equal(LegacyView(current), list) {
		out := make([]Provider, len(current))
		for i := range current {
			out[i] = Clone(&current[i])
		}
		return out, nil
	}
	rows := legacyRows(current)
	owners := make([]int, len(list))
	used := make([]bool, len(rows))
	counts := make(map[LegacyProvider]int, len(list))
	for _, e := range list {
		counts[e]++
	}
	for i, e := range list {
		owners[i] = -1
		match, count, owner, ambiguous := -1, 0, -1, false
		for j, row := range rows {
			if row.entry != e {
				continue
			}
			count++
			if owner >= 0 && owner != row.owner {
				ambiguous = true
			}
			owner = row.owner
			if match < 0 && !used[j] {
				match = j
			}
		}
		if ambiguous && counts[e] < count {
			return nil, &FieldError{Provider: e.Name, Field: "models", Problem: "legacy rows have indistinguishable provider owners"}
		}
		if match >= 0 {
			owners[i], used[match] = rows[match].owner, true
		}
	}
	// Resolve strong model matches before weaker endpoint/name matches, so a
	// newly inserted row cannot consume an existing row that was edited later.
	for priority := 4; priority >= 1; priority-- {
		for i, e := range list {
			if owners[i] >= 0 {
				continue
			}
			match, best, owner, ambiguous := -1, 0, -1, false
			for j, row := range rows {
				old := row.entry
				if used[j] || old.Name != e.Name || old.MessagesOnly != e.MessagesOnly || old.ResponsesOnly != e.ResponsesOnly {
					continue
				}
				score := 1
				if old.BaseURL == e.BaseURL && old.APIKey == e.APIKey {
					score = 2
				}
				if old.Alias == e.Alias || old.Model == e.Model {
					score = 3
				}
				if old.Alias == e.Alias && old.Model == e.Model {
					score = 4
				}
				if score > best {
					match, best, owner, ambiguous = j, score, row.owner, false
				} else if score == best && row.owner != owner {
					ambiguous = true
				}
			}
			if best != priority {
				continue
			}
			if ambiguous {
				return nil, &FieldError{Provider: e.Name, Field: "models", Problem: "legacy edit matches multiple v2 providers"}
			}
			owners[i], used[match] = owner, true
		}
	}
	for i, e := range list {
		if owners[i] >= 0 {
			continue
		}
		// A new model can join a uniquely identified existing endpoint, but
		// endpoint equality alone cannot choose between distinct v2 providers.
		owner := -1
		for _, row := range rows {
			old := row.entry
			if old.Name == e.Name && old.BaseURL == e.BaseURL && old.APIKey == e.APIKey && old.MessagesOnly == e.MessagesOnly && old.ResponsesOnly == e.ResponsesOnly {
				if owner >= 0 && owner != row.owner {
					return nil, &FieldError{Provider: e.Name, Field: "models", Problem: "new legacy model matches multiple v2 providers"}
				}
				owner = row.owner
			}
		}
		owners[i] = owner
	}
	grouped := make([][]LegacyProvider, len(current)+1)
	positions := make([][]int, len(grouped))
	for i, owner := range owners {
		if owner < 0 {
			owner = len(current)
		}
		grouped[owner] = append(grouped[owner], list[i])
		positions[owner] = append(positions[owner], i+1)
	}
	var out []Provider
	ids, names := make(map[string]bool), make(map[string]bool)
	for i := range current {
		ids[current[i].ID], names[current[i].Name] = true, true
	}
	for _, e := range list {
		names[e.Name] = true
	}
	for owner, entries := range grouped {
		converted, err := FromLegacy(entries)
		if err != nil {
			return nil, err
		}
		for i := range converted {
			for j := range converted[i].Models {
				m := &converted[i].Models[j]
				m.LegacyPosition = positions[owner][m.LegacyPosition-1]
			}
		}
		if owner == len(current) {
			for i := range converted {
				converted[i].ID = nextLegacyID(converted[i].Name, ids, names)
				out = append(out, converted[i])
			}
			continue
		}
		old := &current[owner]
		if len(converted) == 0 {
			retained := Clone(old)
			if old.Enabled && legacyRepresentable(old.Protocol) && len(old.Models) > 0 {
				retained.Models = nil
				for _, m := range old.Models {
					if m.Hidden {
						retained.Models = append(retained.Models, m)
					}
				}
				if len(retained.Models) == 0 {
					continue
				}
			}
			out = append(out, retained)
			continue
		}
		// Endpoint splits inherit settings, but only one group retains the old
		// ID and hidden mappings; prefer the unchanged original endpoint.
		primary := 0
		for i := range converted {
			if converted[i].BaseURL == old.BaseURL && converted[i].Auth.Key == legacyKey(old) {
				primary = i
				break
			}
		}
		for i := range converted {
			next := &converted[i]
			retained := Clone(old)
			retained.Name, retained.BaseURL = next.Name, next.BaseURL
			if legacyKey(old) != next.Auth.Key {
				if next.Auth.Key == "" {
					retained.Auth = Auth{Scheme: AuthNone}
				} else {
					if retained.Auth.Scheme == "" || retained.Auth.Scheme == AuthNone {
						retained.Auth.Scheme = next.Auth.Scheme
					}
					retained.Auth.Key, retained.Auth.KeyEnv = next.Auth.Key, ""
				}
			}
			retained.Models = next.Models
			if i == primary {
				models := make([]ModelMapping, 0, len(next.Models)+len(old.Models))
				at := 0
				for _, m := range old.Models {
					if m.Hidden {
						models = append(models, m)
					} else if at < len(next.Models) {
						models = append(models, next.Models[at])
						at++
					}
				}
				models = append(models, next.Models[at:]...)
				retained.Models = models
			} else {
				retained.ID = nextLegacyID(next.Name, ids, names)
			}
			out = append(out, retained)
		}
	}
	return out, nil
}
