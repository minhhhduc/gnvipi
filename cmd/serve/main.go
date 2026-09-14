// cmd/serve — multi-format OpenAI/Claude/Responses gateway for NVIDIA playground.
//
// Embeds CLIProxyAPI with a custom nvidia ProviderExecutor. Upstream predict is
// already OpenAI Chat Completions shape; builtin translators expose:
//
//	POST /v1/chat/completions
//	POST /v1/responses
//	POST /v1/messages
//
// No inbound gateway API keys. Captcha via -auto pool, -captcha, or nv-captcha-token.
//
// Usage:
//
//	go run ./cmd/serve -auto
//	go run ./cmd/serve -auto -pool-size=2 -pool-workers=1 -coalesce-ms=0 -max-inflight=8
//	go run ./cmd/serve -captcha "P1_..."
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"glm52-nvidia/internal/captcha"
	"glm52-nvidia/internal/provider/nvidia"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"

	_ "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator/builtin"
)

// Set via -ldflags "-X main.version=v1.2.3" at release build time.
var version = "dev"

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	captchaFlag := flag.String("captcha", "", "one-shot hCaptcha token (consumed on first use)")
	auto := flag.Bool("auto", false, "prewarm captcha tokens via shared Chrome + pool")
	poolSize := flag.Int("pool-size", 3, "ready captcha tokens to keep buffered (-auto)")
	poolWorkers := flag.Int("pool-workers", 1, "concurrent captcha extractors / Chrome processes (-auto); each worker owns one Chrome")
	maxInflight := flag.Int("max-inflight", 4, "max concurrent upstream streams (0=unlimited)")
	inflightWait := flag.Duration("inflight-wait", 500*time.Millisecond, "how long to wait for an in-flight slot before returning 503 (0=reject immediately)")
	coalesceMs := flag.Int("coalesce-ms", 16, "merge consecutive SSE content deltas within this window (0=off); first token always flushes immediately")
	warmTimeout := flag.Duration("warm-timeout", 3*time.Minute, "wait for at least one pooled captcha before serving (-auto); 0=skip")
	poolTTL := flag.Duration("pool-ttl", 90*time.Second, "discard pooled captcha tokens older than this (-auto)")
	captchaWait := flag.Duration("captcha-wait", 30*time.Second, "max wait for a pooled captcha token per request (0=block until ready); then 503")
	modelsFile := flag.String("models-file", "internal/models/playground_models.json", "path to save hidden/deleted models and custom endpoints")
	chromeProxy := flag.String("chrome-proxy", "", "proxy for captcha Chrome and upstream API (e.g. socks5://host:port); falls back to CHROME_PROXY")
	captchaHarness := flag.Bool("captcha-harness", false, "mint tokens on a local data: HTML harness page instead of a real playground URL (lower RAM; requires -pool-batch to realise speedup)")
	poolBatch := flag.Int("pool-batch", 1, "tokens to mint per sticky-tab borrow (1=single; requires -captcha-harness=true for speedup)")
	captchaSelectBudget := flag.Duration("captcha-select-budget", 0, "if >0, run a sliding-window benchmark across playground candidates at startup to pick the fastest minting URL (persists to ~/.cache/glm52-nvidia/playground-state.json)")
	flag.Parse()

	if *modelsFile != "" {
		prefs.load(*modelsFile)
	}

	if !*auto && *captchaFlag == "" {
		log.Print("warning: no -auto/-captcha; each request must send nv-captcha-token")
	}

	proxyURL := strings.TrimSpace(*chromeProxy)
	if proxyURL == "" {
		proxyURL = strings.TrimSpace(os.Getenv("CHROME_PROXY"))
	}
	proxyFunc := http.ProxyFromEnvironment
	if proxyURL != "" {
		u, err := url.Parse(proxyURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			log.Fatalf("chrome-proxy: invalid URL %q", proxyURL)
		}
		proxyFunc = http.ProxyURL(u)
		log.Printf("upstream + captcha proxy=%s", proxyURL)
	}

	transport := &http.Transport{
		Proxy: proxyFunc,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   64,
		MaxConnsPerHost:       0,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var (
		browser *captcha.BrowserGroup
		pool    *captcha.Pool
	)
	// urlProvider picks which playground page hosts the hCaptcha widget. The
	// default is a single hardcoded URL; -captcha-select-budget > 0 runs a
	// sliding-window benchmark across candidates and persists a champion to
	// disk so restarts skip the benchmark.
	var urlProvider captcha.URLProvider = captcha.NewDefaultURLProvider()
	if *captchaSelectBudget > 0 {
		candidates := playgroundCandidates(5)
		log.Printf("captcha: running champion selection (%d candidates, budget=%s)",
			len(candidates), *captchaSelectBudget)
		state, err := captcha.SelectChampion(ctx, candidates, *captchaSelectBudget)
		if err != nil {
			log.Printf("warning: champion selection failed (%v); falling back to default URL", err)
		} else {
			urlProvider = state
			log.Printf("captcha: champion=%s score=%dms", state.ChampionURL, state.ScoreMS)
		}
	} else if existing := captcha.LoadChampion(captcha.ChampionStateFile()); existing.ChampionURL != "" {
		urlProvider = existing
		log.Printf("captcha: loaded champion=%s from disk", existing.ChampionURL)
	}
	if *auto {
		browserCfg := captcha.BrowserConfig{Proxy: proxyURL, URLProvider: urlProvider}
		if *captchaHarness {
			championURL := urlProvider.PlaygroundURL(ctx)
			sitekey, err := captcha.ScrapeSitekey(ctx, championURL)
			if err != nil {
				log.Printf("warning: harness sitekey scrape failed (%v); falling back to playground mode", err)
			} else {
				browserCfg.HarnessPage = captcha.HarnessPageFor(sitekey, *poolBatch)
				log.Printf("captcha: harness mode sitekey=%s batch=%d (RAM ~50MB/chrome)", sitekey, *poolBatch)
			}
		}
		var err error
		browser, err = captcha.NewBrowserGroup(ctx, *poolWorkers, browserCfg)
		if err != nil {
			log.Fatalf("captcha browser: %v", err)
		}
		chromeAdmin = browser // /admin/chromes pause/resume surface
	}
	if browser != nil {
		batch := *poolBatch
		if batch < 1 {
			batch = 1
		}
		if !*captchaHarness && batch > 1 {
			log.Printf("captcha: -pool-batch=%d clamped to 1 in playground mode (batching requires -captcha-harness=true to avoid worker starvation)", batch)
			batch = 1
		}
		poolCfg := captcha.PoolConfig{
			Size:    *poolSize,
			Workers: *poolWorkers,
			TTL:     *poolTTL,
		}
		if batch > 1 {
			poolCfg.BatchSize = batch
			poolCfg.ExtractN = browser.ExtractN
		}
		pool = captcha.NewPool(ctx, browser.Extract, poolCfg)
		defer func() {
			pool.Close()
			browser.Close()
		}()
		log.Printf("captcha pool: size=%d workers=%d chromes=%d batch=%d ttl=%s captcha-wait=%s",
			*poolSize, *poolWorkers, browser.Len(), batch, *poolTTL, *captchaWait)

		if *warmTimeout > 0 {
			log.Printf("warming captcha pool (timeout=%s)…", *warmTimeout)
			if err := waitPoolReady(ctx, pool, 1, *warmTimeout); err != nil {
				log.Printf("warning: %v — first requests may block on captcha extract", err)
			} else {
				log.Printf("captcha pool ready=%d (TTFT path unblocked)", pool.Ready())
			}
		}
	}

	exec := nvidia.NewExecutor(nvidia.Options{
		Auto:         *auto,
		FlagCaptcha:  *captchaFlag,
		Coalesce:     time.Duration(*coalesceMs) * time.Millisecond,
		MaxInflight:  *maxInflight,
		InflightWait: *inflightWait,
		CaptchaWait:  *captchaWait,
		HTTPClient:   &http.Client{Timeout: 0, Transport: transport},
		Pool:         pool,
	})

	cfg, cfgPath, err := buildConfig(*addr)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	defer os.RemoveAll(cfg.AuthDir)

	// Register /admin custom endpoints (e.g. OpenRouter) as OpenAI-compat
	// providers so CLIProxyAPI routes their aliases.
	cfg.OpenAICompatibility = compatProviders(prefs.listProviders())
	if err := rewriteConfigYAML(cfgPath, cfg.OpenAICompatibility); err != nil {
		log.Printf("warning: could not write initial config for providers: %v", err)
	}
	prefs.onChange = func(list []customProvider) {
		cfg.OpenAICompatibility = compatProviders(list)
		if err := rewriteConfigYAML(cfgPath, cfg.OpenAICompatibility); err != nil {
			log.Printf("warning: could not rewrite config for providers: %v", err)
		}
	}

	tokenStore := sdkAuth.GetTokenStore()
	if dirSetter, ok := tokenStore.(interface{ SetBaseDir(string) }); ok {
		dirSetter.SetBaseDir(cfg.AuthDir)
	}

	models := nvidia.RegistryModels()
	// getNvidiaModels is the catalog bound to the NVIDIA client ONLY. It must
	// never include other providers' models: a model owned by two clients makes
	// routing pick between them at random (intermittent 400s).
	getNvidiaModels := func() []*cliproxy.ModelInfo {
		out := make([]*cliproxy.ModelInfo, 0, len(models))
		for _, m := range models {
			if prefs.isHidden(m.ID) {
				continue
			}
			out = append(out, m)
		}
		return out
	}

	// getModels is the advertised catalog (/v1/models + Claude cache): NVIDIA
	// models plus custom endpoints.
	getModels := func() []*cliproxy.ModelInfo {
		out := make([]*cliproxy.ModelInfo, 0, len(models)+4)
		for _, m := range getNvidiaModels() {
			out = append(out, m)
		}
		for _, p := range prefs.listProviders() {
			id := p.modelID()
			if prefs.isHidden(id) {
				continue
			}
			out = append(out, &cliproxy.ModelInfo{
				ID:      id,
				Object:  "model",
				OwnedBy: p.Name,
				Type:    "openai-compat",
			})
		}
		return out
	}

	authHook := &nvidiaAuthHook{exec: exec, getModels: getNvidiaModels}
	core := coreauth.NewManager(tokenStore, nil, authHook)
	authHook.core = core
	core.RegisterExecutor(exec)
	// Do NOT Register auth before Run: coreManager.Load() resets from AuthDir.
	// Auth file is written in buildConfig so Load() picks up provider=nvidia.

	// Watcher replaces unknown providers with OpenAICompatExecutor and clears
	// models via UnregisterClient; hooks + reconciler put ours back.
	cliproxy.SetGlobalModelRegistryHook(&nvidiaModelHook{core: core, exec: exec, getModels: getNvidiaModels})
	bindNvidiaRuntime(core, exec, getNvidiaModels())

	prefs.onMaskChange = func() {
		bindNvidiaRuntime(core, exec, getNvidiaModels())
		writeClaudeModelCache(*addr, getModels())
	}

	hooks := cliproxy.Hooks{
		OnAfterStart: func(_ *cliproxy.Service) {
			ensureNvidiaAuth(core)
			n := bindNvidiaRuntime(core, exec, getNvidiaModels())
			writeClaudeModelCache(*addr, getModels())
			startNvidiaReconciler(ctx, core, exec, getNvidiaModels)
			log.Printf("serve %s listening on http://localhost%s (models=%d auth=%d; chat/completions + responses + messages; coalesce=%s max-inflight=%d)",
				version, *addr, len(models), n, execCoalesce(*coalesceMs), *maxInflight)
		},
	}

	svc, err := cliproxy.NewBuilder().
		WithConfig(cfg).
		WithConfigPath(cfgPath).
		WithCoreAuthManager(core).
		WithServerOptions(
			api.WithMiddleware(
				sanitizeClaudeToolResults(),
				customOpenAICompatPassthrough(prefs),
				claudeDefaultModelAlias(nvidia.DefaultAliasModel()),
				messagesOnlyPassthrough(prefs),
				requestLogger(),
			),
			// CLIProxyAPI already registers GET/HEAD /healthz; re-registering panics.
			// Install middleware before routes so we can enrich the response with pool stats.
			api.WithEngineConfigurator(func(engine *gin.Engine) {
				adminHandler := func(c *gin.Context) {
					adminRoutes(c, models, func() []gatewayModel { return nil })
				}
				engine.GET("/admin", adminHandler)
				engine.GET("/admin/stats", adminHandler)
				engine.GET("/admin/chromes", adminHandler)
				engine.POST("/admin/chromes", adminHandler)
				engine.POST("/admin/models", adminHandler)
				engine.POST("/admin/providers", adminHandler)
				engine.POST("/admin/delete", adminHandler)
				engine.POST("/admin/retry-failed", adminHandler)

				// Dummy endpoint for Claude Code ping/connection check
				engine.HEAD("/v1/api/hello", func(c *gin.Context) { c.Status(http.StatusOK) })
				engine.GET("/v1/api/hello", func(c *gin.Context) { c.Status(http.StatusOK) })
				engine.HEAD("/api/hello", func(c *gin.Context) { c.Status(http.StatusOK) })
				engine.GET("/api/hello", func(c *gin.Context) { c.Status(http.StatusOK) })

				// internal route for OpenRouter
				// engine.POST("/internal/openrouter/chat/completions", openRouterProxyHandler)

				// Catalog and health middleware must be installed before default routes.
				engine.Use(func(c *gin.Context) {
					path := c.Request.URL.Path
					if c.Request.Method == http.MethodGet && (path == "/v1/models" || path == "/v1/models/") {
						catalog := getModels()
						data := make([]gatewayModel, 0, len(catalog))
						for _, m := range catalog {
							if m == nil {
								continue
							}
							data = append(data, gatewayModel{
								Type:        "model",
								Object:      "model",
								ID:          m.ID,
								Name:        m.ID,
								DisplayName: m.ID,
								OwnedBy:     m.OwnedBy,
							})
						}
						firstID, lastID := "", ""
						if len(data) > 0 {
							firstID, lastID = data[0].ID, data[len(data)-1].ID
						}
						c.JSON(http.StatusOK, gin.H{
							"object":   "list",
							"data":     data,
							"has_more": false,
							"first_id": firstID,
							"last_id":  lastID,
						})
						c.Abort()
						return
					}

					if path != "/healthz" {
						c.Next()
						return
					}
					switch c.Request.Method {
					case http.MethodHead:
						c.Status(http.StatusOK)
						c.Abort()
					case http.MethodGet:
						out := gin.H{"ok": true}
						if p := exec.Pool(); p != nil {
							fills, takes, errs, expired := p.Stats()
							out["pool"] = gin.H{
								"ready":   p.Ready(),
								"fills":   fills,
								"takes":   takes,
								"errors":  errs,
								"expired": expired,
							}
						}
						c.JSON(http.StatusOK, out)
						c.Abort()
					default:
						c.Next()
					}
				})
			}),
		).
		WithHooks(hooks).
		Build()
	if err != nil {
		log.Fatalf("build gateway: %v", err)
	}

	if err := svc.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}

// writeClaudeModelCache refreshes Claude Code's gateway model discovery cache
// (~/.claude/cache/gateway-models.json) so /model reflects admin mask changes
// without waiting for Claude Code to re-fetch /v1/models. Best-effort: a
// missing cache dir or Claude Code version with a different format is ignored.
func writeClaudeModelCache(addr string, catalog []*cliproxy.ModelInfo) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	path := filepath.Join(home, ".claude", "cache", "gateway-models.json")
	type entry struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	}
	modelsOut := make([]entry, 0, len(catalog))
	for _, m := range catalog {
		if m == nil {
			continue
		}
		modelsOut = append(modelsOut, entry{
			ID:          m.ID,
			DisplayName: m.ID,
		})
	}
	payload, err := json.Marshal(gin.H{
		"baseUrl":   "http://localhost" + addr,
		"fetchedAt": time.Now().UnixMilli(),
		"models":    modelsOut,
	})
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		log.Printf("warning: could not refresh Claude model cache %s: %v", path, err)
	}
}

// rewriteConfigYAML rewrites the gateway's config stub with the current
// openai-compatibility providers; the CLIProxyAPI file watcher picks up the
// change and reloads the provider registry without a restart.
func rewriteConfigYAML(path string, providers []config.OpenAICompatibility) error {
	var b strings.Builder
	b.WriteString("# generated by cmd/serve; not used for API keys\n")
	if len(providers) > 0 {
		b.WriteString("openai-compatibility:\n")
		for _, p := range providers {
			b.WriteString("  - name: " + p.Name + "\n")
			b.WriteString("    base-url: " + p.BaseURL + "\n")
			b.WriteString("    api-key-entries:\n")
			for _, k := range p.APIKeyEntries {
				b.WriteString("      - api-key: " + k.APIKey + "\n")
			}
			b.WriteString("    models:\n")
			for _, m := range p.Models {
				b.WriteString("      - name: " + m.Name + "\n")
				if m.Alias != "" {
					b.WriteString("        alias: " + m.Alias + "\n")
				}
				if m.DisplayName != "" {
					b.WriteString("        display-name: " + m.DisplayName + "\n")
				}
			}
		}
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

func execCoalesce(ms int) time.Duration {
	return time.Duration(ms) * time.Millisecond
}

// requestLogger logs method, path, status and duration per request, enriched
// with the model name parsed from the JSON body (CLIProxyAPI's access log
// omits it).
func requestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()

		model := ""
		if c.Request.Body != nil && c.Request.Method == http.MethodPost {
			body, err := io.ReadAll(c.Request.Body)
			if err == nil {
				var payload struct {
					Model string `json:"model"`
				}
				if json.Unmarshal(body, &payload) == nil {
					model = payload.Model
				}
				c.Request.Body = io.NopCloser(bytes.NewReader(body))
			} else {
				c.Request.Body = io.NopCloser(bytes.NewReader(body))
			}
		}

		c.Next()

		log.Printf("%d | %s | %s | %s %q model=%q",
			c.Writer.Status(), time.Since(start).Round(time.Microsecond),
			c.ClientIP(), c.Request.Method, c.Request.URL.Path, model)
	}
}

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
