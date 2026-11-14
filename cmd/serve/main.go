// cmd/serve — multi-format OpenAI/Claude/Responses gateway for NVIDIA playground.
//
// Embeds CLIProxyAPI with a custom nvidia ProviderExecutor. Upstream predict is
// already OpenAI Chat Completions shape; builtin translators expose:
//
//	POST /v1/chat/completions
//	POST /v1/responses
//	POST /v1/messages
//
// Optional inbound keys via -gateway-key. Captcha via Admin Start All,
// -captcha, or nv-captcha-token. Chrome remains idle at startup, even with -auto.
//
// Usage:
//
//	go run ./cmd/serve
//	go run ./cmd/serve -pool-size=2 -pool-workers=1 -coalesce-ms=0 -max-inflight=8
//	go run ./cmd/serve -captcha "P1_..."
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/minhhhduc/gnvipi/internal/captcha"
	"github.com/minhhhduc/gnvipi/internal/db"
	gwprovider "github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"github.com/minhhhduc/gnvipi/internal/provider/nvidia"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"

	_ "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator/builtin"
)

// Set via -ldflags "-X main.version=v1.2.3" at release build time.
var version = "dev"

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	handled, err := runCLICommand(os.Args[1:], os.LookupEnv, db.Migrate, runImportV2Command)
	if handled {
		return err
	}
	return runServer()
}

func runCLICommand(
	args []string,
	lookup func(string) (string, bool),
	migrate func(context.Context, string) error,
	importV2 func(context.Context, []string, func(string) (string, bool)) error,
) (bool, error) {
	var command func(context.Context) error
	switch {
	case len(args) > 0 && args[0] == "migrate":
		command = func(ctx context.Context) error { return runMigrateCommand(ctx, args[1:], lookup, migrate) }
	case len(args) > 1 && args[0] == "admin" && args[1] == "import-v2":
		command = func(ctx context.Context) error { return importV2(ctx, args[2:], lookup) }
	default:
		return false, nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return true, command(ctx)
}

func runServer() (err error) {
	// Loopback by default: the admin console and /v1 routes stay unreachable
	// from the network unless the operator opts in with an explicit wildcard
	// bind. A non-loopback bind also requires -admin-token for /admin.
	addr := flag.String("addr", "127.0.0.1:8080", "listen address (loopback by default; use :8080 for all interfaces)")
	captchaFlag := flag.String("captcha", "", "one-shot hCaptcha token (consumed on first use)")
	flag.Bool("auto", false, "captcha Chrome starts only on demand from /admin (startup stays idle)")
	poolSize := flag.Int("pool-size", 3, "ready captcha tokens to keep buffered after Admin Start All")
	poolWorkers := flag.Int("pool-workers", 1, "concurrent captcha extractors / Chrome processes after Admin Start All")
	maxInflight := flag.Int("max-inflight", 4, "max concurrent upstream streams (0=unlimited)")
	inflightWait := flag.Duration("inflight-wait", 500*time.Millisecond, "how long to wait for an in-flight slot before returning 503 (0=reject immediately)")
	coalesceMs := flag.Int("coalesce-ms", 16, "merge consecutive SSE content deltas within this window (0=off); first token always flushes immediately")
	warmTimeout := flag.Duration("warm-timeout", 3*time.Minute, "background wait for the first captcha token after Admin Start All; 0=skip")
	poolTTL := flag.Duration("pool-ttl", 90*time.Second, "discard pooled captcha tokens older than this")
	captchaWait := flag.Duration("captcha-wait", 30*time.Second, "max wait for a pooled captcha token per request (0=block until ready); then 503")
	modelsFile := flag.String("models-file", "internal/models/playground_models.json", "path to save hidden/deleted models and custom endpoints")
	chromeProxy := flag.String("chrome-proxy", "", "proxy for captcha Chrome and upstream API (e.g. socks5://host:port); falls back to CHROME_PROXY")
	captchaHarness := flag.Bool("captcha-harness", false, "mint tokens on a local data: HTML harness page instead of a real playground URL (lower RAM; requires -pool-batch to realize speedup)")
	poolBatch := flag.Int("pool-batch", 1, "tokens to mint per sticky-tab borrow (1=single; requires -captcha-harness=true for speedup)")
	captchaSelectBudget := flag.Duration("captcha-select-budget", 0, "if >0, benchmark playground candidates on Admin Start All to pick the fastest minting URL (persists to ~/.cache/gnvipi/playground-state.json)")
	adminToken := flag.String("admin-token", "", "admin access token (also GNVIPI_ADMIN_TOKEN)")
	trustedProxies := flag.String("trusted-proxies", "", "comma-separated proxy IPs/CIDRs trusted for client IP forwarding (also GNVIPI_TRUSTED_PROXIES)")
	// Headless deployments (containers, pure proxy nodes) can drop the whole
	// admin surface so /admin and /admin/api are not routable at all.
	disableAdminUI := flag.Bool("disable-admin-ui", false, "disable the admin UI and admin API entirely (headless proxy mode)")
	gatewayKey := flag.String("gateway-key", "", "comma-separated gateway API keys (also GNVIPI_GATEWAY_KEY)")
	gatewayV2 := flag.Bool("gateway-v2", true, "route all custom provider requests (same-protocol relay and cross-protocol translation) through the gateway core dispatcher (enabled by default; set -gateway-v2=false as a deprecated escape hatch)")
	autoModeChatResponses := flag.Bool("automode-chat-responses", false, "allow automode virtual model on /v1/chat/completions and /v1/responses (default false: Anthropic /v1/messages only)")
	autoMigrate := flag.Bool("auto-migrate", false, "apply database migrations at startup (development only; production uses `serve migrate`)")
	maxBodyBytes := flag.Int64("max-body-bytes", 32<<20, "maximum request body bytes for /v1 routes")
	maxAdminBodyBytes := flag.Int64("max-admin-body-bytes", 1<<20, "maximum request body bytes for /admin routes")
	statsOptions := nvidia.DefaultStatsLogOptions()
	flag.Int64Var(&statsOptions.MaxBytes, "stats-max-bytes", statsOptions.MaxBytes, "maximum bytes of retained stats history")
	flag.Int64Var(&statsOptions.SegmentBytes, "stats-segment-bytes", statsOptions.SegmentBytes, "maximum bytes per stats log segment")
	flag.DurationVar(&statsOptions.RotateInterval, "stats-rotate-interval", statsOptions.RotateInterval, "maximum age of an active stats segment")
	flag.DurationVar(&statsOptions.Retention, "stats-retention", statsOptions.Retention, "retention period for stats history")
	flag.DurationVar(&statsOptions.ReadInterval, "stats-read-interval", statsOptions.ReadInterval, "minimum dashboard polling and uncached history read interval")
	var upstreamPolicy gwprovider.URLPolicy
	flag.BoolVar(&upstreamPolicy.AllowUnsafe, "allow-unsafe-upstreams", false, "allow link-local/cloud metadata upstreams (unsafe)")
	flag.BoolVar(&upstreamPolicy.DenyPrivate, "deny-private-upstreams", false, "deny loopback/LAN upstreams")
	flag.Parse()
	configureUpstreamURLPolicy(upstreamPolicy)
	adminTokenSet := false
	flag.Visit(func(f *flag.Flag) { adminTokenSet = adminTokenSet || f.Name == "admin-token" })
	if !adminTokenSet {
		*adminToken = os.Getenv("GNVIPI_ADMIN_TOKEN")
	}
	gatewayKeySet := false
	flag.Visit(func(f *flag.Flag) { gatewayKeySet = gatewayKeySet || f.Name == "gateway-key" })
	if !gatewayKeySet {
		*gatewayKey = os.Getenv("GNVIPI_GATEWAY_KEY")
	}
	trustedProxiesSet := false
	flag.Visit(func(f *flag.Flag) { trustedProxiesSet = trustedProxiesSet || f.Name == "trusted-proxies" })
	if !trustedProxiesSet {
		*trustedProxies = os.Getenv("GNVIPI_TRUSTED_PROXIES")
	}
	clientIPs, err := newClientIPResolver(*trustedProxies)
	if err != nil {
		return fmt.Errorf("trusted proxies: %w", err)
	}
	autoModeChatResponsesSet := false
	flag.Visit(func(f *flag.Flag) {
		autoModeChatResponsesSet = autoModeChatResponsesSet || f.Name == "automode-chat-responses"
	})
	if !autoModeChatResponsesSet {
		if v := os.Getenv("GNVIPI_AUTOMODE_CHAT_RESPONSES"); v == "1" || strings.EqualFold(v, "true") {
			*autoModeChatResponses = true
		}
	}
	if *autoModeChatResponses {
		prefs.autoModelChatResponses = true
	}
	configureSecretRedaction(*adminToken, *gatewayKey, *captchaFlag)
	if *maxBodyBytes <= 0 || *maxAdminBodyBytes <= 0 {
		return errors.New("request body limits must be positive")
	}
	if err := nvidia.ConfigureStatsLog(statsOptions); err != nil {
		return fmt.Errorf("stats configuration: %w", err)
	}

	if *modelsFile != "" {
		if err := prefs.load(*modelsFile); err != nil {
			return fmt.Errorf("models file: %w", err)
		}
	}

	if *captchaFlag == "" {
		log.Print("captcha Chrome idle; enable it from /admin?page=chromes or send nv-captcha-token per request")
	}

	proxyURL := strings.TrimSpace(*chromeProxy)
	if proxyURL == "" {
		proxyURL = strings.TrimSpace(os.Getenv("CHROME_PROXY"))
	}
	proxyFunc := http.ProxyFromEnvironment
	if proxyURL != "" {
		u, err := url.Parse(proxyURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("chrome-proxy: invalid URL %q", proxyURL)
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Database mode owns /admin/api identity. Without GNVIPI_DATABASE_URL the
	// handle stays nil and the file-backed admin-token gate keeps serving.
	appState, dbErr := openAppDB(ctx, *autoMigrate)
	if dbErr != nil {
		return dbErr
	}
	defer appState.Close()
	var adminAuth *authState
	if appState != nil {
		adminAuth = appState.auth
		// The cookie transport rule depends on how the admin listener is bound,
		// which openAppDB does not know.
		adminAuth.loopbackBind = adminLoopbackBind(*addr)
		adminAuth.clientIPs = clientIPs
	}

	// Database mode also owns routing configuration: one immutable snapshot per
	// process, replaced after every committed write. A configured database that
	// cannot produce a valid first snapshot is a startup error, so the process
	// never serves an empty or partially validated configuration.
	snapshotRuntime, err := openConfiguredProviderRuntime(ctx, appState)
	if err != nil {
		return err
	}
	defer snapshotRuntime.Close()

	exec := nvidia.NewExecutor(&nvidia.Options{
		FlagCaptcha:  *captchaFlag,
		Coalesce:     time.Duration(*coalesceMs) * time.Millisecond,
		MaxInflight:  *maxInflight,
		InflightWait: *inflightWait,
		CaptchaWait:  *captchaWait,
		HTTPClient:   &http.Client{Timeout: 0, Transport: transport},
	})
	chromes := newChromeLifecycle(ctx, exec, func(ctx context.Context) (chromeBrowser, *captcha.Pool, error) {
		candidates := playgroundCandidates(8)
		// Prefer a model-independent NVIDIA page when it exposes a usable
		// CAPTCHA widget; otherwise retain the rotating playground fallback.
		if _, err := captcha.ScrapeSitekey(ctx, captcha.NVIDIAHomeURL); err == nil {
			candidates = append([]string{captcha.NVIDIAHomeURL}, candidates...)
		} else {
			log.Printf("captcha: NVIDIA home page has no usable widget (%v); using playground fallback", err)
		}
		orderedURLs := append([]string(nil), candidates...)
		if *captchaSelectBudget > 0 {
			log.Printf("captcha: running champion selection (%d candidates, budget=%s)", len(candidates), *captchaSelectBudget)
			if state, err := captcha.SelectChampion(ctx, candidates, *captchaSelectBudget); err != nil {
				log.Printf("warning: champion selection failed (%v); rotating through playground candidates", err)
			} else {
				orderedURLs = append([]string{state.ChampionURL}, append(state.FallbackURLs, candidates...)...)
				log.Printf("captcha: champion=%s score=%dms", state.ChampionURL, state.ScoreMS)
			}
		} else if state := captcha.LoadChampion(captcha.ChampionStateFile()); state.ChampionURL != "" {
			orderedURLs = append([]string{state.ChampionURL}, append(state.FallbackURLs, candidates...)...)
			log.Printf("captcha: loaded champion=%s from disk", state.ChampionURL)
		}
		urlProvider := captcha.NewRotatingURLProvider(uniqueURLs(orderedURLs))
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
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		browser, err := captcha.NewBrowserGroup(ctx, *poolWorkers, browserCfg)
		if err != nil {
			return nil, nil, err
		}
		batch := *poolBatch
		if batch < 1 {
			batch = 1
		}
		if browserCfg.HarnessPage == "" && batch > 1 {
			log.Printf("captcha: -pool-batch=%d clamped to 1 in playground mode (batching requires a usable harness)", batch)
			batch = 1
		}
		poolCfg := captcha.PoolConfig{Size: *poolSize, Workers: *poolWorkers, TTL: *poolTTL}
		if batch > 1 {
			poolCfg.BatchSize = batch
			poolCfg.ExtractN = browser.ExtractN
		}
		pool := captcha.NewPool(ctx, browser.Extract, poolCfg)
		log.Printf("captcha pool: size=%d workers=%d chromes=%d batch=%d ttl=%s captcha-wait=%s", *poolSize, *poolWorkers, browser.Len(), batch, *poolTTL, *captchaWait)
		if *warmTimeout > 0 {
			go func() {
				if err := waitPoolReady(ctx, pool, 1, *warmTimeout); err != nil {
					if ctx.Err() == nil {
						log.Printf("warning: %v — admin remains available; NVIDIA requests may wait for captcha", err)
					}
				} else {
					log.Printf("captcha pool ready=%d (TTFT path unblocked)", pool.Ready())
				}
			}()
		}
		return browser, pool, nil
	})
	defer chromes.Close()
	stopChromeShutdown := context.AfterFunc(ctx, chromes.Close)
	defer stopChromeShutdown()

	cfg, cfgPath, err := buildConfig(*addr)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	defer func() {
		if cleanupErr := os.RemoveAll(cfg.AuthDir); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("remove auth dir: %w", cleanupErr))
		}
	}()
	hasGatewayKey := false
	for _, key := range strings.Split(*gatewayKey, ",") {
		if strings.TrimSpace(key) != "" {
			hasGatewayKey = true
			break
		}
	}
	if !hasGatewayKey && !strings.EqualFold(cfg.Host, "localhost") && !net.ParseIP(cfg.Host).IsLoopback() {
		log.Print("warning: non-loopback listener has no gateway key; /v1 and /mcp are unauthenticated (set -gateway-key or GNVIPI_GATEWAY_KEY)")
	}

	nvidiaModels := nvidia.RegistryModels()
	// getNvidiaModels is the catalog bound to the NVIDIA client ONLY. It must
	// never include other providers' models: a model owned by two clients makes
	// routing pick between them at random (intermittent 400s).
	getNvidiaModels := func() []*cliproxy.ModelInfo {
		out := make([]*cliproxy.ModelInfo, 0, len(nvidiaModels))
		for _, m := range nvidiaModels {
			if prefs.isHidden(m.ID) {
				continue
			}
			out = append(out, m)
		}
		return out
	}

	// getModels is the advertised catalog (/v1/models + Claude cache): NVIDIA
	// models + v2 provider aliases + virtual models.
	getModels := func() []gatewayModel {
		return buildCatalog(prefs, getNvidiaModels)
	}

	// Register /admin custom endpoints (e.g. OpenRouter) as OpenAI-compat
	// providers so CLIProxyAPI routes their aliases.
	providerConfigs, err := prefs.listProvidersV2()
	if err != nil {
		return fmt.Errorf("provider configuration: %w", err)
	}
	cfg.OpenAICompatibility = compatProviders(providerConfigs)
	if err := rewriteConfigYAML(cfgPath, cfg.OpenAICompatibility); err != nil {
		log.Printf("warning: could not write initial config for providers: %v", err)
	}
	prefs.onChange = func(_ []customProvider) {
		providerConfigs, err := prefs.listProvidersV2()
		if err != nil {
			log.Printf("provider reload: %v", err)
			return
		}
		cfg.OpenAICompatibility = compatProviders(providerConfigs)
		if err := rewriteConfigYAML(cfgPath, cfg.OpenAICompatibility); err != nil {
			log.Printf("warning: could not rewrite config for providers: %v", err)
		}
		writeClaudeModelCache(*addr, getModels())
	}

	gatewayMiddleware, err := gatewayV2Middleware(prefs, *gatewayV2, proxyURL)
	if err != nil {
		return fmt.Errorf("gateway dispatcher: %w", err)
	}

	tokenStore := sdkAuth.GetTokenStore()
	if dirSetter, ok := tokenStore.(interface{ SetBaseDir(string) }); ok {
		dirSetter.SetBaseDir(cfg.AuthDir)
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
		writeClaudeModelCache(portOnly(*addr), getModels())
	}

	started := make(chan struct{})
	hooks := cliproxy.Hooks{
		OnAfterStart: func(_ *cliproxy.Service) {
			ensureNvidiaAuth(core)
			n := bindNvidiaRuntime(core, exec, getNvidiaModels())
			writeClaudeModelCache(portOnly(*addr), getModels())
			startNvidiaReconciler(ctx, core, exec, getNvidiaModels)
			log.Printf("serve %s listening on http://localhost%s (models=%d auth=%d; chat/completions + responses + messages; coalesce=%s max-inflight=%d)",
				version, portOnly(*addr), len(nvidiaModels), n, execCoalesce(*coalesceMs), *maxInflight)
			if *disableAdminUI {
				log.Print("admin console: disabled by -disable-admin-ui")
			} else {
				logAdminSurface(*addr, *adminToken)
			}
			close(started)
		},
	}

	svc, err := cliproxy.NewBuilder().
		WithConfig(cfg).
		WithConfigPath(cfgPath).
		WithCoreAuthManager(core).
		WithServerOptions(
			api.WithMiddleware(
				gatewayMiddleware,
				// Responses-only providers need a real Anthropic<->Responses adapter;
				// keep it ahead of generic Messages sanitizing/fallback routing.
				anthropicResponsesPassthrough(prefs),
				sanitizeClaudeToolResults(),
				customOpenAIResponsesPassthrough(prefs),
				customOpenAICompatPassthrough(prefs),
				messagesOnlyPassthrough(prefs),
				requestLogger(prefs),
			),
			// CLIProxyAPI already registers GET/HEAD /healthz; re-registering panics.
			// Install middleware before routes so we can enrich the response with pool stats.
			api.WithEngineConfigurator(func(engine *gin.Engine) {
				// The SDK calls this before WithMiddleware and early route registration.
				engine.Use(secretResponseRedaction())
				engine.Use(requestBodyLimit(*maxBodyBytes, *maxAdminBodyBytes))
				// Attach identity before early routes or cycle dispatch can finish the request.
				engine.Use(gatewayRequestContext())
				// Guard the early catalog routes and cycle dispatch before WithMiddleware.
				engine.Use(gatewayAuth(*gatewayKey))
				// One switch owns the whole admin surface: access middleware, SPA,
				// legacy endpoints and JSON API. Headless mode registers none of them.
				if !*disableAdminUI {
					// The SDK registers these routes before applying WithMiddleware.
					engine.Use(adminAccess(*addr, *adminToken, adminAuth != nil))
					// Database mode authenticates /admin/api per route with a
					// role/scope table; a route without a declaration is refused.
					engine.Use(adminAPIAuthorization(adminAuth))
				}
				// Resolve automode before cycleRouter inspects the model field.
				// Install middleware before routes: Gin snapshots the chain per route.
				engine.Use(claudeAutoModelAlias(prefs), cycleRouter(prefs, engine))
				if !*disableAdminUI {
					adminHandler := func(c *gin.Context) {
						adminRoutes(c, nvidiaModels)
					}
					// The SPA owns GET /admin; legacy endpoints stay for tools.
					registerAdminUI(engine)
					engine.GET("/admin/stats", adminHandler)
					chromeHandler := func(c *gin.Context) { chromeRoutes(c, chromes) }
					engine.GET("/admin/chromes", chromeHandler)
					engine.POST("/admin/chromes", chromeHandler)
					engine.POST("/admin/models", adminHandler)
					engine.POST("/admin/automode", adminHandler)
					engine.POST("/admin/cycle", adminHandler)
					engine.POST("/admin/search", adminHandler)
					engine.POST("/admin/providers", adminHandler)
					engine.POST("/admin/delete", adminHandler)
					engine.POST("/admin/retry-failed", adminHandler)
					registerAdminAPIRoutes(engine, prefs, nvidiaModels, chromes, adminAuth)
				}
				engine.POST("/mcp", searchMCP)
				engine.GET("/mcp", searchMCP)

				// Dummy endpoint for Claude Code ping/connection check
				engine.HEAD("/v1/api/hello", func(c *gin.Context) { c.Status(http.StatusOK) })
				engine.GET("/v1/api/hello", func(c *gin.Context) { c.Status(http.StatusOK) })
				engine.HEAD("/api/hello", func(c *gin.Context) { c.Status(http.StatusOK) })
				engine.GET("/api/hello", func(c *gin.Context) { c.Status(http.StatusOK) })

				// internal route for OpenRouter
				// engine.POST("/internal/openrouter/chat/completions", openRouterProxyHandler)

				// Catalog and health middleware must be installed before default routes.
				engine.Use(modelsAndHealthMiddleware(getModels, exec, snapshotRuntime))
			}),
		).
		WithHooks(hooks).
		Build()
	if err != nil {
		return fmt.Errorf("build gateway: %w", err)
	}
	installSDKPolicyTransports(core, cfg, &sdkPolicyTransports{policy: upstreamPolicy})

	// Start the API/admin listener with Chrome idle. No NVIDIA playground or
	// browser allocation occurs until the operator requests Admin Start All.
	runErr := make(chan error, 1)
	go func() { runErr <- svc.Run(ctx) }()
	select {
	case <-started:
	case err := <-runErr:
		if err != nil && ctx.Err() == nil {
			return err
		}
		return nil
	case <-ctx.Done():
		return nil
	}

	if err := <-runErr; err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}
