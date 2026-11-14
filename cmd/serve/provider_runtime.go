package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minhhhduc/gnvipi/internal/models"
	"github.com/minhhhduc/gnvipi/internal/providerstore"
)

// snapshotPollInterval is the config_state.version poll that backs up
// LISTEN gnvipi_config (spec §8.2). A missed notification is repaired within
// one interval instead of waiting for the next write.
const snapshotPollInterval = 30 * time.Second

// healthProbeTimeout bounds the /healthz database probe so a hung database
// cannot hang the health endpoint.
const healthProbeTimeout = 2 * time.Second

// providerRuntimeOptions configures the database-backed configuration
// snapshot. Keyring decrypts stored credentials; ListenPool is only used for
// the LISTEN connection, so it is nil in tests that do not need invalidation.
type providerRuntimeOptions struct {
	Keyring      *providerstore.Keyring
	IsProtected  func(model string) bool
	Ping         func(context.Context) error
	ListenPool   *pgxpool.Pool
	PollInterval time.Duration
	Logf         func(format string, args ...any)
}

// providerRuntime owns the process-wide configuration snapshot: one immutable
// view of providers, keys and aliases, replaced only after a committed write
// is observed. Reads never touch the database.
type providerRuntime struct {
	store  *providerstore.Store
	source providerstore.RowSource
	ping   func(context.Context) error
	logf   func(format string, args ...any)
	cancel context.CancelFunc
	done   chan struct{}
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
		IsProtected: opts.IsProtected,
	})
	if err := store.Reload(ctx); err != nil {
		return nil, fmt.Errorf("provider snapshot: %w", err)
	}
	runtime := &providerRuntime{
		store:  store,
		source: source,
		ping:   opts.Ping,
		logf:   logf,
	}
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
			// degradation until the next successful reload.
			logf("provider snapshot: %v", err)
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

// openConfiguredProviderRuntime builds the snapshot runtime for a process whose
// admin state is database-backed. File mode returns nil and keeps the legacy
// file-backed routing.
func openConfiguredProviderRuntime(ctx context.Context, state *appDB) (*providerRuntime, error) {
	if state == nil {
		return nil, nil
	}
	keyring, err := providerstore.LoadKeyringFromEnv()
	if err != nil {
		if !errors.Is(err, providerstore.ErrMissingMasterKey) {
			return nil, fmt.Errorf("provider keyring: %w", err)
		}
		// A missing master key is not fatal: keys without stored secrets stay
		// routable and encrypted keys degrade to diagnostics instead of
		// failing the whole snapshot.
		log.Printf("warning: %s is not set; stored provider credentials cannot be decrypted", "GNVIPI_MASTER_KEY")
	}
	runtime, err := openProviderRuntime(ctx, providerstore.NewDBRowSource(state.pool), providerRuntimeOptions{
		Keyring:     keyring,
		IsProtected: protectedModel,
		Ping:        state.pool.Ping,
		ListenPool:  state.pool,
		Logf:        log.Printf,
	})
	if err != nil {
		return nil, err
	}
	log.Printf("provider snapshot: version=%d loaded from database (%s)", runtime.store.Version(), databaseEnv)
	return runtime, nil
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

// snapshotHealth is the /healthz view of the configuration snapshot (spec §8.3).
type snapshotHealth struct {
	// DBOK reports reachability of the database on this request.
	DBOK bool
	// SnapshotVersion is the version of the published snapshot.
	SnapshotVersion int64
	// ConfigVersion is the last config_state.version seen, which is ahead of
	// the snapshot while a reload is pending or failing.
	ConfigVersion int64
	// SnapshotAge is how long the published snapshot has been in service.
	SnapshotAge time.Duration
	// Degraded reports that the process serves from the last known good
	// snapshot: the database is unreachable or the last reload failed.
	Degraded bool
	// Detail carries the error that caused degradation.
	Detail string
}

// health reports the snapshot state. It never blocks for longer than
// healthProbeTimeout and never mutates the snapshot.
func (r *providerRuntime) health(ctx context.Context) snapshotHealth {
	var health snapshotHealth
	if r == nil {
		return health
	}
	snapshot := r.store.Current()
	if snapshot != nil {
		health.SnapshotVersion = snapshot.Version()
		health.ConfigVersion = snapshot.Version()
		health.SnapshotAge = time.Since(snapshot.LoadedAt())
	}
	health.DBOK = true
	if r.ping != nil {
		pingCtx, cancel := context.WithTimeout(ctx, healthProbeTimeout)
		err := r.ping(pingCtx)
		cancel()
		if err != nil {
			health.DBOK = false
			health.Detail = err.Error()
		}
	}
	if health.DBOK {
		versionCtx, cancel := context.WithTimeout(ctx, healthProbeTimeout)
		version, err := r.source.Version(versionCtx)
		cancel()
		if err != nil {
			health.DBOK = false
			health.Detail = err.Error()
		} else {
			health.ConfigVersion = version
		}
	}
	if err := r.store.LastError(); err != nil {
		health.Detail = err.Error()
	}
	health.Degraded = !health.DBOK || snapshot == nil || r.store.LastError() != nil
	return health
}
