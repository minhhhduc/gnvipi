package main

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"strings"
	"time"

	"github.com/minhhhduc/gnvipi/internal/captcha"
	"github.com/minhhhduc/gnvipi/internal/models"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func waitPoolReady(ctx context.Context, pool *captcha.Pool, want int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(400 * time.Millisecond)
	defer ticker.Stop()
	for {
		if pool.Ready() >= want {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("captcha pool still empty after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func bindNvidiaRuntime(core *coreauth.Manager, exec coreauth.ProviderExecutor, catalog []*cliproxy.ModelInfo) int {
	if core == nil || exec == nil {
		return 0
	}
	core.RegisterExecutor(exec)
	n := 0
	for _, a := range core.List() {
		if a == nil || !strings.EqualFold(a.Provider, nvidiaProvider) {
			continue
		}
		cliproxy.GlobalModelRegistry().RegisterClient(a.ID, nvidiaProvider, catalog)
		core.RefreshSchedulerEntry(a.ID)
		n++
	}
	if n == 0 {
		cliproxy.GlobalModelRegistry().RegisterClient(nvidiaAuthFileName, nvidiaProvider, catalog)
	}
	return n
}

// ensureNvidiaAuth registers an in-memory auth when AuthDir load produced none.
func ensureNvidiaAuth(core *coreauth.Manager) {
	if core == nil {
		return
	}
	for _, a := range core.List() {
		if a != nil && strings.EqualFold(a.Provider, nvidiaProvider) {
			return
		}
	}
	if _, err := core.Register(coreauth.WithSkipPersist(context.Background()), &coreauth.Auth{
		ID:       nvidiaAuthFileName,
		Provider: nvidiaProvider,
		Status:   coreauth.StatusActive,
	}); err != nil {
		log.Printf("warning: register nvidia auth: %v", err)
	}
}

// startNvidiaReconciler heals races with watcher model/executor registration.
// Fast for the first 90s after startup, then a slow heartbeat forever.
func startNvidiaReconciler(ctx context.Context, core *coreauth.Manager, exec coreauth.ProviderExecutor, getModels func() []*cliproxy.ModelInfo) {
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		slow := false
		fastUntil := time.Now().Add(90 * time.Second)
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("nvidia reconciler: recovered from panic: %v", r)
						}
					}()
					ensureNvidiaAuth(core)
					bindNvidiaRuntime(core, exec, getModels())
				}()
				if !slow && now.After(fastUntil) {
					ticker.Reset(5 * time.Second)
					slow = true
				}
			}
		}
	}()
}

// nvidiaAuthHook restores executor+models whenever cliproxy registers/updates nvidia auth.
type nvidiaAuthHook struct {
	coreauth.NoopHook
	core      *coreauth.Manager
	exec      coreauth.ProviderExecutor
	getModels func() []*cliproxy.ModelInfo
}

func (h *nvidiaAuthHook) OnAuthRegistered(_ context.Context, auth *coreauth.Auth) {
	h.rebind(auth)
}

func (h *nvidiaAuthHook) OnAuthUpdated(_ context.Context, auth *coreauth.Auth) {
	h.rebind(auth)
}

func (h *nvidiaAuthHook) rebind(auth *coreauth.Auth) {
	if h == nil || auth == nil || !strings.EqualFold(auth.Provider, nvidiaProvider) {
		return
	}
	if h.exec != nil && h.core != nil {
		h.core.RegisterExecutor(h.exec)
	}
	if h.getModels != nil {
		catalog := h.getModels()
		if len(catalog) > 0 {
			cliproxy.GlobalModelRegistry().RegisterClient(auth.ID, nvidiaProvider, catalog)
		}
	}
	if h.core != nil {
		h.core.RefreshSchedulerEntry(auth.ID)
	}
}

// nvidiaModelHook restores catalog when registerModelsForAuth UnregisterClient's nvidia.
type nvidiaModelHook struct {
	core      *coreauth.Manager
	exec      coreauth.ProviderExecutor
	getModels func() []*cliproxy.ModelInfo
}

func (h *nvidiaModelHook) OnModelsRegistered(context.Context, string, string, []*cliproxy.ModelInfo) {
}

func (h *nvidiaModelHook) OnModelsUnregistered(_ context.Context, provider, clientID string) {
	if h == nil || h.getModels == nil || clientID == "" {
		return
	}
	if provider != "" && !strings.EqualFold(provider, nvidiaProvider) {
		return
	}
	if h.exec != nil && h.core != nil {
		h.core.RegisterExecutor(h.exec)
	}
	catalog := h.getModels()
	if len(catalog) > 0 {
		cliproxy.GlobalModelRegistry().RegisterClient(clientID, nvidiaProvider, catalog)
	}
	if h.core != nil {
		h.core.RefreshSchedulerEntry(clientID)
	}
}

// verifiedPlaygroundCandidates are known healthy playground pages that mount
// the hCaptcha widget quickly and reliably.
var verifiedPlaygroundCandidates = []string{
	"https://build.nvidia.com/deepseek-ai/deepseek-v4.1-flash/playground",
	"https://build.nvidia.com/nvidia/nemotron-3.5-lightning-30b-a3b/playground",
	"https://build.nvidia.com/nvidia/nemotron-3.5-super-vl-120b-a12b/playground",
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
