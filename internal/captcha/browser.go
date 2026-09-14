package captcha

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/chromedp"
)

// Browser owns one long-lived headless Chrome and one sticky playground tab.
//
// chromedp allocates the OS Chrome process on the first Run and ties it to that
// context (exec.CommandContext). Canceling a timeout used for that first Run
// kills Chrome — so we keep browserCtx alive until Close.
//
// After the playground is warm, Extract fires hcaptcha.execute({async:true})
// and polls the response (~300ms steady-state) instead of a full Navigate
// (~6–10s). After stickyMaxIdle without a successful extract, the next Extract
// re-navigates instead of burning the sticky timeout on a stale widget.
//
// For parallel pool fills use NewBrowserGroup (separate Chrome processes):
// a second tab in the same Chrome never mounts the hCaptcha widget on this site.
type Browser struct {
	browser context.Context
	cancel  context.CancelFunc // allocator
	bCancel context.CancelFunc // browser tab / process owner

	url        URLProvider
	harnessURL string // data: URL when harness mode is on; "" = playground mode

	// busy/dead are owned by BrowserGroup: busy is set on borrow and
	// cleared on release; dead marks a slot whose Chrome was killed (admin
	// Kill) — the tombstone keeps the index until Start spawns a replacement.
	busy     atomic.Bool
	dead     atomic.Bool
	extracts atomic.Uint64

	mu     sync.Mutex
	closed bool
	warmed bool
	lastOK time.Time
}

// PID returns the Chrome OS process id (0 when unavailable, e.g. test fakes).
func (b *Browser) PID() int {
	if b.browser == nil {
		return 0
	}
	if c := chromedp.FromContext(b.browser); c != nil && c.Browser != nil {
		if p := c.Browser.Process(); p != nil {
			return p.Pid
		}
	}
	return 0
}

// Warmed reports whether the sticky tab is currently warm.
func (b *Browser) Warmed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.warmed
}

// LastOK returns the time of the last successful extract (zero = never).
func (b *Browser) LastOK() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastOK
}

// currentURL returns the navigation target for warmPlayground. Harness mode
// uses a precomputed data: URL (set once in NewBrowser from cfg.HarnessPage);
// playground mode defers to URLProvider each call (so ChampionState swaps
// are picked up on the next sticky-idle re-navigate).
func (b *Browser) currentURL(ctx context.Context) string {
	if b.harnessURL != "" {
		return b.harnessURL
	}
	return b.url.PlaygroundURL(ctx)
}

// stickyMaxIdle is how long a warm playground tab is trusted for sticky execute.
// Longer idle (user paused mid-chat) often leaves the widget unable to mint tokens.
const stickyMaxIdle = 60 * time.Second

// NewBrowser starts a shared Chrome process and warms the playground page.
// Call Close when done.
//
// Container / proxy hints:
//   - CHROME_PATH: absolute path to chromium/chrome binary
//   - CHROMEDP_NO_SANDBOX=1: add --no-sandbox and --disable-dev-shm-usage
//   - CHROMEDP_ALLOW_IMAGES=1: re-enable image loading (default is off). Pictures
//     are unnecessary for this invisible hCaptcha widget, so images are blocked
//     by default to cut per-navigate RAM/bandwidth; re-enable only if a future
//     site change makes image decode required for token extraction.
//   - CHROME_PROXY / BrowserConfig.Proxy: Chrome --proxy-server (e.g. socks5://host:port)
//   - BrowserConfig.HarnessPage: when set, Browser warms a local data: URL
//     instead of a real playground URL — RAM drops from ~150–350MB to ~50MB.
func NewBrowser(parent context.Context, cfg BrowserConfig) (*Browser, error) {
	cfg = cfg.withDefaults()
	allocOpts := ChromeAllocatorOptions()
	if path := os.Getenv("CHROME_PATH"); path != "" {
		allocOpts = append(allocOpts, chromedp.ExecPath(path))
	}
	if os.Getenv("CHROMEDP_NO_SANDBOX") == "1" {
		allocOpts = append(allocOpts,
			chromedp.Flag("no-sandbox", true),
			chromedp.Flag("disable-dev-shm-usage", true),
		)
	}
	// Images are blocked by default (verified end-to-end: a token extracted with
	// imagesEnabled=false is accepted by the upstream predict API → HTTP 200).
	// CHROMEDP_ALLOW_IMAGES=1 opts back in for debugging unexpected hCaptcha change.
	if os.Getenv("CHROMEDP_ALLOW_IMAGES") != "1" {
		allocOpts = append(allocOpts,
			chromedp.Flag("blink-settings", "imagesEnabled=false"),
		)
	}
	if cfg.Proxy != "" {
		allocOpts = append(allocOpts, chromeProxyOpts(cfg.Proxy)...)
	}

	allocCtx, allocCancel := chromedp.NewExecAllocator(parent, allocOpts...)
	// Drop chromedp's noisy "unhandled … event" logs (e.g. TopLayerElementsUpdated).
	// They are CDP events the library does not model; they do not abort Run.
	browser, bCancel := chromedp.NewContext(allocCtx, chromedp.WithErrorf(quietChromedpErrorf))

	// Allocate Chrome on browserCtx with no canceling timeout (CommandContext
	// would kill the process if the first-Run context is canceled).
	if err := chromedp.Run(browser, chromedp.Navigate("about:blank")); err != nil {
		bCancel()
		allocCancel()
		return nil, fmt.Errorf("captcha browser alloc: %w", err)
	}

	b := &Browser{
		browser:    browser,
		cancel:     allocCancel,
		bCancel:    bCancel,
		url:        cfg.URLProvider,
		harnessURL: cfg.HarnessPage,
	}

	// Warm playground once so Extract can skip Navigate in the steady state.
	warmCtx, warmCancel := context.WithTimeout(browser, 90*time.Second)
	defer warmCancel()
	if err := warmPlayground(warmCtx, b.currentURL(parent)); err != nil {
		b.Close()
		return nil, fmt.Errorf("captcha browser warm: %w", err)
	}
	b.warmed = true
	b.lastOK = time.Now()
	return b, nil
}

// Extract returns a one-shot captcha token from the sticky playground tab.
// Concurrent callers are serialized (one tab); steady-state cost is execute({async:true}).
func (b *Browser) Extract(ctx context.Context) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return "", fmt.Errorf("captcha browser closed")
	}

	// Sticky execute is normally ~300ms; bound it tightly so a hung widget
	// fails fast and re-navigate can start (pool Take otherwise waits with
	// no tokens). 5s is enough for a healthy sticky path and avoids burning
	// most of captcha-wait (30s) before recovery begins.
	needNav := !b.warmed || time.Since(b.lastOK) > stickyMaxIdle
	if needNav {
		pageURL := b.currentURL(ctx)
		token, err := b.runExtract(ctx, 90*time.Second, func(c context.Context) (string, error) {
			return navigateAndExecute(c, pageURL)
		})
		if err != nil {
			b.warmed = false
			return "", err
		}
		b.warmed = true
		b.lastOK = time.Now()
		return token, nil
	}

	token, err := b.runExtract(ctx, 5*time.Second, executeOnly)
	if err == nil {
		b.lastOK = time.Now()
		return token, nil
	}
	// Page may have broken (navigation, bot wall, widget gone) — full recover.
	pageURL := b.currentURL(ctx)
	token, navErr := b.runExtract(ctx, 90*time.Second, func(c context.Context) (string, error) {
		return navigateAndExecute(c, pageURL)
	})
	if navErr != nil {
		b.warmed = false
		return "", fmt.Errorf("sticky execute failed (%v); re-navigate failed: %w", err, navErr)
	}
	b.warmed = true
	b.lastOK = time.Now()
	return token, nil
}

// ExtractN returns n captcha tokens from one sticky-tab borrow. Only the
// harness mode can do this in a single CDP round-trip — the harness page
// pre-mounts N hidden widgets, so each hcaptcha.execute(id_i) returns a
// fresh token independently. Playground mode falls back to N serial
// Extracts (sticky widget can only hold one response at a time).
//
// ponytail: playground batch is N serial sticky executes — same per-borrow
// cost, no throughput gain. Switch to -captcha-harness=true to actually
// realise the speedup.
func (b *Browser) ExtractN(ctx context.Context, n int) ([]string, error) {
	if n <= 1 {
		t, err := b.Extract(ctx)
		if err != nil {
			return nil, err
		}
		return []string{t}, nil
	}
	if b.harnessURL == "" {
		// Playground mode: serialize n sticky executes. See ponytail comment.
		out := make([]string, 0, n)
		for i := 0; i < n; i++ {
			t, err := b.Extract(ctx)
			if err != nil {
				return nil, err
			}
			out = append(out, t)
		}
		return out, nil
	}
	return b.extractBatchHarness(ctx, n)
}

func (b *Browser) runExtract(ctx context.Context, limit time.Duration, fn func(context.Context) (string, error)) (string, error) {
	runCtx, cancel := context.WithTimeout(b.browser, limit)
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	return fn(runCtx)
}

// Close shuts down the shared Chrome process.
func (b *Browser) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	b.warmed = false
	if b.bCancel != nil {
		b.bCancel()
	}
	b.cancel()
}

// quietChromedpErrorf suppresses known-benign CDP events chromedp has not
// wired into its DOM/page switch (logged as ERROR otherwise).
func quietChromedpErrorf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if strings.HasPrefix(msg, "unhandled node event") || strings.HasPrefix(msg, "unhandled page event") {
		return
	}
	log.Printf("ERROR: "+format, args...)
}
