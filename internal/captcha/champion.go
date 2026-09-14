package captcha

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// ChampionState persists the playground URL that currently mints tokens
// fastest (or at all) across restarts. On a sustained hard-extract
// failure the captcha subsystem can swap to a fallback URL without
// re-running the full SelectChampion benchmark.
//
// State file location: $XDG_CACHE_HOME/glm52-nvidia/playground-state.json
// (defaults to ~/.cache/glm52-nvidia/playground-state.json on Linux,
// %LocalAppData%/glm52-nvidia/playground-state.json on Windows).
type ChampionState struct {
	mu sync.Mutex

	ChampionURL  string    `json:"champion_url"`
	ScoreMS      int64     `json:"score_ms"`        // median steady-state extract latency
	SuccessRate  float64   `json:"success_rate"`    // 1.0 if every trial minted a token
	LastOK       time.Time `json:"last_ok"`
	LastFailed   time.Time `json:"last_failed"`
	FailStreak   int       `json:"fail_streak"`
	FallbackURLs []string  `json:"fallback_urls"`   // ranked by ScoreMS ascending
	Updated      time.Time `json:"updated"`
}

// ChampionStateFile returns the absolute path of the persisted state file.
// Tests + callers override by passing a path directly to Load/Save.
func ChampionStateFile() string {
	if dir := os.Getenv("GLM52_CAPTCHA_STATE"); dir != "" {
		return dir
	}
	// os.UserCacheDir returns ~/.cache on Linux, %LocalAppData% on Windows,
	// ~/Library/Caches on macOS — same convention as nvpi.
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		base, _ = os.UserHomeDir()
	}
	return filepath.Join(base, "glm52-nvidia", "playground-state.json")
}

// LoadChampion reads champion state from path; returns an empty state
// (not an error) if the file is missing so first-run just falls back to
// the default URL. A malformed file is logged + ignored — the worst
// outcome is a re-run of SelectChampion, not a crash.
func LoadChampion(path string) *ChampionState {
	raw, err := os.ReadFile(path)
	if err != nil {
		return &ChampionState{}
	}
	var s ChampionState
	if err := json.Unmarshal(raw, &s); err != nil {
		log.Printf("champion state: ignoring malformed %s: %v", path, err)
		return &ChampionState{}
	}
	return &s
}

// Save writes the state file atomically (tmp + rename) so a crash mid-write
// does not leave a half-decoded JSON on disk.
func (s *ChampionState) Save(path string) error {
	s.mu.Lock()
	s.Updated = time.Now()
	data, err := json.MarshalIndent(s, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// PlaygroundURL satisfies URLProvider: returns the current champion URL,
// falling back to FallbackURLs[0] after MarkFailed trips failStreak, then
// to the package-level default if nothing else is available.
//
// We do NOT auto-rerun SelectChampion here — that is a startup concern
// (-captcha-select-budget). The runtime fallback path is fast-fail + swap.
func (s *ChampionState) PlaygroundURL(ctx context.Context) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ChampionURL != "" && s.FailStreak < maxFailStreak {
		return s.ChampionURL
	}
	if len(s.FallbackURLs) > 0 {
		return s.FallbackURLs[0]
	}
	return defaultPlaygroundURL
}

// maxFailStreak is the number of consecutive hard-extract failures on the
// champion URL before we transparently swap to FallbackURLs[0]. 3 mirrors
// nvpi's ladder: reload (1 retry) + relaunch (1 recycle) + 1 more
// settle-down attempt before yielding the slot.
const maxFailStreak = 3

// MarkFailed is called by the BrowserGroup when isHardExtractFailure lands
// on the current champion. After maxFailStreak consecutive fails,
// PlaygroundURL returns FallbackURLs[0] until SelectChampion reruns.
func (s *ChampionState) MarkFailed(url string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if url != s.ChampionURL {
		return // fail on a fallback URL we already swapped to; ignore
	}
	s.FailStreak++
	s.LastFailed = time.Now()
}

// MarkOK resets the failure counter and updates LastOK. Called after a
// successful token extract.
func (s *ChampionState) MarkOK(url string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if url != s.ChampionURL {
		// Promote the working URL back to champion if it was a fallback.
		if s.ChampionURL == "" || s.FailStreak >= maxFailStreak {
			log.Printf("champion: promote %s (was on fallback)", url)
			s.ChampionURL = url
			s.FallbackURLs = removeString(s.FallbackURLs, url)
			s.FailStreak = 0
		}
		return
	}
	s.FailStreak = 0
	s.LastOK = time.Now()
}

// SelectChampion benchmarks extractMs + success over (per candidate) trial
// count, then persists the winner. budget is the total wall-clock budget;
// candidates is the list of playground URLs to test. trial count is fixed
// at 3 (median of 3 is robust to one bot-wall blip without burning budget).
//
// SelectChampion is meant to run once at startup when
// -captcha-select-budget > 0; it opens its own short-lived Chrome per
// candidate so it does not interfere with the running pool.
//
// ponytail: each candidate is a fresh Chrome. Reusing the pool's browser
// would tie benchmark results to whatever URL the pool last warmed.
func SelectChampion(ctx context.Context, candidates []string, budget time.Duration) (*ChampionState, error) {
	if len(candidates) == 0 {
		return nil, fmt.Errorf("select champion: no candidates")
	}
	if budget <= 0 {
		budget = 4 * time.Minute
	}
	const trialsPerCandidate = 3
	perCandidate := budget / time.Duration(len(candidates))
	if perCandidate < 30*time.Second {
		perCandidate = 30 * time.Second
	}

	type result struct {
		url    string
		median time.Duration
		ok     bool
		err    string
	}
	results := make([]result, len(candidates))
	for i, url := range candidates {
		results[i].url = url
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		log.Printf("champion: benchmarking %s (budget=%s)", url, perCandidate)
		med, ok, err := benchmarkCandidate(ctx, url, trialsPerCandidate, perCandidate)
		results[i].median = med
		results[i].ok = ok
		if err != nil {
			results[i].err = err.Error()
		}
	}

	// Filter to passing candidates, sort by median latency.
	passing := make([]result, 0, len(results))
	for _, r := range results {
		if r.ok {
			passing = append(passing, r)
		} else {
			log.Printf("champion: skipped %s: %s", r.url, r.err)
		}
	}
	if len(passing) == 0 {
		return nil, fmt.Errorf("select champion: no candidate passed (tried %d)", len(candidates))
	}
	sort.Slice(passing, func(i, j int) bool { return passing[i].median < passing[j].median })

	winner := passing[0]
	fallbacks := make([]string, 0, len(passing)-1)
	for _, r := range passing[1:] {
		fallbacks = append(fallbacks, r.url)
	}

	state := &ChampionState{
		ChampionURL:  winner.url,
		ScoreMS:      winner.median.Milliseconds(),
		SuccessRate:  1.0,
		LastOK:       time.Now(),
		FallbackURLs: fallbacks,
	}
	if err := state.Save(ChampionStateFile()); err != nil {
		log.Printf("champion: save failed (non-fatal): %v", err)
	}
	log.Printf("champion: winner=%s median=%s fallbacks=%d",
		winner.url, winner.median, len(fallbacks))
	return state, nil
}

// benchmarkCandidate opens a short-lived Chrome and runs n trials on url.
// Returns (median latency, success, error). median of n<=3 picks the middle;
// with 3 trials one bot-wall blip does not skew the result.
func benchmarkCandidate(ctx context.Context, url string, trials int, budget time.Duration) (time.Duration, bool, error) {
	deadline, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	// Probe sitekey up front so we can fail fast (no point warming Chrome
	// against a retired page).
	sitekey, err := ScrapeSitekey(deadline, url)
	if err != nil {
		return 0, false, err
	}
	_ = sitekey // not used directly here — Sitekey only matters in harness mode

	browser, err := NewBrowser(deadline, BrowserConfig{URLProvider: StaticURLProvider{URL: url}})
	if err != nil {
		return 0, false, fmt.Errorf("alloc: %w", err)
	}
	defer browser.Close()

	durs := make([]time.Duration, 0, trials)
	for i := 0; i < trials; i++ {
		if deadline.Err() != nil {
			return 0, false, deadline.Err()
		}
		start := time.Now()
		if _, err := browser.Extract(deadline); err != nil {
			return 0, false, err
		}
		durs = append(durs, time.Since(start))
	}
	if len(durs) == 0 {
		return 0, false, fmt.Errorf("no trials completed")
	}
	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
	return durs[len(durs)/2], true, nil
}

func removeString(s []string, v string) []string {
	for i, x := range s {
		if x == v {
			return append(s[:i], s[i+1:]...)
		}
	}
	return s
}
