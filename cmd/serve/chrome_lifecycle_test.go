package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/captcha"
	"github.com/minhhhduc/gnvipi/internal/models"
	"github.com/minhhhduc/gnvipi/internal/provider/nvidia"
	clipexec "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// Only the external Chrome process boundary is substituted. Token workers,
// cancellation, HTTP handlers and the NVIDIA executor remain real.
type lifecycleTestBrowser struct {
	mu     sync.Mutex
	killed bool
}

func (b *lifecycleTestBrowser) Snapshot() []captcha.ChromeInfo {
	b.mu.Lock()
	defer b.mu.Unlock()
	return []captcha.ChromeInfo{{Index: 0, Killed: b.killed}}
}

func (b *lifecycleTestBrowser) Kill(i int) error {
	if i != 0 {
		return fmt.Errorf("chrome %d not found", i)
	}
	b.mu.Lock()
	b.killed = true
	b.mu.Unlock()
	return nil
}

func (b *lifecycleTestBrowser) Start(i int) error {
	if i != 0 {
		return fmt.Errorf("chrome %d not found", i)
	}
	b.mu.Lock()
	b.killed = false
	b.mu.Unlock()
	return nil
}

func (b *lifecycleTestBrowser) Close() {
	if err := b.Kill(0); err != nil {
		panic(err)
	}
}

func lifecycleTestHTTP(t *testing.T, lifecycle *chromeLifecycle, method, body string) (int, chromeStatus) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Any("/admin/chromes", func(c *gin.Context) { chromeRoutes(c, lifecycle) })
	request := httptest.NewRequestWithContext(t.Context(), method, "/admin/chromes", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, request)
	var status chromeStatus
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatalf("invalid admin status: %v: %s", err, w.Body.String())
	}
	return w.Code, status
}

func requireChromeState(t *testing.T, lifecycle *chromeLifecycle, want string, enabled bool) {
	t.Helper()
	code, status := lifecycleTestHTTP(t, lifecycle, http.MethodGet, "")
	if code != http.StatusOK || status.State != want || status.Enabled != enabled {
		t.Fatalf("GET status=%d %+v, want %s enabled=%t", code, status, want, enabled)
	}
}

func TestChromeAdminStartsIdle(t *testing.T) {
	exec := nvidia.NewExecutor(&nvidia.Options{})
	lifecycle := newChromeLifecycle(t.Context(), exec, func(context.Context) (chromeBrowser, *captcha.Pool, error) {
		panic("startup or status must not allocate Chrome or warm a pool")
	})
	t.Cleanup(lifecycle.Close)
	requireChromeState(t, lifecycle, "idle", false)
	code, status := lifecycleTestHTTP(t, lifecycle, http.MethodGet, "")
	if code != http.StatusOK || status.Chromes == nil || len(status.Chromes) != 0 || status.Ready != 0 {
		t.Fatalf("idle status=%d %+v", code, status)
	}
}

func TestChromeAdminStartStopRestartUsesCurrentTokens(t *testing.T) {
	tokens := make(chan string, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokens <- r.Header.Get("nv-captcha-token")
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(upstream.Close)
	exec := nvidia.NewExecutor(&nvidia.Options{CaptchaWait: time.Second, PredictURL: func(models.ModelInfo) string { return upstream.URL }})
	generation := 0
	var retiredPool *captcha.Pool
	lifecycle := newChromeLifecycle(t.Context(), exec, func(ctx context.Context) (chromeBrowser, *captcha.Pool, error) {
		generation++
		token := fmt.Sprintf("generation-%d", generation)
		pool := captcha.NewPool(ctx, func(ctx context.Context) (string, error) {
			if token != "" {
				out := token
				token = ""
				return out, nil
			}
			<-ctx.Done()
			return "", ctx.Err()
		}, captcha.PoolConfig{Size: 1, Workers: 1, TTL: time.Minute})
		retiredPool = pool
		return &lifecycleTestBrowser{}, pool, nil
	})
	t.Cleanup(lifecycle.Close)
	request := clipexec.Request{Model: "deepseek-ai/deepseek-v4-pro", Payload: []byte(`{"model":"deepseek-ai/deepseek-v4-pro","messages":[{"role":"user","content":"hi"}]}`)}
	options := clipexec.Options{SourceFormat: sdktranslator.FormatOpenAI}
	for _, wantToken := range []string{"generation-1", "generation-2"} {
		code, status := lifecycleTestHTTP(t, lifecycle, http.MethodPost, `{"mode":"start","all":true}`)
		if code != http.StatusOK || !status.Enabled || status.State != "enabled" {
			t.Fatalf("start status=%d %+v", code, status)
		}
		if _, err := exec.Execute(t.Context(), nil, request, options); err != nil {
			t.Fatalf("pooled NVIDIA request: %v", err)
		}
		select {
		case token := <-tokens:
			if token != wantToken {
				t.Fatalf("upstream token=%q, want %q", token, wantToken)
			}
		case <-time.After(time.Second):
			t.Fatal("NVIDIA request did not reach upstream")
		}
		code, status = lifecycleTestHTTP(t, lifecycle, http.MethodPost, `{"mode":"kill","all":true}`)
		if code != http.StatusOK || status.Enabled || status.State != "stopped" || len(status.Chromes) != 0 {
			t.Fatalf("stop status=%d %+v", code, status)
		}
		if _, err := retiredPool.Take(t.Context()); err == nil {
			t.Fatal("retired pool remains usable")
		}
		if _, err := exec.Execute(t.Context(), nil, request, options); err == nil {
			t.Fatal("disabled NVIDIA request reused retired tokens")
		}
	}
}

func TestChromeAdminStopCancelsStartup(t *testing.T) {
	started := make(chan struct{})
	poolStopped := make(chan struct{})
	var browser *lifecycleTestBrowser
	lifecycle := newChromeLifecycle(t.Context(), nvidia.NewExecutor(&nvidia.Options{}), func(ctx context.Context) (chromeBrowser, *captcha.Pool, error) {
		browser = &lifecycleTestBrowser{}
		pool := captcha.NewPool(ctx, func(ctx context.Context) (string, error) {
			close(started)
			<-ctx.Done()
			close(poolStopped)
			return "", ctx.Err()
		}, captcha.PoolConfig{Size: 1, Workers: 1})
		<-ctx.Done()
		// Returning resources after cancellation exercises ownership of a late
		// allocation, even when the underlying factory also reports an error.
		return browser, pool, ctx.Err()
	})
	t.Cleanup(lifecycle.Close)
	startDone := make(chan error, 1)
	go func() { startDone <- lifecycle.Start(t.Context(), true, 0) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("startup did not begin")
	}
	requireChromeState(t, lifecycle, "starting", false)
	code, status := lifecycleTestHTTP(t, lifecycle, http.MethodPost, `{"mode":"stop","all":true}`)
	if code != http.StatusOK || status.State != "stopped" || status.Enabled {
		t.Fatalf("stop during startup=%d %+v", code, status)
	}
	select {
	case err := <-startDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("startup error=%v, want canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled startup did not return")
	}
	select {
	case <-poolStopped:
	case <-time.After(time.Second):
		t.Fatal("startup pool worker leaked")
	}
	if !browser.Snapshot()[0].Killed {
		t.Fatal("late browser allocation was not closed")
	}
}

func TestChromeAdminFailedStartCanRestart(t *testing.T) {
	fail := true
	lifecycle := newChromeLifecycle(t.Context(), nvidia.NewExecutor(&nvidia.Options{}), func(ctx context.Context) (chromeBrowser, *captcha.Pool, error) {
		if fail {
			fail = false
			return nil, nil, errors.New("Chrome binary unavailable")
		}
		return &lifecycleTestBrowser{}, captcha.NewPool(ctx, func(ctx context.Context) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		}, captcha.PoolConfig{Workers: 1}), nil
	})
	t.Cleanup(lifecycle.Close)
	code, status := lifecycleTestHTTP(t, lifecycle, http.MethodPost, `{"mode":"start","all":true}`)
	if code != http.StatusInternalServerError || status.Enabled || status.Error == "" {
		t.Fatalf("failed start=%d %+v", code, status)
	}
	requireChromeState(t, lifecycle, "stopped", false)
	code, status = lifecycleTestHTTP(t, lifecycle, http.MethodPost, `{"mode":"start","all":true}`)
	if code != http.StatusOK || !status.Enabled || status.Error != "" {
		t.Fatalf("restart after failure=%d %+v", code, status)
	}
}

func TestChromeLifecycleShutdownCancelsStartup(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	started := make(chan struct{})
	lifecycle := newChromeLifecycle(ctx, nvidia.NewExecutor(&nvidia.Options{}), func(ctx context.Context) (chromeBrowser, *captcha.Pool, error) {
		close(started)
		<-ctx.Done()
		return nil, nil, ctx.Err()
	})
	t.Cleanup(lifecycle.Close)
	done := make(chan error, 1)
	go func() { done <- lifecycle.Start(t.Context(), true, 0) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("startup did not begin")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("shutdown startup error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel startup")
	}
	lifecycle.Close()
	requireChromeState(t, lifecycle, "stopped", false)
	if err := lifecycle.Start(t.Context(), true, 0); err == nil {
		t.Fatal("closed lifecycle restarted Chrome")
	}
}

func TestChromeAdminSuccessfulStartSurvivesRequestEnd(t *testing.T) {
	exec := nvidia.NewExecutor(&nvidia.Options{})
	workerExited := make(chan struct{})
	workerStarted := make(chan struct{})
	browser := &lifecycleTestBrowser{}
	lifecycle := newChromeLifecycle(t.Context(), exec, func(ctx context.Context) (chromeBrowser, *captcha.Pool, error) {
		pool := captcha.NewPool(ctx, func(ctx context.Context) (string, error) {
			close(workerStarted)
			<-ctx.Done()
			close(workerExited)
			return "", ctx.Err()
		}, captcha.PoolConfig{Size: 1, Workers: 1})
		return browser, pool, nil
	})
	t.Cleanup(lifecycle.Close)
	requestCtx, cancel := context.WithCancel(t.Context())
	if err := lifecycle.Start(requestCtx, true, 0); err != nil {
		t.Fatal(err)
	}
	select {
	case <-workerStarted:
	case <-time.After(time.Second):
		t.Fatal("token worker did not start")
	}
	cancel()
	requireChromeState(t, lifecycle, "enabled", true)
	pool := exec.Pool()
	takeCtx, cancelTake := context.WithCancel(t.Context())
	cancelTake()
	if _, err := pool.Take(takeCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("runtime pool no longer respects caller cancellation: %v", err)
	}
	select {
	case <-workerExited:
		t.Fatal("ending the successful Admin request stopped its token worker")
	default:
	}
	lifecycle.Close()
	select {
	case <-workerExited:
	case <-time.After(time.Second):
		t.Fatal("shutdown leaked active token worker")
	}
	if !browser.Snapshot()[0].Killed {
		t.Fatal("shutdown left the browser running")
	}
	requireChromeState(t, lifecycle, "stopped", false)
}

func TestChromeAdminCanceledRequestDiscardsLateStartup(t *testing.T) {
	started := make(chan struct{})
	browser := &lifecycleTestBrowser{}
	exec := nvidia.NewExecutor(&nvidia.Options{})
	lifecycle := newChromeLifecycle(t.Context(), exec, func(ctx context.Context) (chromeBrowser, *captcha.Pool, error) {
		close(started)
		<-ctx.Done()
		pool := captcha.NewPool(ctx, func(ctx context.Context) (string, error) {
			return "", ctx.Err()
		}, captcha.PoolConfig{Workers: 1})
		return browser, pool, nil
	})
	t.Cleanup(lifecycle.Close)
	requestCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- lifecycle.Start(requestCtx, true, 0) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("startup did not begin")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled start=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled request did not interrupt startup")
	}
	requireChromeState(t, lifecycle, "stopped", false)
	if !browser.Snapshot()[0].Killed {
		t.Fatal("canceled request retained a late startup allocation")
	}
}

// The process boundary can outlive cancellation while a worker waits for a
// browser slot. Closing it must wake that worker before the pool is joined.
type lifecycleGatedBrowser struct {
	lifecycleTestBrowser
	snapshotStarted chan struct{}
	snapshotRelease chan struct{}
	closeStarted    chan struct{}
	closeOnce       sync.Once
}

func (b *lifecycleGatedBrowser) Snapshot() []captcha.ChromeInfo {
	if b.snapshotStarted != nil {
		close(b.snapshotStarted)
		<-b.snapshotRelease
	}
	return b.lifecycleTestBrowser.Snapshot()
}

func (b *lifecycleGatedBrowser) Close() {
	b.closeOnce.Do(func() {
		b.lifecycleTestBrowser.Close()
		if b.closeStarted != nil {
			close(b.closeStarted)
		}
	})
}

func awaitChromeSignal(t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal(message)
	}
}

func TestChromeLifecycleStopWakesBlockedPoolBorrowers(t *testing.T) {
	for _, lateStartup := range []bool{false, true} {
		t.Run(fmt.Sprintf("late-startup=%t", lateStartup), func(t *testing.T) {
			browser := &lifecycleGatedBrowser{closeStarted: make(chan struct{})}
			workersEntered := make(chan struct{}, 2)
			factoryReady := make(chan struct{})
			lifecycle := newChromeLifecycle(t.Context(), nvidia.NewExecutor(&nvidia.Options{}), func(ctx context.Context) (chromeBrowser, *captcha.Pool, error) {
				pool := captcha.NewPool(ctx, func(context.Context) (string, error) {
					workersEntered <- struct{}{}
					// BrowserGroup.borrow waits on group closure, not ctx.Done.
					<-browser.closeStarted
					return "", errors.New("browser group closed")
				}, captcha.PoolConfig{Size: 2, Workers: 2})
				close(factoryReady)
				if lateStartup {
					<-ctx.Done()
				}
				return browser, pool, nil
			})
			// Unblock a regressed pool-first teardown before cleanup joins it.
			t.Cleanup(lifecycle.Close)
			t.Cleanup(browser.Close)
			startDone := make(chan error, 1)
			go func() { startDone <- lifecycle.Start(t.Context(), true, 0) }()
			awaitChromeSignal(t, factoryReady, "factory did not allocate")
			awaitChromeSignal(t, workersEntered, "first pool worker did not enter borrow")
			awaitChromeSignal(t, workersEntered, "second pool worker did not enter borrow")
			if !lateStartup {
				if err := <-startDone; err != nil {
					t.Fatal(err)
				}
			}
			stopDone := make(chan struct{})
			go func() { lifecycle.Stop(); close(stopDone) }()
			awaitChromeSignal(t, stopDone, "Kill All waited for borrowers before closing their browser group")
			if lateStartup {
				if err := <-startDone; !errors.Is(err, context.Canceled) {
					t.Fatalf("late startup error=%v, want canceled", err)
				}
			}
			requireChromeState(t, lifecycle, "stopped", false)
		})
	}
}

func TestChromeLifecycleSlowSnapshotDoesNotBlockKillAllCancellation(t *testing.T) {
	browser := &lifecycleGatedBrowser{
		snapshotStarted: make(chan struct{}),
		snapshotRelease: make(chan struct{}),
	}
	workerStarted := make(chan struct{})
	workerCanceled := make(chan struct{})
	lifecycle := newChromeLifecycle(t.Context(), nvidia.NewExecutor(&nvidia.Options{}), func(ctx context.Context) (chromeBrowser, *captcha.Pool, error) {
		pool := captcha.NewPool(ctx, func(ctx context.Context) (string, error) {
			close(workerStarted)
			<-ctx.Done()
			close(workerCanceled)
			return "", ctx.Err()
		}, captcha.PoolConfig{Size: 1, Workers: 1})
		return browser, pool, nil
	})
	t.Cleanup(lifecycle.Close)
	var releaseOnce sync.Once
	releaseSnapshot := func() { releaseOnce.Do(func() { close(browser.snapshotRelease) }) }
	t.Cleanup(releaseSnapshot)
	if err := lifecycle.Start(t.Context(), true, 0); err != nil {
		t.Fatal(err)
	}
	awaitChromeSignal(t, workerStarted, "pool worker did not start")
	statusDone := make(chan chromeStatus, 1)
	go func() { statusDone <- lifecycle.Status() }()
	awaitChromeSignal(t, browser.snapshotStarted, "status did not reach the slow browser snapshot")
	stopDone := make(chan struct{})
	go func() {
		if err := lifecycle.Kill(true, 0); err != nil {
			t.Error(err)
		}
		close(stopDone)
	}()
	awaitChromeSignal(t, workerCanceled, "slow Admin status prevented Kill All from canceling extraction")
	awaitChromeSignal(t, stopDone, "Kill All did not finish while status was waiting")
	releaseSnapshot()
	<-statusDone
	requireChromeState(t, lifecycle, "stopped", false)
}

func TestChromeLifecycleOverlappingStopsRetireRestartedGeneration(t *testing.T) {
	tokens := make(chan string, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokens <- r.Header.Get("nv-captcha-token")
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(upstream.Close)
	exec := nvidia.NewExecutor(&nvidia.Options{CaptchaWait: time.Second, PredictURL: func(models.ModelInfo) string { return upstream.URL }})
	var pools []*captcha.Pool
	var runtimes []context.Context
	var browsers []*lifecycleTestBrowser
	lifecycle := newChromeLifecycle(t.Context(), exec, func(ctx context.Context) (chromeBrowser, *captcha.Pool, error) {
		token := fmt.Sprintf("generation-%d", len(pools)+1)
		pool := captcha.NewPool(ctx, func(ctx context.Context) (string, error) {
			if token != "" {
				out := token
				token = ""
				return out, nil
			}
			<-ctx.Done()
			return "", ctx.Err()
		}, captcha.PoolConfig{Size: 1, Workers: 1})
		browser := &lifecycleTestBrowser{}
		pools, runtimes, browsers = append(pools, pool), append(runtimes, ctx), append(browsers, browser)
		return browser, pool, nil
	})
	t.Cleanup(lifecycle.Close)
	if err := lifecycle.Start(t.Context(), true, 0); err != nil {
		t.Fatal(err)
	}
	firstCancel := make(chan struct{})
	secondCancel := make(chan struct{})
	releaseSecond := make(chan struct{})
	var releaseOnce sync.Once
	releaseStop := func() { releaseOnce.Do(func() { close(releaseSecond) }) }
	t.Cleanup(releaseStop)
	lifecycle.mu.Lock()
	cancel := lifecycle.cancel
	var cancelCalls atomic.Int32
	lifecycle.cancel = func() {
		cancel()
		switch cancelCalls.Add(1) {
		case 1:
			close(firstCancel)
		case 2:
			close(secondCancel)
			// Suspend Stop B after its early generation cancellation. Stop A
			// can finish and Start C can publish before B resumes cleanup.
			<-releaseSecond
		}
	}
	lifecycle.mu.Unlock()
	lifecycle.opMu.Lock()
	var unlockOnce sync.Once
	unlockOperations := func() { unlockOnce.Do(lifecycle.opMu.Unlock) }
	t.Cleanup(unlockOperations)
	firstStopDone := make(chan struct{})
	go func() { lifecycle.Stop(); close(firstStopDone) }()
	awaitChromeSignal(t, firstCancel, "first Stop did not cancel the running generation")
	secondStopDone := make(chan struct{})
	go func() { lifecycle.Stop(); close(secondStopDone) }()
	awaitChromeSignal(t, secondCancel, "overlapping Stop did not reach early cancellation")
	unlockOperations()
	awaitChromeSignal(t, firstStopDone, "first Stop did not release its generation")
	restartDone := make(chan error, 1)
	go func() { restartDone <- lifecycle.Start(t.Context(), true, 0) }()
	select {
	case err := <-restartDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("early cancellation retained the lifecycle lock across the delayed Stop")
	}
	lease, err := pools[1].TakeLease(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	releaseStop()
	awaitChromeSignal(t, secondStopDone, "delayed Stop did not retire the restarted generation")
	requireChromeState(t, lifecycle, "stopped", false)
	if runtimes[1].Err() == nil {
		t.Fatal("delayed Stop closed browsers without canceling their current runtime")
	}
	if lease.Commit() {
		t.Fatal("a retained token from the retired restart remained sendable")
	}
	for i, pool := range pools {
		if _, err := pool.Take(t.Context()); err == nil {
			t.Fatalf("retired generation %d still dispenses tokens", i+1)
		}
		if !browsers[i].Snapshot()[0].Killed {
			t.Fatalf("retired generation %d left a browser running", i+1)
		}
	}
	request := clipexec.Request{Model: "deepseek-ai/deepseek-v4-pro", Payload: []byte(`{"model":"deepseek-ai/deepseek-v4-pro","messages":[{"role":"user","content":"hi"}]}`)}
	options := clipexec.Options{SourceFormat: sdktranslator.FormatOpenAI}
	if _, err := exec.Execute(t.Context(), nil, request, options); err == nil || !strings.Contains(err.Error(), "captcha token required") {
		t.Fatalf("stopped executor still consults a retired pool: %v", err)
	}
	if err := lifecycle.Start(t.Context(), true, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := exec.Execute(t.Context(), nil, request, options); err != nil {
		t.Fatalf("request after both stops: %v", err)
	}
	if token := <-tokens; token != "generation-3" {
		t.Fatalf("restart sent token=%q, want generation-3", token)
	}
}
