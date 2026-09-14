package main

import (
	"math/rand"

	"glm52-nvidia/internal/models"
)

// verifiedPlaygroundCandidates are known healthy playground pages that mount
// the hCaptcha widget quickly and reliably.
var verifiedPlaygroundCandidates = []string{
	"https://build.nvidia.com/deepseek-ai/deepseek-v4-flash-0731/playground",
	"https://build.nvidia.com/deepseek-ai/deepseek-v4-pro-0813/playground",
	"https://build.nvidia.com/deepseek-ai/deepseek-v4-pro/playground",
}

// playgroundCandidates returns up to n playground URLs. It prioritizes verified
// playground pages and samples active (non-deleted) models from the registry.
func playgroundCandidates(n int) []string {
	if n <= 0 {
		return nil
	}
	out := make([]string, 0, n)
	seen := make(map[string]bool)

	for _, u := range verifiedPlaygroundCandidates {
		if len(out) >= n {
			break
		}
		if !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}

	type pair struct{ pub, slug, fullID string }
	var all []pair
	for _, g := range models.AllGroups() {
		for _, m := range g.Models {
			if m.Slug == "" {
				continue
			}
			fullID := g.Publisher + "/" + m.Slug
			if prefs != nil && (prefs.isDeleted(fullID) || prefs.isHidden(fullID)) {
				continue
			}
			all = append(all, pair{g.Publisher, m.Slug, fullID})
		}
	}

	if len(all) > 0 {
		rand.Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })
		for _, p := range all {
			if len(out) >= n {
				break
			}
			u := "https://build.nvidia.com/" + p.pub + "/" + p.slug + "/playground"
			if !seen[u] {
				seen[u] = true
				out = append(out, u)
			}
		}
	}

	return out
}
