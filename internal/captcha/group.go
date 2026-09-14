package captcha

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// BrowserGroup fans Extract across n independent Chrome processes.
// Same-Chrome multi-tab does not work on build.nvidia.com: a second tab never
// mounts the invisible hCaptcha widget (CreateTarget probe: widget timeout).
//
// After a hard extract failure (empty token / dead widget), the offending Chrome
// is killed and replaced so the pool is not stuck on a zombie process.
type BrowserGroup struct {
	parent         context.Context
	cfg            BrowserConfig
	browserFactory func(context.Context, BrowserConfig) (*Browser, error)
	browsers       []*Browser
	free           chan *Browser
	done           chan struct{}

	mu     sync.Mutex
	closed bool
}

// NewBrowserGroup starts n warmed browsers (n Chrome processes). n < 1 means 1.
func NewBrowserGroup(parent context.Context, n int, cfg BrowserConfig) (*BrowserGroup, error) {
	if n < 1 {
		n = 1
	}
	cfg = cfg.withDefaults()
	g := &BrowserGroup{
		parent:         parent,
		cfg:            cfg,
		browserFactory: NewBrowser,
		browsers:       make([]*Browser, 0, n),
		free:           make(chan *Browser, n),
		done:           make(chan struct{}),
	}
	if cfg.Proxy != "" {
		log.Printf("captcha chrome proxy=%s", cfg.Proxy)
	}
	for i := 0; i < n; i++ {
		b, err := g.browserFactory(parent, cfg)
		if err != nil {
			target := ""
			if cfg.URLProvider != nil {
				target = cfg.URLProvider.PlaygroundURL(parent)
			}
			log.Printf("warning: captcha browser %d failed on %s (%v); trying fallback URL", i, target, err)
			fallbackCfg := cfg
			fallbackCfg.URLProvider = NewDefaultURLProvider()
			b, err = g.browserFactory(parent, fallbackCfg)
			if err != nil {
				if len(g.browsers) > 0 {
					log.Printf("warning: captcha browser %d failed fallback (%v); continuing with %d worker(s)", i, err, len(g.browsers))
					break
				}
				g.Close()
				return nil, fmt.Errorf("captcha browser %d: %w", i, err)
			}
		}
		g.browsers = append(g.browsers, b)
		g.free <- b
	}
	return g, nil
}

// Len returns how many Chrome workers are available.
func (g *BrowserGroup) Len() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.browsers)
}

// ChromeInfo is one Chrome's admin-facing status.
type ChromeInfo struct {
	Index    int       `json:"index"`
	PID      int       `json:"pid"`
	Busy     bool      `json:"busy"`
	Killed   bool      `json:"killed"`
	Warmed   bool      `json:"warmed"`
	LastOK   time.Time `json:"last_ok"`
	Extracts uint64    `json:"extracts"` // successful ExtractN borrows since start
}

// Snapshot returns the status of every Chrome in the group.
func (g *BrowserGroup) Snapshot() []ChromeInfo {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]ChromeInfo, 0, len(g.browsers))
	for i, b := range g.browsers {
		out = append(out, ChromeInfo{
			Index:    i,
			PID:      b.PID(),
			Busy:     b.busy.Load(),
			Killed:   b.dead.Load(),
			Warmed:   b.Warmed(),
			LastOK:   b.LastOK(),
			Extracts: b.extracts.Load(),
		})
	}
	return out
}

// Kill takes Chrome i out of rotation and releases its memory: the process,
// tab and allocator contexts are closed, and the slot keeps a dead tombstone
// so its index survives. Mint skips dead slots; a mid-flight extract on this
// Chrome fails fast and its pool worker backs off. Start puts a fresh Chrome
// back into the slot.
func (g *BrowserGroup) Kill(i int) error {
	g.mu.Lock()
	if i < 0 || i >= len(g.browsers) {
		g.mu.Unlock()
		return fmt.Errorf("chrome %d not found", i)
	}
	b := g.browsers[i]
	g.mu.Unlock()
	if b.dead.Swap(true) {
		return nil // already killed
	}
	pid := b.PID()
	b.Close()
	log.Printf("captcha chrome %d (pid %d) killed by admin", i, pid)
	return nil
}

// Start puts Chrome i back into rotation with a fresh Chrome process,
// replacing the dead tombstone left by Kill (so the GC can collect it).
func (g *BrowserGroup) Start(i int) error {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return fmt.Errorf("captcha browser group closed")
	}
	if i < 0 || i >= len(g.browsers) {
		g.mu.Unlock()
		return fmt.Errorf("chrome %d not found", i)
	}
	old := g.browsers[i]
	g.mu.Unlock()

	nb, err := g.browserFactory(g.parent, g.cfg)
	if err != nil {
		return fmt.Errorf("spawn chrome: %w", err)
	}

	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		nb.Close()
		return fmt.Errorf("captcha browser group closed")
	}
	if i >= len(g.browsers) || g.browsers[i] != old {
		g.mu.Unlock()
		nb.Close()
		return fmt.Errorf("chrome %d replaced meanwhile", i)
	}
	g.browsers[i] = nb
	g.mu.Unlock()

	// The old browser (a Kill tombstone or a still-live Chrome superseded by
	// this Start) is gone from g.browsers; close it for good and clear its
	// dead flag so a stale reference never reports itself as killed.
	old.Close()
	old.dead.Store(false)
	log.Printf("captcha chrome %d started (pid %d)", i, nb.PID())
	return nil
}

// borrow returns a free, live browser, or an error when every browser is
// killed or the group is closed. Busy browsers wait (bounded by the caller's
// ctx — mint eventually proceeds when one frees up). Dead-slot tombstones are
// skipped: their Chrome is gone, their index survives for Start.
func (g *BrowserGroup) borrow() (*Browser, error) {
	for {
		g.mu.Lock()
		closed := g.closed
		browsers := g.browsers
		g.mu.Unlock()
		if closed {
			return nil, fmt.Errorf("captcha browser group closed")
		}
		allDead := true
		for _, b := range browsers {
			if b.dead.Load() || b.closed {
				continue
			}
			allDead = false
			if b.busy.Load() {
				continue
			}
			if !b.busy.CompareAndSwap(false, true) {
				continue
			}
			return b, nil
		}
		if allDead {
			return nil, fmt.Errorf("all captcha chromes killed; start them from /admin")
		}
		// Everything usable is busy; wait for a release or a swap, then
		// rescan. Releases push to g.free — wait on it (also woken by Close).
		select {
		case b, ok := <-g.free:
			if !ok {
				return nil, fmt.Errorf("captcha browser group closed")
			}
			if b == nil || b.dead.Load() || b.busy.Load() || b.closed {
				continue // stale/dead release; keep waiting
			}
			if !b.busy.CompareAndSwap(false, true) {
				continue
			}
			return b, nil
		case <-g.done:
			return nil, fmt.Errorf("captcha browser group closed")
		}
	}
}

// releaseBorrowed returns a borrowed browser to rotation. A killed browser is
// dropped: its Chrome is already dead.
func (g *BrowserGroup) releaseBorrowed(b *Browser) {
	b.busy.Store(false)
	if b.dead.Load() {
		return
	}
	g.release(b)
}

// ExtractN borrows a free browser and asks for n tokens in one sticky-tab
// pass. Falls back to n serial extracts on browsers that don't support
// batch (Playground mode — single widget). Same reload/relaunch ladder
// as Extract: a hard failure retries once on the same browser, then
// recycles the Chrome process before giving up.
func (g *BrowserGroup) ExtractN(ctx context.Context, n int) ([]string, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	b, err := g.borrow()
	if err != nil {
		return nil, err
	}

	tryBatch := func(br *Browser) ([]string, error) { return br.ExtractN(ctx, n) }

	toks, err := tryBatch(b)
	if err == nil {
		b.extracts.Add(1)
		g.releaseBorrowed(b)
		return toks, nil
	}
	if ctx.Err() != nil || !isHardExtractFailure(err) {
		g.releaseBorrowed(b)
		return nil, err
	}
	log.Printf("captcha browser hard failure; retrying on same chrome: %v", err)
	toks, err = tryBatch(b)
	if err == nil {
		b.extracts.Add(1)
		g.releaseBorrowed(b)
		return toks, nil
	}
	if ctx.Err() != nil || !isHardExtractFailure(err) {
		g.releaseBorrowed(b)
		return nil, err
	}
	log.Printf("captcha browser hard failure again; recycling chrome: %v", err)
	nb, rerr := g.recycle(b)
	if rerr != nil {
		g.releaseBorrowed(b)
		return nil, fmt.Errorf("%w; chrome recycle: %v", err, rerr)
	}
	toks, err = tryBatch(nb)
	g.releaseBorrowed(nb)
	if err != nil {
		return nil, fmt.Errorf("after chrome recycle: %w", err)
	}
	nb.extracts.Add(1)
	return toks, nil
}

// Extract borrows a free browser, mints one token, then returns it to the pool.
// Recovery is layered, mirroring BoxPwnr NimClient's reload→relaunch ladders:
// a hard failure first gets one cheap in-place retry on the *same* browser
// (the "reload" rung — a cold-start missing-captcha often clears on a second
// execute), and only a second hard failure recycles the whole Chrome process
// (the "relaunch" rung — clears a wedged renderer / stale session).
func (g *BrowserGroup) Extract(ctx context.Context) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	default:
	}
	b, err := g.borrow()
	if err != nil {
		return "", err
	}

	tok, err := b.Extract(ctx)
	if err == nil {
		b.extracts.Add(1)
		g.releaseBorrowed(b)
		return tok, nil
	}
	if ctx.Err() != nil || !isHardExtractFailure(err) {
		g.releaseBorrowed(b)
		return "", err
	}
	// "reload" rung: one cheap retry on the same browser before recycling.
	// Cold-start `missing-captcha` (the dominant transient hard failure on
	// freshly-launched Chromium) clears on a second execute; recycling
	// immediately would pay the ~1–2s Chrome relaunch for a transient blip.
	log.Printf("captcha browser hard failure; retrying on same chrome: %v", err)
	tok, err = b.Extract(ctx)
	if err == nil {
		b.extracts.Add(1)
		g.releaseBorrowed(b)
		return tok, nil
	}
	if ctx.Err() != nil || !isHardExtractFailure(err) {
		g.releaseBorrowed(b)
		return "", err
	}
	// "relaunch" rung: twice-bitten renderer / stale session — rebuild.
	log.Printf("captcha browser hard failure again; recycling chrome: %v", err)
	nb, rerr := g.recycle(b)
	if rerr != nil {
		// old browser already closed inside recycle on success only;
		// on failure keep the slot with the old browser if still usable.
		g.releaseBorrowed(b)
		return "", fmt.Errorf("%w; chrome recycle: %v", err, rerr)
	}
	tok, err = nb.Extract(ctx)
	g.releaseBorrowed(nb)
	if err != nil {
		return "", fmt.Errorf("after chrome recycle: %w", err)
	}
	nb.extracts.Add(1)
	return tok, nil
}

func (g *BrowserGroup) release(b *Browser) {
	g.mu.Lock()
	closed := g.closed
	g.mu.Unlock()
	if closed {
		return
	}
	select {
	case g.free <- b:
	default:
	}
}

// recycle replaces old with a freshly warmed Chrome. On success old is closed
// and must not be released; the caller releases the returned browser instead.
func (g *BrowserGroup) recycle(old *Browser) (*Browser, error) {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil, fmt.Errorf("captcha browser group closed")
	}
	index := -1
	for i, b := range g.browsers {
		if b == old {
			index = i
			break
		}
	}
	if index < 0 {
		g.mu.Unlock()
		return nil, fmt.Errorf("browser not in group")
	}
	g.mu.Unlock()

	nb, err := g.browserFactory(g.parent, g.cfg)
	if err != nil {
		return nil, err
	}

	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		nb.Close()
		return nil, fmt.Errorf("captcha browser group closed")
	}
	if index >= len(g.browsers) || g.browsers[index] != old {
		g.mu.Unlock()
		nb.Close()
		return nil, fmt.Errorf("browser not in group")
	}
	g.browsers[index] = nb
	g.mu.Unlock()

	old.Close()
	log.Printf("captcha browser recycled after hard extract failure")
	return nb, nil
}

// Close stops every Chrome process in the group.
func (g *BrowserGroup) Close() {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return
	}
	g.closed = true
	close(g.done)
	g.mu.Unlock()

	for _, b := range g.browsers {
		b.Close()
	}
}

func isHardExtractFailure(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "empty captcha token") ||
		strings.Contains(s, "re-navigate failed") ||
		strings.Contains(s, "hcaptcha global not ready") ||
		strings.Contains(s, "chromedp navigate") ||
		strings.Contains(s, "captcha token did not refresh")
}
