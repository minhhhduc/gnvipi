package reliability

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
)

// Limiter coordinates concurrency bounds across providers. Each provider's
// MaxConcurrency and QueueTimeout constrain in-flight upstream requests.
// Unbounded or unconfigured providers bypass queue barriers without allocation.
type Limiter struct {
	mu         sync.Mutex
	semaphores map[string]*semaphore
}

// NewLimiter creates an idle concurrency coordinator.
func NewLimiter() *Limiter {
	return &Limiter{
		semaphores: make(map[string]*semaphore),
	}
}

type semaphore struct {
	capacity int
	tokens   chan struct{}
}

func newSemaphore(capacity int) *semaphore {
	return &semaphore{
		capacity: capacity,
		tokens:   make(chan struct{}, capacity),
	}
}

func (s *semaphore) releaseFunc() func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			select {
			case <-s.tokens:
			default:
			}
		})
	}
}

func overloadedError(providerID string, maxConcurrency int) *gwerr.Error {
	msg := fmt.Sprintf("provider max concurrency reached (%d)", maxConcurrency)
	if providerID != "" {
		msg = fmt.Sprintf("provider %q max concurrency reached (%d)", providerID, maxConcurrency)
	}
	return &gwerr.Error{
		Kind:       gwerr.KindOverloaded,
		Status:     http.StatusServiceUnavailable,
		Message:    msg,
		RetryAfter: time.Second,
	}
}

func (l *Limiter) getOrCreate(providerID string, capacity int) *semaphore {
	l.mu.Lock()
	defer l.mu.Unlock()
	sem, exists := l.semaphores[providerID]
	if !exists || sem.capacity != capacity {
		sem = newSemaphore(capacity)
		l.semaphores[providerID] = sem
	}
	return sem
}

// Acquire reserves a concurrency token for the given provider.
// If p.MaxConcurrency is <= 0, the provider is unbounded; Acquire returns a
// no-op release function and nil error immediately.
// When all concurrency slots are saturated, Acquire waits up to p.QueueTimeout.
// If p.QueueTimeout <= 0, it rejects immediately when all slots are full.
// If QueueTimeout expires before a slot is freed, a 503 Overloaded error is returned.
// If ctx cancels while waiting or immediately after acquiring, Acquire aborts and
// releases any held slot. The returned release function is idempotent and safe
// for concurrent calls.
func (l *Limiter) Acquire(ctx context.Context, p *provider.Provider) (func(), error) {
	if p == nil || p.MaxConcurrency <= 0 {
		return func() {}, nil
	}
	return l.AcquireFor(ctx, p.ID, p.MaxConcurrency, p.QueueTimeout)
}

// AcquireFor acquires a concurrency slot for a specific provider ID and limit parameters.
// If queueTimeout <= 0, it rejects immediately when all concurrency slots are full.
func (l *Limiter) AcquireFor(ctx context.Context, providerID string, maxConcurrency int, queueTimeout time.Duration) (func(), error) {
	if maxConcurrency <= 0 {
		return func() {}, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	sem := l.getOrCreate(providerID, maxConcurrency)

	if queueTimeout <= 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case sem.tokens <- struct{}{}:
			// Ensure late cancellation before work begins does not retain the slot.
			if err := ctx.Err(); err != nil {
				<-sem.tokens
				return nil, err
			}
			return sem.releaseFunc(), nil
		default:
			return nil, overloadedError(providerID, maxConcurrency)
		}
	}

	timer := time.NewTimer(queueTimeout)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case sem.tokens <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-sem.tokens
			return nil, err
		}
		return sem.releaseFunc(), nil
	case <-timer.C:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, overloadedError(providerID, maxConcurrency)
	}
}

// InFlight reports the number of concurrently held tokens for providerID.
func (l *Limiter) InFlight(providerID string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	sem, exists := l.semaphores[providerID]
	if !exists {
		return 0
	}
	return len(sem.tokens)
}

// Reset clears all registered provider semaphores. Active holders can safely
// release their tokens without corrupting future provider semaphores.
func (l *Limiter) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	clear(l.semaphores)
}

// Invalidate removes the semaphore for the given provider ID.
func (l *Limiter) Invalidate(providerID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.semaphores, providerID)
}
