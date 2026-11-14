package providerstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minhhhduc/gnvipi/internal/db"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
)

// These tests assert the cross-instance invalidation contract of spec §8.2
// against a real PostgreSQL: the snapshot carries the whole configuration,
// nothing is published before the writing transaction commits, committed
// writes reach every instance through LISTEN/NOTIFY, and a lost notification
// is repaired by the config_state.version poll.
//
// They are skipped unless GNVIPI_TEST_DATABASE_URL points at a disposable
// database; GNVIPI_REQUIRE_DB_TESTS=true turns a missing database into a
// failure.

var (
	integrationSchemaOnce sync.Once
	integrationSchemaErr  error
)

func integrationDSN(t *testing.T) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("GNVIPI_TEST_DATABASE_URL"))
	if dsn == "" {
		if os.Getenv("GNVIPI_REQUIRE_DB_TESTS") == "true" {
			t.Fatal("GNVIPI_TEST_DATABASE_URL is required when GNVIPI_REQUIRE_DB_TESTS=true")
		}
		t.Skip("GNVIPI_TEST_DATABASE_URL is not set")
	}
	return dsn
}

func integrationEnsureSchema(t *testing.T, dsn string) {
	t.Helper()
	integrationSchemaOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		integrationSchemaErr = db.Migrate(ctx, dsn)
	})
	if integrationSchemaErr != nil {
		t.Fatalf("migrate integration database: %v", integrationSchemaErr)
	}
}

func integrationPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect integration database: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping integration database: %v", err)
	}
	return pool
}

func integrationConfigVersion(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()
	var version int64
	if err := pool.QueryRow(ctx, `SELECT version FROM config_state WHERE id`).Scan(&version); err != nil {
		t.Fatalf("read config_state.version: %v", err)
	}
	return version
}

func integrationUUID(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&id); err != nil {
		t.Fatalf("generate uuid: %v", err)
	}
	return id
}

func integrationNewUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool, username string) string {
	t.Helper()
	var id string
	err := pool.QueryRow(ctx,
		`INSERT INTO users(username, role, password_hash) VALUES($1, 'user', 'x') RETURNING id::text`,
		username).Scan(&id)
	if err != nil {
		t.Fatalf("insert user %s: %v", username, err)
	}
	return id
}

func integrationNewProvider(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id string) {
	t.Helper()
	_, err := pool.Exec(ctx,
		`INSERT INTO providers(id, name, protocol, base_url) VALUES($1, $1, 'openai-chat', $2)`,
		id, "https://"+id+".example.com/v1")
	if err != nil {
		t.Fatalf("insert provider %s: %v", id, err)
	}
}

// integrationCleanup removes the rows a test created. Deleting the provider
// cascades to its keys and models, and deleting the user removes personal
// keys; both are best-effort because the database is disposable.
func integrationCleanup(t *testing.T, pool *pgxpool.Pool, providerID string, userIDs, modelIDs []string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := pool.Exec(ctx, `DELETE FROM providers WHERE id = $1`, providerID); err != nil {
			t.Logf("cleanup provider %s: %v", providerID, err)
		}
		for _, id := range userIDs {
			if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, id); err != nil {
				t.Logf("cleanup user %s: %v", id, err)
			}
		}
		for _, id := range modelIDs {
			if _, err := pool.Exec(ctx, `DELETE FROM model_state WHERE model_id = $1`, id); err != nil {
				t.Logf("cleanup model_state %s: %v", id, err)
			}
		}
	})
}

func integrationUniqueSuffix() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

// integrationListenerCount reports how many sessions in this database are
// currently parked in LISTEN gnvipi_config. pg_stat_activity keeps the last
// statement text, which is how the watcher's dedicated connection is
// identified.
func integrationListenerCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	var count int
	err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_stat_activity
		  WHERE datname = current_database()
		    AND query = 'LISTEN gnvipi_config'`).Scan(&count)
	if err != nil {
		t.Fatalf("count listeners: %v", err)
	}
	return count
}

func integrationWaitForListeners(t *testing.T, ctx context.Context, pool *pgxpool.Pool, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if got := integrationListenerCount(t, ctx, pool); got >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d LISTEN gnvipi_config sessions (have %d)",
				want, integrationListenerCount(t, ctx, pool))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// integrationStartWatcher runs a watcher for the lifetime of the test; the
// goroutine has exited before the cleanup registered here returns.
func integrationStartWatcher(t *testing.T, ctx context.Context, store *Store, pool *pgxpool.Pool, poll time.Duration) {
	t.Helper()
	watchCtx, cancel := context.WithCancel(ctx)
	watcher := &Watcher{
		Store:          store,
		Pool:           pool,
		PollInterval:   poll,
		InitialBackoff: 20 * time.Millisecond,
		MaxBackoff:     100 * time.Millisecond,
		OnError: func(err error) {
			t.Logf("watcher: %v", err)
		},
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := watcher.Run(watchCtx); err != nil && watchCtx.Err() == nil {
			t.Logf("watcher stopped: %v", err)
		}
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			wg.Wait()
		})
	}
	t.Cleanup(stop)
}

func integrationWaitForVersion(t *testing.T, ctx context.Context, store *Store, want int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if got := store.Version(); got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("snapshot version = %d after 10s, want %d (last error: %v)",
				store.Version(), want, store.LastError())
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for snapshot version %d: %v", want, ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// TestSnapshotFromDatabaseCarriesProvidersKeysAndAliasCandidates proves the
// runtime snapshot is complete: providers, personal user keys, shared none
// keys and per-user alias candidates all come from the committed rows.
func TestSnapshotFromDatabaseCarriesProvidersKeysAndAliasCandidates(t *testing.T) {
	dsn := integrationDSN(t)
	integrationEnsureSchema(t, dsn)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool := integrationPool(t, dsn)
	ring := testKeyring(t)

	suffix := integrationUniqueSuffix()
	providerID := "p201content" + suffix
	personalModel := "personal-model-" + suffix
	sharedModel := "shared-model-" + suffix
	hiddenModel := "hidden-model-" + suffix
	owner := integrationNewUser(t, ctx, pool, "owner201"+suffix)
	other := integrationNewUser(t, ctx, pool, "other201"+suffix)
	integrationNewProvider(t, ctx, pool, providerID)
	integrationCleanup(t, pool, providerID, []string{owner, other}, []string{hiddenModel})

	personalKeyID := integrationUUID(t, ctx, pool)
	secret := []byte("sk-personal-201")
	ciphertext, nonce, keyVersion, err := ring.Encrypt(secret, personalKeyID, owner, string(SchemeBearer))
	if err != nil {
		t.Fatalf("seal personal key: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO provider_keys(id, provider_id, owner_id, name, auth_scheme,
		     secret_enc, secret_nonce, secret_kver, secret_last4)
		 VALUES($1, $2, $3, 'personal', 'bearer', $4, $5, $6, $7)`,
		personalKeyID, providerID, owner, ciphertext, nonce, keyVersion,
		SecretLast4(secret, SchemeBearer)); err != nil {
		t.Fatalf("insert personal key: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO key_models(key_id, alias, upstream_model) VALUES($1, $2, 'upstream-personal')`,
		personalKeyID, personalModel); err != nil {
		t.Fatalf("insert personal alias: %v", err)
	}

	var sharedKeyID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO provider_keys(provider_id, name, auth_scheme)
		 VALUES($1, 'shared', 'none') RETURNING id::text`, providerID).Scan(&sharedKeyID); err != nil {
		t.Fatalf("insert shared key: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO key_models(key_id, alias, upstream_model) VALUES($1, $2, 'upstream-shared')`,
		sharedKeyID, sharedModel); err != nil {
		t.Fatalf("insert shared alias: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO model_state(model_id, hidden) VALUES($1, true)`, hiddenModel); err != nil {
		t.Fatalf("insert hidden model: %v", err)
	}

	store := NewStore(NewDBRowSource(pool), StoreOptions{Keyring: ring, Now: time.Now})
	if err := store.Reload(ctx); err != nil {
		t.Fatalf("reload snapshot: %v", err)
	}
	if got, want := store.Version(), integrationConfigVersion(t, ctx, pool); got != want {
		t.Fatalf("snapshot version = %d, want config_state.version %d", got, want)
	}

	definition, ok := store.Current().Provider(providerID)
	if !ok {
		t.Fatalf("snapshot is missing provider %s", providerID)
	}
	if want := "https://" + providerID + ".example.com/v1"; definition.BaseURL != want {
		t.Fatalf("provider base url = %q, want %q", definition.BaseURL, want)
	}

	router := NewRouter(store)
	route, err := router.Resolve(ctx, owner, personalModel, ir.ProtocolOpenAIChat)
	if err != nil {
		t.Fatalf("resolve personal alias as owner: %v", err)
	}
	if route.KeyID != personalKeyID || route.ProviderID != providerID || route.OwnerID != owner {
		t.Fatalf("personal route = key %q provider %q owner %q, want key %q provider %q owner %q",
			route.KeyID, route.ProviderID, route.OwnerID, personalKeyID, providerID, owner)
	}
	if route.UpstreamModel != "upstream-personal" {
		t.Fatalf("personal upstream model = %q, want upstream-personal", route.UpstreamModel)
	}

	if _, err := router.Resolve(ctx, other, personalModel, ir.ProtocolOpenAIChat); !errors.Is(err, ErrKeyRequired) {
		t.Fatalf("resolve another user's personal alias = %v, want ErrKeyRequired", err)
	}

	route, err = router.Resolve(ctx, other, sharedModel, ir.ProtocolOpenAIChat)
	if err != nil {
		t.Fatalf("resolve shared alias: %v", err)
	}
	if route.KeyID != sharedKeyID || route.ProviderID != providerID || route.OwnerID != "" {
		t.Fatalf("shared route = key %q provider %q owner %q, want key %q provider %q owner \"\"",
			route.KeyID, route.ProviderID, route.OwnerID, sharedKeyID, providerID)
	}

	if _, err := router.Resolve(ctx, owner, hiddenModel, ir.ProtocolOpenAIChat); !errors.Is(err, ErrModelHidden) {
		t.Fatalf("resolve hidden model = %v, want ErrModelHidden", err)
	}
	if _, err := router.Resolve(ctx, owner, "missing-model-"+suffix, ir.ProtocolOpenAIChat); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("resolve unknown model = %v, want ErrModelNotFound", err)
	}
}

// TestTwoInstancesConvergeAfterCommittedWrite runs two independent instances
// (separate pools, stores and watchers) against one database. Nothing may be
// published before COMMIT, and after COMMIT the notification must reach both
// instances: the poll interval is an hour, so only LISTEN/NOTIFY can converge
// them inside the test window.
func TestTwoInstancesConvergeAfterCommittedWrite(t *testing.T) {
	dsn := integrationDSN(t)
	integrationEnsureSchema(t, dsn)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	poolA := integrationPool(t, dsn)
	poolB := integrationPool(t, dsn)
	writer := integrationPool(t, dsn)
	ring := testKeyring(t)

	suffix := integrationUniqueSuffix()
	providerID := "p201notify" + suffix
	alias := "notify-model-" + suffix
	owner := integrationNewUser(t, ctx, writer, "notify201"+suffix)
	integrationNewProvider(t, ctx, writer, providerID)
	integrationCleanup(t, writer, providerID, []string{owner}, nil)

	storeA := NewStore(NewDBRowSource(poolA), StoreOptions{Keyring: ring, Now: time.Now})
	storeB := NewStore(NewDBRowSource(poolB), StoreOptions{Keyring: ring, Now: time.Now})
	for _, store := range []*Store{storeA, storeB} {
		if err := store.Reload(ctx); err != nil {
			t.Fatalf("initial reload: %v", err)
		}
	}
	baseline := integrationConfigVersion(t, ctx, writer)
	if storeA.Version() != baseline || storeB.Version() != baseline {
		t.Fatalf("initial versions = %d/%d, want %d", storeA.Version(), storeB.Version(), baseline)
	}

	integrationStartWatcher(t, ctx, storeA, poolA, time.Hour)
	integrationStartWatcher(t, ctx, storeB, poolB, time.Hour)
	integrationWaitForListeners(t, ctx, writer, 2)

	// An uncommitted write must never be published, and must never be visible
	// to a reader on another connection.
	tx, err := writer.Begin(ctx)
	if err != nil {
		t.Fatalf("begin write transaction: %v", err)
	}
	keyID := integrationUUID(t, ctx, writer)
	insertKeyAndAlias(t, ctx, tx, providerID, owner, keyID, alias, "upstream-notify")
	time.Sleep(300 * time.Millisecond)
	if storeA.Version() != baseline || storeB.Version() != baseline {
		t.Fatalf("uncommitted write was published: versions = %d/%d, want %d",
			storeA.Version(), storeB.Version(), baseline)
	}
	if _, err := NewRouter(storeA).Resolve(ctx, owner, alias, ir.ProtocolOpenAIChat); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("uncommitted alias resolved = %v, want ErrModelNotFound", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback write transaction: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	if storeA.Version() != baseline || storeB.Version() != baseline {
		t.Fatalf("rolled-back write was published: versions = %d/%d, want %d",
			storeA.Version(), storeB.Version(), baseline)
	}

	// A committed write bumps config_state.version and notifies both instances.
	committedKeyID := integrationUUID(t, ctx, writer)
	commitKeyAndAlias(t, ctx, writer, providerID, owner, committedKeyID, alias, "upstream-notify")
	committedVersion := integrationConfigVersion(t, ctx, writer)
	if committedVersion <= baseline {
		t.Fatalf("config_state.version = %d after a committed write, want > %d", committedVersion, baseline)
	}

	integrationWaitForVersion(t, ctx, storeA, committedVersion)
	integrationWaitForVersion(t, ctx, storeB, committedVersion)

	for name, store := range map[string]*Store{"A": storeA, "B": storeB} {
		route, err := NewRouter(store).Resolve(ctx, owner, alias, ir.ProtocolOpenAIChat)
		if err != nil {
			t.Fatalf("instance %s resolve committed alias: %v", name, err)
		}
		if route.KeyID != committedKeyID {
			t.Fatalf("instance %s key id = %q, want %q", name, route.KeyID, committedKeyID)
		}
	}
}

// TestInstanceConvergesByPollWhenNotificationIsLost proves the version poll is
// a real fallback: config_state.version advances without any notification, and
// the instance still publishes the new version.
func TestInstanceConvergesByPollWhenNotificationIsLost(t *testing.T) {
	dsn := integrationDSN(t)
	integrationEnsureSchema(t, dsn)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := integrationPool(t, dsn)
	writer := integrationPool(t, dsn)
	store := NewStore(NewDBRowSource(pool), StoreOptions{Keyring: testKeyring(t), Now: time.Now})
	if err := store.Reload(ctx); err != nil {
		t.Fatalf("initial reload: %v", err)
	}
	baseline := store.Version()

	integrationStartWatcher(t, ctx, store, pool, 100*time.Millisecond)
	integrationWaitForListeners(t, ctx, writer, 1)

	// config_state has no notification trigger: this bump is only observable
	// through the poll.
	if _, err := writer.Exec(ctx, `UPDATE config_state SET version = version + 1 WHERE id`); err != nil {
		t.Fatalf("bump config_state.version: %v", err)
	}
	bumped := integrationConfigVersion(t, ctx, writer)
	if bumped != baseline+1 {
		t.Fatalf("config_state.version = %d, want %d", bumped, baseline+1)
	}
	integrationWaitForVersion(t, ctx, store, bumped)
}

// integrationExecutor is satisfied by *pgxpool.Pool and pgx.Tx.
type integrationExecutor interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// insertKeyAndAlias adds one personal none key and its alias on the caller's
// connection or transaction, so the caller controls commit visibility.
func insertKeyAndAlias(t *testing.T, ctx context.Context, ex integrationExecutor, providerID, owner, keyID, alias, upstream string) {
	t.Helper()
	if _, err := ex.Exec(ctx,
		`INSERT INTO provider_keys(id, provider_id, owner_id, name, auth_scheme)
		 VALUES($1, $2, $3, $4, 'none')`,
		keyID, providerID, owner, "key-"+keyID[len(keyID)-8:]); err != nil {
		t.Fatalf("insert personal key: %v", err)
	}
	if _, err := ex.Exec(ctx,
		`INSERT INTO key_models(key_id, alias, upstream_model) VALUES($1, $2, $3)`,
		keyID, alias, upstream); err != nil {
		t.Fatalf("insert alias %s: %v", alias, err)
	}
}

// commitKeyAndAlias writes the key and alias in one committed transaction.
func commitKeyAndAlias(t *testing.T, ctx context.Context, pool *pgxpool.Pool, providerID, owner, keyID, alias, upstream string) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin commit transaction: %v", err)
	}
	insertKeyAndAlias(t, ctx, tx, providerID, owner, keyID, alias, upstream)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit key and alias: %v", err)
	}
}
