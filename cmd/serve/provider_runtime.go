package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minhhhduc/gnvipi/internal/auth"
	gatewayprovider "github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"github.com/minhhhduc/gnvipi/internal/models"
	"github.com/minhhhduc/gnvipi/internal/providerstore"
)

// snapshotPollInterval is the config_state.version poll that backs up
// LISTEN gnvipi_config (spec §8.2). A missed notification is repaired within
// one interval instead of waiting for the next write.
const snapshotPollInterval = 30 * time.Second

const (
	// healthProbeTimeout is one deadline shared by both /healthz database
	// probes (reachability and version), so a hung database cannot make one
	// health check block for twice the budget.
	healthProbeTimeout = 2 * time.Second

	// healthProbeTTL is how long a probe result is reused. /healthz is
	// unauthenticated, so a burst of checks collapses into one probe instead of
	// leasing a pool connection each.
	healthProbeTTL = time.Second
)

// Fixed /healthz detail categories. Driver text (host, user, database, relation
// and schema names) is never returned from the unauthenticated endpoint; the
// raw error goes to the server log instead.
const (
	healthDetailDatabaseUnreachable = "database unreachable"
	healthDetailReloadFailed        = "reload failed"
	healthDetailWatcherError        = "watcher not refreshing"
)

// providerRuntimeOptions configures the database-backed configuration
// snapshot. Keyring decrypts stored credentials; ListenPool is only used for
// the LISTEN connection, so it is nil in tests that do not need invalidation.
type providerRuntimeOptions struct {
	Keyring      *providerstore.Keyring
	Refs         providerstore.RefResolver
	IsProtected  func(model string) bool
	Ping         func(context.Context) error
	ListenPool   *pgxpool.Pool
	PollInterval time.Duration
	Logf         func(format string, args ...any)
	Audit        providerstore.AuditFunc
}

// providerRuntime owns the process-wide configuration snapshot: one immutable
// view of providers, keys and aliases, replaced only after a committed write
// is observed. Snapshot reads never touch the database; /healthz probes it.
type providerRuntime struct {
	store  *providerstore.Store
	router *providerstore.Router
	source providerstore.RowSource
	ping   func(context.Context) error
	logf   func(format string, args ...any)
	cancel context.CancelFunc
	done   chan struct{}

	healthTTL         time.Duration
	lastConfigVersion atomic.Int64

	watcherMu  sync.Mutex
	watcherErr error

	healthMu       sync.Mutex
	healthResult   snapshotHealth
	healthProbedAt time.Time
	healthInFlight bool
	healthDone     chan struct{}
}

// openProviderRuntime performs the cold-start load and starts the
// invalidation watcher. A database that cannot produce a valid first snapshot
// is a startup error: a database-backed process never serves an empty or
// partially validated configuration (spec §8.3).
func openProviderRuntime(ctx context.Context, source providerstore.RowSource, opts providerRuntimeOptions) (*providerRuntime, error) {
	if source == nil {
		return nil, errors.New("provider runtime: row source is required")
	}
	logf := opts.Logf
	if logf == nil {
		logf = log.Printf
	}
	store := providerstore.NewStore(source, providerstore.StoreOptions{
		Keyring:     opts.Keyring,
		Refs:        opts.Refs,
		IsProtected: opts.IsProtected,
		Audit:       opts.Audit,
		OnPublish: func(snapshot *providerstore.Snapshot) {
			logSnapshotDiagnostics(logf, snapshot)
		},
	})
	reloadCtx, cancelReload := context.WithTimeout(ctx, providerstore.ReloadTimeout)
	defer cancelReload()
	if err := store.Reload(reloadCtx); err != nil {
		return nil, fmt.Errorf("provider snapshot: %w", err)
	}
	runtime := &providerRuntime{
		store:     store,
		router:    providerstore.NewRouter(store),
		source:    source,
		ping:      opts.Ping,
		logf:      logf,
		healthTTL: healthProbeTTL,
	}
	runtime.lastConfigVersion.Store(store.Version())
	if opts.ListenPool == nil {
		return runtime, nil
	}
	poll := opts.PollInterval
	if poll <= 0 {
		poll = snapshotPollInterval
	}
	watchCtx, cancel := context.WithCancel(ctx)
	watcher := &providerstore.Watcher{
		Store:        store,
		Pool:         opts.ListenPool,
		PollInterval: poll,
		OnError: func(err error) {
			// Failures keep the last known good snapshot; /healthz reports the
			// watcher separately so "not refreshing" is distinguishable from
			// "database down".
			runtime.noteWatcherError(err)
			runtime.logf("provider snapshot watcher: %v", err)
		},
		OnHealthy: func() {
			runtime.noteWatcherError(nil)
		},
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := watcher.Run(watchCtx); err != nil && watchCtx.Err() == nil {
			logf("provider snapshot watcher stopped: %v", err)
		}
	}()
	runtime.cancel, runtime.done = cancel, done
	return runtime, nil
}

// logSnapshotDiagnostics reports keys excluded from routing once per published
// snapshot. Key IDs, provider IDs and the fixed reason only: never credential
// material.
func logSnapshotDiagnostics(logf func(string, ...any), snapshot *providerstore.Snapshot) {
	for _, diagnostic := range snapshot.Diagnostics() {
		logf("provider snapshot: key %s (provider %s) excluded from routing: %s [%s]",
			diagnostic.KeyID, diagnostic.ProviderID, diagnostic.Message, diagnostic.Code)
	}
}

// openConfiguredProviderRuntime builds the snapshot runtime for a process whose
// admin state is database-backed. modelsFile locates the adjacent imported
// secrets map used to resolve key_ref credentials at request time. File mode
// returns nil and keeps the legacy file-backed routing.
func openConfiguredProviderRuntime(ctx context.Context, state *appDB, modelsFile string) (*providerRuntime, error) {
	if state == nil {
		return nil, nil
	}
	// key_ref rows resolve against the same imported secrets map as the v2
	// import; a missing file is an empty map, so those keys degrade to the
	// documented credential-unavailable error instead of failing startup.
	secrets, err := gatewayprovider.LoadSecrets(gatewayprovider.DefaultSecretsPath(modelsFile))
	if err != nil {
		return nil, fmt.Errorf("provider key references: %w", err)
	}
	keyring, err := providerstore.LoadKeyringFromEnv()
	if err != nil {
		if !errors.Is(err, providerstore.ErrMissingMasterKey) {
			return nil, fmt.Errorf("provider keyring: %w", err)
		}
		// A missing master key is not fatal: keys without stored secrets stay
		// routable and encrypted keys degrade to diagnostics instead of
		// failing the whole snapshot.
		log.Printf("warning: GNVIPI_MASTER_KEY is not set; keys with stored secrets are excluded from routing")
	}
	runtime, err := openProviderRuntime(ctx, providerstore.NewDBRowSource(state.pool), providerRuntimeOptions{
		Keyring:     keyring,
		Refs:        providerstore.MapRefResolver(secrets),
		IsProtected: protectedModel,
		Ping:        state.pool.Ping,
		ListenPool:  state.pool,
		Logf:        log.Printf,
		Audit:       providerAuditWriter(state.pool),
	})
	if err != nil {
		return nil, err
	}
	log.Printf("provider snapshot: version=%d loaded from database (%s)", runtime.store.Version(), databaseEnv)
	return runtime, nil
}

// providerAuditWriter persists key-level configuration and routing failures.
// Rows carry identifiers and codes only; WriteAudit's allowlist drops anything
// else and no credential value is ever passed in.
func providerAuditWriter(pool *pgxpool.Pool) providerstore.AuditFunc {
	if pool == nil {
		return nil
	}
	return func(event providerstore.AuditEvent) {
		ctx, cancel := context.WithTimeout(context.Background(), healthProbeTimeout)
		defer cancel()
		for i, keyID := range event.KeyIDs {
			entry := auth.AuditEntry{
				ActorType:  "system",
				Action:     event.Code,
				TargetType: "provider_key",
				KeyID:      keyID,
				UserID:     event.UserID,
				Details:    map[string]any{"model": event.Model},
			}
			if i < len(event.ProviderIDs) {
				entry.ProviderID = event.ProviderIDs[i]
			}
			if err := auth.WriteAudit(ctx, pool, entry); err != nil {
				log.Printf("provider audit: %v", err)
			}
		}
	}
}

// protectedModel mirrors the routing registry's protected-model set, so a
// database alias cannot shadow an NVIDIA model unless it sets allow_shadow.
func protectedModel(model string) bool {
	_, ok := models.Models[model]
	return ok
}

// Close stops the invalidation watcher and waits for it to exit.
func (r *providerRuntime) Close() {
	if r == nil || r.cancel == nil {
		return
	}
	r.cancel()
	<-r.done
	r.cancel, r.done = nil, nil
}

// noteWatcherError records the watcher's last connect/notify failure. A nil
// error clears it after the watcher recovers.
func (r *providerRuntime) noteWatcherError(err error) {
	r.watcherMu.Lock()
	r.watcherErr = err
	r.watcherMu.Unlock()
}

func (r *providerRuntime) watcherError() error {
	r.watcherMu.Lock()
	defer r.watcherMu.Unlock()
	return r.watcherErr
}

// snapshotHealth is the /healthz view of the configuration snapshot (spec §8.3).
type snapshotHealth struct {
	// DBOK reports reachability of the database on this request.
	DBOK bool
	// SnapshotVersion is the version of the published snapshot.
	SnapshotVersion int64
	// ConfigVersion is the last config_state.version observed, which is ahead
	// of the snapshot while a reload is pending or failing. It never regresses
	// because a probe failed.
	ConfigVersion int64
	// SnapshotAge is how long the published snapshot has been in service.
	SnapshotAge time.Duration
	// WatcherOK reports that the invalidation watcher is connected and
	// refreshing, so "not refreshing" is distinct from "database down".
	WatcherOK bool
	// Degraded reports that the process serves from the last known good
	// snapshot: the database is unreachable, the watcher is not refreshing, or
	// the last reload failed.
	Degraded bool
	// Detail is a fixed category, never driver error text, because /healthz is
	// unauthenticated.
	Detail string
}

// health reports the snapshot state. A probe is shared by concurrent callers
// and cached briefly, and never runs longer than healthProbeTimeout. The
// returned value never mutates the snapshot.
func (r *providerRuntime) health(ctx context.Context) snapshotHealth {
	if r == nil {
		return snapshotHealth{}
	}
	return r.cachedHealth(ctx)
}

// cachedHealth serves a recent probe result and collapses concurrent checks
// into one database round trip.
func (r *providerRuntime) cachedHealth(ctx context.Context) snapshotHealth {
	r.healthMu.Lock()
	if r.healthInFlight {
		done := r.healthDone
		r.healthMu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			r.healthMu.Lock()
			result := r.healthResult
			completed := !r.healthProbedAt.IsZero()
			r.healthMu.Unlock()
			if completed {
				return result
			}
			return snapshotHealth{
				SnapshotVersion: r.store.Version(),
				ConfigVersion:   r.lastConfigVersion.Load(),
				Degraded:        true,
			}
		}
		r.healthMu.Lock()
		result := r.healthResult
		r.healthMu.Unlock()
		return result
	}
	if !r.healthProbedAt.IsZero() && time.Since(r.healthProbedAt) < r.healthTTL {
		result := r.healthResult
		r.healthMu.Unlock()
		return result
	}
	r.healthInFlight = true
	r.healthDone = make(chan struct{})
	r.healthMu.Unlock()

	result := r.probeHealth()

	r.healthMu.Lock()
	r.healthResult = result
	r.healthProbedAt = time.Now()
	r.healthInFlight = false
	done := r.healthDone
	r.healthDone = nil
	close(done)
	r.healthMu.Unlock()
	return result
}

func (r *providerRuntime) probeHealth() snapshotHealth {
	var health snapshotHealth
	snapshot := r.store.Current()
	if snapshot != nil {
		health.SnapshotVersion = snapshot.Version()
		health.SnapshotAge = time.Since(snapshot.LoadedAt())
		if version := snapshot.Version(); version > r.lastConfigVersion.Load() {
			r.lastConfigVersion.Store(version)
		}
	}
	health.ConfigVersion = r.lastConfigVersion.Load()

	// One deadline for both probes: the version read inherits whatever the
	// reachability check left rather than opening a second full budget.
	probeCtx, cancel := context.WithTimeout(context.Background(), healthProbeTimeout)
	defer cancel()
	health.DBOK = true
	if r.ping != nil {
		if err := r.ping(probeCtx); err != nil {
			health.DBOK = false
			health.Detail = healthDetailDatabaseUnreachable
			r.logProbeError(healthDetailDatabaseUnreachable, err)
		}
	}
	staleConfig := false
	if health.DBOK {
		version, err := r.source.Version(probeCtx)
		if err != nil {
			// db_ok means reachability only: a failed version read on a
			// reachable database is a stale-config/reload failure, not an
			// outage.
			staleConfig = true
			r.logProbeError(healthDetailReloadFailed, err)
		} else if version > r.lastConfigVersion.Load() {
			r.lastConfigVersion.Store(version)
		}
	}
	health.ConfigVersion = r.lastConfigVersion.Load()

	if err := r.watcherError(); err != nil {
		health.WatcherOK = false
		r.logProbeError(healthDetailWatcherError, err)
		if health.Detail == "" {
			health.Detail = healthDetailWatcherError
		}
	} else {
		health.WatcherOK = true
	}
	lastErr := r.store.LastError()
	if lastErr != nil {
		r.logProbeError(healthDetailReloadFailed, lastErr)
	}
	if health.Detail == "" && (lastErr != nil || staleConfig) {
		health.Detail = healthDetailReloadFailed
	}

	// Derive every field from the snapshot captured above: a reload landing
	// mid-probe must not produce a body that mixes two versions.
	health.Degraded = !health.DBOK || snapshot == nil || !health.WatcherOK || lastErr != nil || staleConfig
	return health
}

// logProbeError keeps the operator signal in the server log. The raw error is
// for operators only; /healthz receives the fixed category instead.
func (r *providerRuntime) logProbeError(category string, err error) {
	if r.logf == nil || err == nil {
		return
	}
	r.logf("provider health: %s: %v", category, err)
}
