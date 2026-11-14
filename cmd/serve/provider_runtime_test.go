package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minhhhduc/gnvipi/internal/provider/nvidia"
	"github.com/minhhhduc/gnvipi/internal/providerstore"
)

// serveTestApplicationName scopes the listener-count queries to this package's
// test connections. go test runs packages in parallel against one
// GNVIPI_TEST_DATABASE_URL, so a database-global count collides with
// internal/providerstore's watchers.
const serveTestApplicationName = "gnvipi-serve-test"

// captureLogf collects log lines emitted by the runtime under test.
func captureLogf() (logf func(string, ...any), logged func() string) {
	var mu sync.Mutex
	var lines []string
	logf = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	logged = func() string {
		mu.Lock()
		defer mu.Unlock()
		return strings.Join(lines, "\n")
	}
	return logf, logged
}

// stubSnapshotSource lets the runtime tests control the first load and the
// config_state.version without a database.
type stubSnapshotSource struct {
	mu      sync.Mutex
	rows    providerstore.Rows
	version int64
	loadErr error
	// versionHook, when set, replaces Version so a test can observe the
	// deadline the health probe passes to the version read.
	versionHook func(context.Context) (int64, error)
	// loadHook, when set, replaces LoadConfig so a load can fail while the
	// version read still succeeds (a reload failure, not a database outage).
	loadHook func(context.Context) (providerstore.Rows, int64, error)
}

func (s *stubSnapshotSource) LoadConfig(ctx context.Context) (providerstore.Rows, int64, error) {
	if err := ctx.Err(); err != nil {
		return providerstore.Rows{}, 0, err
	}
	s.mu.Lock()
	hook := s.loadHook
	rows, version, loadErr := s.rows, s.version, s.loadErr
	s.mu.Unlock()
	if hook != nil {
		return hook(ctx)
	}
	if loadErr != nil {
		return providerstore.Rows{}, 0, loadErr
	}
	return rows, version, nil
}

func (s *stubSnapshotSource) Version(ctx context.Context) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	hook := s.versionHook
	s.mu.Unlock()
	if hook != nil {
		return hook(ctx)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return 0, s.loadErr
	}
	return s.version, nil
}

func (s *stubSnapshotSource) setVersion(version int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.version = version
}

func (s *stubSnapshotSource) setLoadErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadErr = err
}

func stubSnapshotRows() providerstore.Rows {
	return providerstore.Rows{
		Providers: []providerstore.ProviderRow{{
			ID:       "stub-provider",
			Name:     "Stub",
			Protocol: "openai-chat",
			BaseURL:  "https://stub.example.com/v1",
			Enabled:  true,
			TLS:      "default",
		}},
		Keys: []providerstore.KeyRow{{
			ID: "stub-key", ProviderID: "stub-provider", OwnerID: "11111111-1111-1111-1111-111111111111",
			Name: "key", Enabled: true, AuthScheme: providerstore.SchemeNone,
		}},
		KeyModels: []providerstore.KeyModelRow{{
			KeyID: "stub-key", ProviderID: "stub-provider",
			OwnerID: "11111111-1111-1111-1111-111111111111",
			Alias:   "stub-model", UpstreamModel: "upstream-stub",
		}},
	}
}

func stubRuntimeOptions() providerRuntimeOptions {
	return providerRuntimeOptions{}
}

// TestOpenProviderRuntimeFailsClosedWhenDatabaseIsUnreachable is the cold-start
// contract of spec §8.3: a database-backed process never serves an empty
// configuration, so an unreachable database is a startup error.
func TestOpenProviderRuntimeFailsClosedWhenDatabaseIsUnreachable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, "postgres://runtime@127.0.0.1:1/gnvipi?sslmode=disable&connect_timeout=2")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()

	runtime, err := openProviderRuntime(ctx, providerstore.NewDBRowSource(pool), stubRuntimeOptions())
	if runtime != nil {
		runtime.Close()
		t.Fatal("unreachable database returned a provider runtime")
	}
	if err == nil || !strings.Contains(err.Error(), "provider snapshot") {
		t.Fatalf("cold start error = %v, want provider snapshot context", err)
	}
}

// TestOpenProviderRuntimeRejectsInvalidConfiguration proves validation happens
// before the first request is served, not lazily on a routing miss.
func TestOpenProviderRuntimeRejectsInvalidConfiguration(t *testing.T) {
	source := &stubSnapshotSource{version: 3, rows: providerstore.Rows{
		Providers: []providerstore.ProviderRow{{
			ID: "stub-provider", Name: "Stub", Protocol: "carrier-pigeon",
			BaseURL: "https://stub.example.com/v1", Enabled: true, TLS: "default",
		}},
	}}

	runtime, err := openProviderRuntime(t.Context(), source, stubRuntimeOptions())
	if runtime != nil {
		runtime.Close()
		t.Fatal("invalid configuration returned a provider runtime")
	}
	if err == nil || !strings.Contains(err.Error(), "provider snapshot") {
		t.Fatalf("cold start error = %v, want provider snapshot context", err)
	}
}

// TestProviderRuntimeHealthReportsLastSnapshotWhenDatabaseIsDown covers spec
// §8.3: the process keeps the last good snapshot and reports degraded, with
// the snapshot version, the last seen config version and the snapshot age.
func TestProviderRuntimeHealthReportsLastSnapshotWhenDatabaseIsDown(t *testing.T) {
	source := &stubSnapshotSource{version: 7, rows: stubSnapshotRows()}

	var pingErr error
	var pingMu sync.Mutex
	ping := func(context.Context) error {
		pingMu.Lock()
		defer pingMu.Unlock()
		return pingErr
	}

	runtime, err := openProviderRuntime(t.Context(), source, providerRuntimeOptions{Ping: ping})
	if err != nil {
		t.Fatalf("open provider runtime: %v", err)
	}
	runtime.healthTTL = -1 // no probe cache: the test changes state between checks
	t.Cleanup(runtime.Close)

	health := runtime.health(t.Context())
	if !health.DBOK || health.Degraded {
		t.Fatalf("healthy runtime reported db_ok=%v degraded=%v", health.DBOK, health.Degraded)
	}
	if health.SnapshotVersion != 7 || health.ConfigVersion != 7 {
		t.Fatalf("healthy runtime versions = snapshot %d config %d, want 7/7",
			health.SnapshotVersion, health.ConfigVersion)
	}
	if health.SnapshotAge < 0 {
		t.Fatalf("snapshot age = %v, want >= 0", health.SnapshotAge)
	}

	// A committed write advances config_state.version before this instance
	// reloads, so config_version runs ahead of the published snapshot.
	source.setVersion(8)
	health = runtime.health(t.Context())
	if health.ConfigVersion != 8 {
		t.Fatalf("config version = %d, want the committed 8", health.ConfigVersion)
	}
	if health.SnapshotVersion != 7 {
		t.Fatalf("snapshot version = %d, want the published 7", health.SnapshotVersion)
	}

	// The database disappears: the snapshot must survive and health must say so.
	pingMu.Lock()
	pingErr = errors.New("connection refused")
	pingMu.Unlock()

	health = runtime.health(t.Context())
	if health.DBOK {
		t.Fatal("db_ok = true while the database is unreachable")
	}
	if !health.Degraded {
		t.Fatal("health is not degraded while the database is unreachable")
	}
	if health.SnapshotVersion != 7 {
		t.Fatalf("snapshot version = %d, want the last known good 7", health.SnapshotVersion)
	}
	if health.Detail == "" {
		t.Fatal("degraded health has no detail")
	}
	if got := runtime.store.Version(); got != 7 {
		t.Fatalf("store version = %d, want the last known good 7", got)
	}
}

// TestProviderRuntimeHealthReportsFailedReload keeps the "publish only a fully
// validated snapshot" rule observable: a failed reload never replaces the
// current snapshot but must surface as degraded.
func TestProviderRuntimeHealthReportsFailedReload(t *testing.T) {
	source := &stubSnapshotSource{version: 4, rows: stubSnapshotRows()}
	runtime, err := openProviderRuntime(t.Context(), source, stubRuntimeOptions())
	if err != nil {
		t.Fatalf("open provider runtime: %v", err)
	}
	runtime.healthTTL = -1
	t.Cleanup(runtime.Close)

	if err := runtime.store.Reload(t.Context()); err != nil {
		t.Fatalf("baseline reload: %v", err)
	}
	if health := runtime.health(t.Context()); health.Degraded {
		t.Fatalf("baseline health is degraded: %s", health.Detail)
	}

	source.setLoadErr(errors.New("relation provider_keys does not exist"))
	if err := runtime.store.Reload(t.Context()); err == nil {
		t.Fatal("reload with a failing source returned no error")
	}

	health := runtime.health(t.Context())
	if !health.Degraded {
		t.Fatal("health is not degraded after a failed reload")
	}
	if health.SnapshotVersion != 4 {
		t.Fatalf("snapshot version = %d, want the last known good 4", health.SnapshotVersion)
	}
}

// TestHealthzReportsSnapshotState asserts the /healthz enrichment and that a
// file-backed (no runtime) instance keeps its previous response shape.
func TestHealthzReportsSnapshotState(t *testing.T) {
	gin.SetMode(gin.TestMode)
	getModels := func() []gatewayModel { return nil }
	exec := nvidia.NewExecutor(&nvidia.Options{})

	serve := func(handler gin.HandlerFunc) (*httptest.ResponseRecorder, map[string]any) {
		t.Helper()
		engine := gin.New()
		engine.Use(handler)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil)
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("parse /healthz body %q: %v", rec.Body.String(), err)
		}
		return rec, body
	}

	rec, body := serve(modelsAndHealthMiddleware(getModels, exec, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200", rec.Code)
	}
	if body["ok"] != true {
		t.Fatalf("/healthz ok = %v, want true", body["ok"])
	}
	if _, ok := body["db_ok"]; ok {
		t.Fatalf("file-backed /healthz reported db_ok: %v", body)
	}

	source := &stubSnapshotSource{version: 11, rows: stubSnapshotRows()}
	runtime, err := openProviderRuntime(t.Context(), source, stubRuntimeOptions())
	if err != nil {
		t.Fatalf("open provider runtime: %v", err)
	}
	runtime.healthTTL = -1
	t.Cleanup(runtime.Close)

	rec, body = serve(modelsAndHealthMiddleware(getModels, exec, runtime))
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200", rec.Code)
	}
	if body["ok"] != true || body["db_ok"] != true || body["degraded"] != false {
		t.Fatalf("/healthz healthy body = %v", body)
	}
	if body["snapshot_version"] != float64(11) || body["config_version"] != float64(11) {
		t.Fatalf("/healthz versions = %v/%v, want 11/11", body["snapshot_version"], body["config_version"])
	}
	if _, ok := body["snapshot_age"]; !ok {
		t.Fatalf("/healthz has no snapshot_age: %v", body)
	}

	source.setLoadErr(errors.New("database is down"))
	if err := runtime.store.Reload(t.Context()); err == nil {
		t.Fatal("reload with a failing source returned no error")
	}
	rec, body = serve(modelsAndHealthMiddleware(getModels, exec, runtime))
	if rec.Code != http.StatusOK {
		t.Fatalf("degraded /healthz status = %d, want 200: the process still serves", rec.Code)
	}
	if body["ok"] != false || body["degraded"] != true {
		t.Fatalf("degraded /healthz body = %v", body)
	}
	if body["snapshot_version"] != float64(11) {
		t.Fatalf("degraded snapshot_version = %v, want 11", body["snapshot_version"])
	}
	if body["status"] != "degraded" {
		t.Fatalf("degraded status = %v, want \"degraded\"", body["status"])
	}
}

// TestProviderRuntimeKeepsLastSnapshotWhenDatabaseGoesDown is the end-to-end
// form of spec §8.3 against PostgreSQL: after a successful load the database
// disappears, the snapshot stays published and /healthz reports degraded.
func TestProviderRuntimeKeepsLastSnapshotWhenDatabaseGoesDown(t *testing.T) {
	dsn := os.Getenv("GNVIPI_TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("GNVIPI_REQUIRE_DB_TESTS") == "true" {
			t.Fatal("GNVIPI_TEST_DATABASE_URL is required when GNVIPI_REQUIRE_DB_TESTS=true")
		}
		t.Skip("GNVIPI_TEST_DATABASE_URL is not set; skipping DB-backed runtime tests")
	}
	ctx := t.Context()
	pool := newAdminTestPool(t, dsn)

	if _, err := pool.Exec(ctx,
		`INSERT INTO providers(id, name, protocol, base_url)
		 VALUES('db201', 'DB 201', 'openai-chat', 'https://db201.example.com/v1')`); err != nil {
		t.Fatalf("insert provider: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO provider_keys(id, provider_id, name, auth_scheme)
		 VALUES('22222222-2222-2222-2222-222222222222', 'db201', 'shared', 'none')`); err != nil {
		t.Fatalf("insert shared key: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO key_models(key_id, alias, upstream_model)
		 VALUES('22222222-2222-2222-2222-222222222222', 'db201-model', 'upstream-db201')`); err != nil {
		t.Fatalf("insert alias: %v", err)
	}

	runtime, err := openProviderRuntime(ctx, providerstore.NewDBRowSource(pool), providerRuntimeOptions{Ping: pool.Ping})
	if err != nil {
		t.Fatalf("open provider runtime: %v", err)
	}
	runtime.healthTTL = -1
	t.Cleanup(runtime.Close)

	var configVersion int64
	if err := pool.QueryRow(ctx, `SELECT version FROM config_state WHERE id`).Scan(&configVersion); err != nil {
		t.Fatalf("read config_state.version: %v", err)
	}
	if _, ok := runtime.store.Current().Provider("db201"); !ok {
		t.Fatal("snapshot is missing the provider loaded from the database")
	}
	loaded := runtime.health(ctx)
	if !loaded.DBOK || loaded.Degraded {
		t.Fatalf("after load: db_ok=%v degraded=%v", loaded.DBOK, loaded.Degraded)
	}
	if loaded.SnapshotVersion != configVersion || loaded.ConfigVersion != configVersion {
		t.Fatalf("after load: snapshot=%d config=%d, want %d", loaded.SnapshotVersion, loaded.ConfigVersion, configVersion)
	}

	// The database goes away.
	pool.Close()

	health := runtime.health(ctx)
	if health.DBOK {
		t.Fatal("db_ok = true after the database went away")
	}
	if !health.Degraded {
		t.Fatal("health is not degraded after the database went away")
	}
	if health.SnapshotVersion != configVersion {
		t.Fatalf("snapshot version = %d after the database went away, want the last known good %d",
			health.SnapshotVersion, configVersion)
	}
	if health.ConfigVersion != configVersion {
		t.Fatalf("config version = %d after the database went away, want the last seen %d",
			health.ConfigVersion, configVersion)
	}
	if health.Detail == "" {
		t.Fatal("degraded health has no detail")
	}

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(modelsAndHealthMiddleware(func() []gatewayModel { return nil }, nvidia.NewExecutor(&nvidia.Options{}), runtime))
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200 while serving the last snapshot", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse /healthz body: %v", err)
	}
	if body["ok"] != false || body["degraded"] != true || body["db_ok"] != false {
		t.Fatalf("degraded /healthz body = %v", body)
	}
	if body["snapshot_version"] != float64(configVersion) {
		t.Fatalf("/healthz snapshot_version = %v, want %d", body["snapshot_version"], configVersion)
	}
}

// TestProviderRuntimeWatchesCommittedWrites proves the wiring, not just the
// library: the runtime opened for a real pool publishes a committed write
// through LISTEN gnvipi_config (the poll interval is an hour), and Close tears
// down the listener connection.
func TestProviderRuntimeWatchesCommittedWrites(t *testing.T) {
	dsn := os.Getenv("GNVIPI_TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("GNVIPI_REQUIRE_DB_TESTS") == "true" {
			t.Fatal("GNVIPI_TEST_DATABASE_URL is required when GNVIPI_REQUIRE_DB_TESTS=true")
		}
		t.Skip("GNVIPI_TEST_DATABASE_URL is not set; skipping DB-backed runtime tests")
	}
	ctx := t.Context()
	pool := newAdminTestPool(t, dsn)

	if _, err := pool.Exec(ctx,
		`INSERT INTO providers(id, name, protocol, base_url)
		 VALUES('watch201', 'Watch 201', 'openai-chat', 'https://watch201.example.com/v1')`); err != nil {
		t.Fatalf("insert provider: %v", err)
	}

	before := listenerSessions(t, ctx, pool)
	runtime, err := openProviderRuntime(ctx, providerstore.NewDBRowSource(pool), providerRuntimeOptions{
		ListenPool:   pool,
		PollInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("open provider runtime: %v", err)
	}
	runtime.healthTTL = -1
	t.Cleanup(runtime.Close)
	baseline := runtime.health(ctx).SnapshotVersion
	waitForListenerSessions(t, ctx, pool, before+1)

	if _, err := pool.Exec(ctx,
		`INSERT INTO providers(id, name, protocol, base_url)
		 VALUES('watch201-second', 'Watch 201 second', 'openai-chat', 'https://watch201b.example.com/v1')`); err != nil {
		t.Fatalf("insert second provider: %v", err)
	}
	var committed int64
	if err := pool.QueryRow(ctx, `SELECT version FROM config_state WHERE id`).Scan(&committed); err != nil {
		t.Fatalf("read config_state.version: %v", err)
	}
	if committed <= baseline {
		t.Fatalf("config_state.version = %d after a committed write, want > %d", committed, baseline)
	}

	deadline := time.Now().Add(10 * time.Second)
	for runtime.health(ctx).SnapshotVersion != committed {
		if time.Now().After(deadline) {
			t.Fatalf("snapshot version = %d after 10s, want %d (last error: %v)",
				runtime.health(ctx).SnapshotVersion, committed, runtime.store.LastError())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, ok := runtime.store.Current().Provider("watch201-second"); !ok {
		t.Fatal("snapshot is missing the provider written after startup")
	}

	runtime.Close()
	waitForListenerSessions(t, ctx, pool, before)
}

// listenerSessions counts this test package's sessions parked in
// LISTEN gnvipi_config. Counting is scoped by application_name because another
// package's watcher against the same database must not satisfy this assertion.
func listenerSessions(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_stat_activity
		  WHERE datname = current_database()
		    AND application_name = $1
		    AND query = 'LISTEN gnvipi_config'`, serveTestApplicationName).Scan(&count); err != nil {
		t.Fatalf("count listener sessions: %v", err)
	}
	return count
}

func waitForListenerSessions(t *testing.T, ctx context.Context, pool *pgxpool.Pool, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for listenerSessions(t, ctx, pool) != want {
		if time.Now().After(deadline) {
			t.Fatalf("LISTEN gnvipi_config sessions = %d after 10s, want %d",
				listenerSessions(t, ctx, pool), want)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for %d listener sessions: %v", want, ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// TestProviderRuntimeHealthRedactsDatabaseErrors keeps driver text — DSN host,
// user, database and relation/schema names — out of the unauthenticated
// /healthz body while retaining an operator signal in the server log.
func TestProviderRuntimeHealthRedactsDatabaseErrors(t *testing.T) {
	logf, logged := captureLogf()
	const connectionDetail = "failed to connect to `host=db.internal user=gnvipi database=gnvipi`: dial error"
	pingErr := errors.New(connectionDetail)
	source := &stubSnapshotSource{version: 7, rows: stubSnapshotRows()}
	runtime, err := openProviderRuntime(t.Context(), source, providerRuntimeOptions{
		Ping: func(context.Context) error { return pingErr },
		Logf: logf,
	})
	if err != nil {
		t.Fatalf("open provider runtime: %v", err)
	}
	runtime.healthTTL = -1
	t.Cleanup(runtime.Close)

	health := runtime.health(t.Context())
	if health.Detail != "database unreachable" {
		t.Fatalf("health detail = %q, want the fixed category \"database unreachable\"", health.Detail)
	}
	for _, secret := range []string{"db.internal", "user=gnvipi", "database=gnvipi"} {
		if strings.Contains(health.Detail, secret) {
			t.Fatalf("health detail leaks %q: %q", secret, health.Detail)
		}
	}
	if !strings.Contains(logged(), "host=db.internal") {
		t.Fatalf("operator log is missing the raw probe error: %q", logged())
	}

	// A failed reload has its own category and must not leak relation or schema
	// names either.
	pingErr = nil
	source.mu.Lock()
	source.loadHook = func(context.Context) (providerstore.Rows, int64, error) {
		return providerstore.Rows{}, 0, errors.New(`failed to read relation "provider_keys": permission denied`)
	}
	source.mu.Unlock()
	if err := runtime.store.Reload(t.Context()); err == nil {
		t.Fatal("reload with a failing source returned no error")
	}
	health = runtime.health(t.Context())
	if health.Detail != "reload failed" {
		t.Fatalf("health detail = %q, want the fixed category \"reload failed\"", health.Detail)
	}
	if strings.Contains(health.Detail, "provider_keys") {
		t.Fatalf("health detail leaks a relation name: %q", health.Detail)
	}
	if !strings.Contains(logged(), "provider_keys") {
		t.Fatalf("operator log is missing the raw reload error: %q", logged())
	}

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(modelsAndHealthMiddleware(func() []gatewayModel { return nil }, nvidia.NewExecutor(&nvidia.Options{}), runtime))
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse /healthz body: %v", err)
	}
	detail, _ := body["detail"].(string)
	if detail == "" || detail != "reload failed" {
		t.Fatalf("/healthz detail = %q, want the fixed category \"reload failed\"", detail)
	}
}

// TestProviderRuntimeHealthProbeSharesOneDeadline proves the database probe
// gives both the reachability check and the version read one 2s budget, not two
// sequential 2s budgets that hold a pool connection each.
func TestProviderRuntimeHealthProbeSharesOneDeadline(t *testing.T) {
	source := &stubSnapshotSource{version: 5, rows: stubSnapshotRows()}
	var remaining time.Duration
	source.versionHook = func(ctx context.Context) (int64, error) {
		if deadline, ok := ctx.Deadline(); ok {
			remaining = time.Until(deadline)
		}
		return 5, nil
	}
	runtime, err := openProviderRuntime(t.Context(), source, providerRuntimeOptions{
		Ping: func(context.Context) error {
			time.Sleep(500 * time.Millisecond)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("open provider runtime: %v", err)
	}
	t.Cleanup(runtime.Close)

	runtime.health(t.Context())
	if remaining <= 0 {
		t.Fatal("version probe received no deadline")
	}
	if remaining > 1700*time.Millisecond {
		t.Fatalf("version probe deadline = %v remaining after a 500ms ping, want the shared budget (<1.7s)", remaining)
	}
}

// TestProviderRuntimeHealthProbeCollapsesConcurrentChecks proves a burst of
// unauthenticated /healthz requests shares one database probe instead of
// leasing a pool connection each.
func TestProviderRuntimeHealthProbeCollapsesConcurrentChecks(t *testing.T) {
	source := &stubSnapshotSource{version: 5, rows: stubSnapshotRows()}
	var pings int32
	release := make(chan struct{})
	runtime, err := openProviderRuntime(t.Context(), source, providerRuntimeOptions{
		Ping: func(context.Context) error {
			atomic.AddInt32(&pings, 1)
			<-release
			return nil
		},
	})
	if err != nil {
		t.Fatalf("open provider runtime: %v", err)
	}
	t.Cleanup(runtime.Close)

	const callers = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			runtime.health(context.Background())
		}()
	}
	close(start)

	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&pings) == 0 {
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("no health probe started")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&pings); got != 1 {
		t.Fatalf("concurrent health checks issued %d probes, want 1 shared probe", got)
	}
}

// TestProviderRuntimeHealthReportsWatcherError keeps "not refreshing" distinct
// from "database down": the watcher cannot connect, but the published snapshot
// and the database probe are fine.
func TestProviderRuntimeHealthReportsWatcherError(t *testing.T) {
	source := &stubSnapshotSource{version: 6, rows: stubSnapshotRows()}
	runtime, err := openProviderRuntime(t.Context(), source, providerRuntimeOptions{})
	if err != nil {
		t.Fatalf("open provider runtime: %v", err)
	}
	runtime.healthTTL = -1
	t.Cleanup(runtime.Close)

	runtime.noteWatcherError(errors.New("providerstore: listen connection: connection refused"))
	health := runtime.health(t.Context())
	if !health.DBOK {
		t.Fatal("db_ok = false while the database is reachable")
	}
	if health.WatcherOK {
		t.Fatal("watcher_ok = true after a watcher connect failure")
	}
	if health.Detail != "watcher not refreshing" {
		t.Fatalf("health detail = %q, want \"watcher not refreshing\"", health.Detail)
	}
	if !health.Degraded {
		t.Fatal("health is not degraded while the watcher cannot refresh")
	}

	runtime.noteWatcherError(nil)
	health = runtime.health(t.Context())
	if !health.WatcherOK || health.Degraded {
		t.Fatalf("watcher recovery: watcher_ok=%v degraded=%v", health.WatcherOK, health.Degraded)
	}
}

// TestProviderRuntimeHealthKeepsObservedConfigVersionDuringOutage proves
// config_version reports the last observed value and does not regress to the
// snapshot version when a probe fails.
func TestProviderRuntimeHealthKeepsObservedConfigVersionDuringOutage(t *testing.T) {
	source := &stubSnapshotSource{version: 8, rows: stubSnapshotRows()}
	var pingErr error
	runtime, err := openProviderRuntime(t.Context(), source, providerRuntimeOptions{
		Ping: func(context.Context) error { return pingErr },
	})
	if err != nil {
		t.Fatalf("open provider runtime: %v", err)
	}
	runtime.healthTTL = -1
	t.Cleanup(runtime.Close)

	// A committed write advances config_state.version before this instance
	// reloads; the probe observes it.
	source.setVersion(9)
	health := runtime.health(t.Context())
	if health.SnapshotVersion != 8 || health.ConfigVersion != 9 {
		t.Fatalf("versions = snapshot %d config %d, want 8/9", health.SnapshotVersion, health.ConfigVersion)
	}

	pingErr = errors.New("connection refused")
	health = runtime.health(t.Context())
	if health.ConfigVersion != 9 {
		t.Fatalf("config_version = %d during an outage, want the last observed 9", health.ConfigVersion)
	}
}

// TestProviderRuntimeLogsCredentialDiagnosticsAndAudits proves per-key decrypt
// failures are surfaced: one audit event and a log line naming the key and
// reason, never the credential material.
func TestProviderRuntimeLogsCredentialDiagnosticsAndAudits(t *testing.T) {
	rows := stubSnapshotRows()
	rows.Keys[0].AuthScheme = providerstore.SchemeBearer
	rows.Keys[0].SecretEnc = []byte("sealed-secret-material")
	rows.Keys[0].SecretNonce = make([]byte, providerstore.SecretNonceSize)
	rows.Keys[0].SecretKVer = 1
	rows.Keys[0].SecretLast4 = "l4st"
	source := &stubSnapshotSource{version: 2, rows: rows}

	logf, logged := captureLogf()
	var auditEvents []providerstore.AuditEvent
	runtime, err := openProviderRuntime(t.Context(), source, providerRuntimeOptions{
		Logf: logf,
		Audit: func(event providerstore.AuditEvent) {
			auditEvents = append(auditEvents, event)
		},
	})
	if err != nil {
		t.Fatalf("open provider runtime: %v", err)
	}
	t.Cleanup(runtime.Close)

	diagnostics := runtime.store.Current().Diagnostics()
	if len(diagnostics) != 1 || diagnostics[0].KeyID != "stub-key" {
		t.Fatalf("diagnostics = %+v, want one for stub-key", diagnostics)
	}
	if len(auditEvents) != 1 {
		t.Fatalf("audit events = %+v, want one", auditEvents)
	}
	if got := auditEvents[0].Code; got != providerstore.AuditDecryptFailed {
		t.Fatalf("audit code = %q, want %q", got, providerstore.AuditDecryptFailed)
	}
	if len(auditEvents[0].KeyIDs) != 1 || auditEvents[0].KeyIDs[0] != "stub-key" {
		t.Fatalf("audit key ids = %v, want [stub-key]", auditEvents[0].KeyIDs)
	}

	lines := logged()
	if !strings.Contains(lines, "stub-key") || !strings.Contains(lines, "cannot be decrypted") {
		t.Fatalf("diagnostic log = %q, want the key id and reason", lines)
	}
	for _, secret := range []string{"sealed-secret-material"} {
		if strings.Contains(lines, secret) {
			t.Fatalf("diagnostic log leaks credential material: %q", lines)
		}
	}
}

// TestProviderRuntimeHealthReportsStaleConfigNotOutage covers criterion (d):
// db_ok reports database reachability only. A reachable database whose version
// read fails means the snapshot is stale, not that the database is unreachable.
func TestProviderRuntimeHealthReportsStaleConfigNotOutage(t *testing.T) {
	source := &stubSnapshotSource{version: 5, rows: stubSnapshotRows()}
	failVersion := false
	source.versionHook = func(context.Context) (int64, error) {
		if failVersion {
			return 0, errors.New(`relation "config_state" does not exist`)
		}
		return 5, nil
	}
	runtime, err := openProviderRuntime(t.Context(), source, providerRuntimeOptions{
		Ping: func(context.Context) error { return nil },
	})
	if err != nil {
		t.Fatalf("open provider runtime: %v", err)
	}
	runtime.healthTTL = -1
	t.Cleanup(runtime.Close)

	failVersion = true
	health := runtime.health(t.Context())
	if !health.DBOK {
		t.Fatal("db_ok = false while the database is reliably reachable")
	}
	if !health.Degraded {
		t.Fatal("health is not degraded while the config version cannot be read")
	}
	if health.Detail != "reload failed" {
		t.Fatalf("health detail = %q, want \"reload failed\" for a stale-config signal", health.Detail)
	}
}

// TestProviderRuntimeCanceledWaiterReportsDegraded covers the canceled-waiter
// path: with no completed probe yet, a canceled caller must not receive the
// zero value that renders as ok=true/status="ok".
func TestProviderRuntimeCanceledWaiterReportsDegraded(t *testing.T) {
	source := &stubSnapshotSource{version: 5, rows: stubSnapshotRows()}
	started := make(chan struct{})
	release := make(chan struct{})
	probeDone := make(chan struct{})
	runtime, err := openProviderRuntime(t.Context(), source, providerRuntimeOptions{
		Ping: func(context.Context) error {
			close(started)
			<-release
			return nil
		},
	})
	if err != nil {
		t.Fatalf("open provider runtime: %v", err)
	}
	t.Cleanup(runtime.Close)

	go func() {
		defer close(probeDone)
		runtime.health(context.Background())
	}()
	<-started

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	health := runtime.health(ctx)

	close(release)
	<-probeDone

	if !health.Degraded {
		t.Fatalf("canceled waiter reported degraded=false (zero value): %+v", health)
	}
	if health.ConfigVersion != 5 {
		t.Fatalf("canceled waiter config_version = %d, want the last observed 5", health.ConfigVersion)
	}
}
