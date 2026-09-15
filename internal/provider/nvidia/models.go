package nvidia

import (
	"sort"
	"strings"

	"github.com/minhhhduc/gnvipi/internal/models"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy"
)

// DefaultAliasModel returns the playground model used to serve requests whose
// model field is a built-in Claude name (see cmd/serve claudeDefaultModelAlias).
func DefaultAliasModel() string { return models.DefaultModel }

// RegistryModels returns cliproxy ModelInfo entries for every playground model.
func RegistryModels() []*cliproxy.ModelInfo {
	ids := make([]string, 0, len(models.Models))
	for id := range models.Models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]*cliproxy.ModelInfo, 0, len(ids))
	for _, id := range ids {
		org, _, _ := strings.Cut(id, "/")
		out = append(out, &cliproxy.ModelInfo{
			ID:      id,
			Object:  "model",
			Created: 0,
			OwnedBy: org,
			Type:    providerKey,
		})
	}
	return out
}
