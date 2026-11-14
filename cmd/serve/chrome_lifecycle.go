package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/captcha"
	"github.com/minhhhduc/gnvipi/internal/provider/nvidia"
)

// chromeBrowser is the existing BrowserGroup controller boundary. Tests replace
// only Chrome allocation; the lifecycle and token pool are not substituted.
type chromeBrowser interface {
	Snapshot() []captcha.ChromeInfo
	Start(int) error
	Kill(int) error
	Close()
}

type chromeStatus struct {
	Enabled     bool                 `json:"enabled"`
	State       string               `json:"state"`
	Chromes     []captcha.ChromeInfo `json:"chromes"`
	Ready       int                  `json:"ready"`
	PoolSize    int                  `json:"pool_size"`
	PoolWorkers int                  `json:"pool_workers"`
	Error       string               `json:"error,omitempty"`
}

// Bounds for the admin-controlled pool target.
const (
	minPoolSize    = 1
	maxPoolSize    = 64
	minPoolWorkers = 1
	maxPoolWorkers = 16
)

type chromeLifecycle struct {
	ctx  context.Context
	exec *nvidia.Executor
	// Points at the -pool-size / -pool-workers values. Start reads them through
	// the factory closure, so an admin change applies on the next Start All.
	poolSize    *int
	poolWorkers *int
	factory     func(context.Context) (chromeBrowser, *captcha.Pool, error)

	// Operations serialize allocation/slot changes/cleanup. Stop cancels under
	// mu before waiting for opMu, so a slow Chrome launch cannot block Kill All.
	opMu    sync.Mutex
	mu      sync.Mutex
	state   string
	closed  bool
	browser chromeBrowser
	pool    *captcha.Pool
	cancel  context.CancelFunc
	lastErr string
}

func newChromeLifecycle(ctx context.Context, exec *nvidia.Executor, factory func(context.Context) (chromeBrowser, *captcha.Pool, error)) *chromeLifecycle {
	return &chromeLifecycle{ctx: ctx, exec: exec, factory: factory, state: "idle"}
}

// newChromeLifecycleWithTarget also exposes the admin-editable pool target.
func newChromeLifecycleWithTarget(ctx context.Context, exec *nvidia.Executor, poolSize, poolWorkers *int, factory func(context.Context) (chromeBrowser, *captcha.Pool, error)) *chromeLifecycle {
	lifecycle := newChromeLifecycle(ctx, exec, factory)
	lifecycle.poolSize, lifecycle.poolWorkers = poolSize, poolWorkers
	return lifecycle
}

// SetTarget stores how many captcha tokens to keep ready and how many extractor
// workers to run. It takes effect on the next Start All while Chrome is running.
func (l *chromeLifecycle) SetTarget(size, workers int) error {
	if size < minPoolSize || size > maxPoolSize {
		return fmt.Errorf("ready tokens must be %d-%d", minPoolSize, maxPoolSize)
	}
	if workers < minPoolWorkers || workers > maxPoolWorkers {
		return fmt.Errorf("workers must be %d-%d", minPoolWorkers, maxPoolWorkers)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.poolSize != nil {
		*l.poolSize = size
	}
	if l.poolWorkers != nil {
		*l.poolWorkers = workers
	}
	return nil
}

// target returns the configured pool size and worker count.
func (l *chromeLifecycle) target() (size, workers int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.poolSize != nil {
		size = *l.poolSize
	}
	if l.poolWorkers != nil {
		workers = *l.poolWorkers
	}
	return size, workers
}

func (l *chromeLifecycle) Status() chromeStatus {
	l.mu.Lock()
	status := chromeStatus{State: l.state, Chromes: []captcha.ChromeInfo{}, Error: l.lastErr}
	if l.poolSize != nil {
		status.PoolSize = *l.poolSize
	}
	if l.poolWorkers != nil {
		status.PoolWorkers = *l.poolWorkers
	}
	browser, pool := l.browser, l.pool
	l.mu.Unlock()
	if browser != nil && status.State == "enabled" {
		status.Enabled = true
		status.Chromes = browser.Snapshot()
		status.Ready = pool.Ready()
	}
	return status
}

func (l *chromeLifecycle) Start(ctx context.Context, all bool, index int) error {
	l.opMu.Lock()
	defer l.opMu.Unlock()
	l.mu.Lock()
	if err := ctx.Err(); err != nil {
		l.mu.Unlock()
		return err
	}
	if l.closed || l.ctx.Err() != nil || l.state == "stopping" {
		l.mu.Unlock()
		return errors.New("captcha Chrome is stopped")
	}
	if l.browser != nil {
		browser := l.browser
		l.mu.Unlock()
		if !all {
			return browser.Start(index)
		}
		for _, slot := range browser.Snapshot() {
			if slot.Killed {
				if err := browser.Start(slot.Index); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if !all {
		l.mu.Unlock()
		return fmt.Errorf("chrome %d not found; use Start All to enable captcha Chrome", index)
	}
	attemptCtx, cancel := context.WithCancel(l.ctx)
	l.cancel, l.state, l.lastErr = cancel, "starting", ""
	l.mu.Unlock()
	stopRequestCancel := context.AfterFunc(ctx, cancel)
	browser, pool, err := l.factory(attemptCtx)
	// A successful Admin action owns resources beyond the HTTP request. A
	// canceled action must never publish a late, already-canceled allocation.
	if !stopRequestCancel() {
		cancel()
	}
	l.mu.Lock()
	if err == nil {
		err = attemptCtx.Err()
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil && (l.closed || l.state != "starting") {
		err = context.Canceled
	}
	if err == nil && (browser == nil || pool == nil) {
		err = errors.New("captcha Chrome factory returned incomplete resources")
	}
	if err == nil {
		l.browser, l.pool, l.state = browser, pool, "enabled"
		l.exec.SetPool(pool)
		l.mu.Unlock()
		return nil
	}
	if l.state == "starting" {
		l.state, l.lastErr = "stopped", err.Error()
	}
	l.cancel = nil
	l.mu.Unlock()
	cancel()
	closeChromeResources(browser, pool)
	return err
}

func (l *chromeLifecycle) Kill(all bool, index int) error {
	if all {
		l.Stop()
		return nil
	}
	l.opMu.Lock()
	defer l.opMu.Unlock()
	l.mu.Lock()
	browser := l.browser
	l.mu.Unlock()
	if browser == nil {
		return fmt.Errorf("chrome %d not found", index)
	}
	return browser.Kill(index)
}

func closeChromeResources(browser chromeBrowser, pool *captcha.Pool) {
	// Cancellation interrupts extraction, and closing the group wakes workers
	// blocked in borrow (which waits on group closure rather than its context).
	// Both must happen before waiting for every token worker to exit.
	if browser != nil {
		browser.Close()
	}
	if pool != nil {
		pool.Close()
	}
}

func (l *chromeLifecycle) Stop() {
	l.mu.Lock()
	l.state = "stopping"
	l.exec.SetPool(nil)
	cancel := l.cancel
	l.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	l.opMu.Lock()
	defer l.opMu.Unlock()
	l.mu.Lock()
	// Another Stop can finish and a Start can publish while this caller waits
	// for opMu. Retire the actual current generation, not only the early one.
	l.state = "stopping"
	l.exec.SetPool(nil)
	cancel = l.cancel
	browser, pool := l.browser, l.pool
	l.browser, l.pool, l.cancel = nil, nil, nil
	l.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	closeChromeResources(browser, pool)
	l.mu.Lock()
	l.state, l.lastErr = "stopped", ""
	l.mu.Unlock()
}

func (l *chromeLifecycle) Close() {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	l.Stop()
}

func chromeRoutes(c *gin.Context, lifecycle *chromeLifecycle) {
	switch c.Request.Method {
	case http.MethodGet:
		c.JSON(http.StatusOK, lifecycle.Status())
	case http.MethodPost:
		var payload struct {
			Index       int    `json:"index"`
			Mode        string `json:"mode"`
			All         bool   `json:"all"`
			PoolSize    *int   `json:"pool_size"`
			PoolWorkers *int   `json:"pool_workers"`
		}
		if c.ContentType() == "application/x-www-form-urlencoded" {
			payload.Mode = c.Request.FormValue("mode")
			payload.All = c.Request.FormValue("all") == "true"
			if index := c.Request.FormValue("index"); index != "" || !payload.All {
				var err error
				payload.Index, err = strconv.Atoi(index)
				if err != nil {
					c.JSON(http.StatusBadRequest, gin.H{"error": "index must be an integer"})
					return
				}
			}
		} else if err := c.ShouldBindJSON(&payload); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		var err error
		switch payload.Mode {
		case "start", "resume":
			if payload.PoolSize != nil || payload.PoolWorkers != nil {
				size, workers := lifecycle.target()
				if payload.PoolSize != nil {
					size = *payload.PoolSize
				}
				if payload.PoolWorkers != nil {
					workers = *payload.PoolWorkers
				}
				if targetErr := lifecycle.SetTarget(size, workers); targetErr != nil {
					c.JSON(http.StatusBadRequest, gin.H{"error": targetErr.Error()})
					return
				}
			}
			err = lifecycle.Start(c.Request.Context(), payload.All, payload.Index)
		case "kill", "stop", "pause":
			err = lifecycle.Kill(payload.All, payload.Index)
		case "config":
			if payload.PoolSize == nil && payload.PoolWorkers == nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "pool_size or pool_workers is required"})
				return
			}
			size, workers := lifecycle.target()
			if payload.PoolSize != nil {
				size = *payload.PoolSize
			}
			if payload.PoolWorkers != nil {
				workers = *payload.PoolWorkers
			}
			if err = lifecycle.SetTarget(size, workers); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "mode must be kill, stop, start or config"})
			return
		}
		status := lifecycle.Status()
		if err != nil {
			status.Error = err.Error()
			code := http.StatusInternalServerError
			if !payload.All {
				code = http.StatusNotFound
			}
			c.JSON(code, status)
			return
		}
		c.JSON(http.StatusOK, status)
	default:
		c.JSON(http.StatusMethodNotAllowed, gin.H{"error": "method not supported"})
	}
}
