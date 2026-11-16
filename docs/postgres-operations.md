# PostgreSQL operations

How to provision, migrate, back up, restore and monitor the database-backed
gateway. The gateway enters database mode when `GNVIPI_DATABASE_URL` is set;
without it, configuration stays in the file-backed models file and none of the
identity or key tables exist.

Provider credentials in the database are encrypted with a master key that is
never stored in PostgreSQL. **Back up the master key separately.** A database
backup without that key cannot decrypt stored provider secrets.

Roles, bootstrap and key handling are in [auth.md](auth.md). Run flags and
`/healthz` fields are in [operations.md](operations.md).

## Roles and grants

In a server deployment the operator creates all roles before the first
migration. The runtime role must be named `gnvipi_app`: migrations 0001 to 0003
grant privileges only to that name.

| Role | DSN variable | Purpose | Privileges |
|---|---|---|---|
| `gnvipi_migrate` | `GNVIPI_MIGRATION_DATABASE_URL` | DDL and migrations | Owns the database and schema objects, or has `CREATE` on `public`. Required in production. |
| `gnvipi_app` | `GNVIPI_DATABASE_URL` | Runtime application | `SELECT, INSERT, UPDATE, DELETE` on identity and configuration tables; `SELECT` on `config_state`, `schema_migrations` and `nvidia_models`; `INSERT` only on `audit_log`. No DDL, no `audit_log` update or delete. |
| `gnvipi_maintenance` | `GNVIPI_MAINTENANCE_DATABASE_URL` | Retention and catalog writes | `SELECT, DELETE` on `audit_log`; `SELECT, INSERT, DELETE` on `nvidia_models`. Optional; without it audit pruning is skipped and production catalog rescans fail. |

`gnvipi_migrate` and `gnvipi_maintenance` are example names; only `gnvipi_app`
is fixed by the migrations.

### Provisioning SQL

Run as a PostgreSQL superuser once per cluster. Replace every placeholder with a
real value; do not reuse the development password.

```sql
CREATE ROLE gnvipi_migrate LOGIN PASSWORD '<migration-password>';
CREATE ROLE gnvipi_app LOGIN PASSWORD '<runtime-password>';
CREATE ROLE gnvipi_maintenance LOGIN PASSWORD '<maintenance-password>';
CREATE DATABASE gnvipi OWNER gnvipi_migrate;
```

Then run the migrations (below), which create the tables and grant the runtime
role. Finally grant the maintenance role its retention and catalog tables:

```sql
\c gnvipi
GRANT USAGE ON SCHEMA public TO gnvipi_maintenance;
GRANT SELECT, DELETE ON audit_log TO gnvipi_maintenance;
GRANT SELECT, INSERT, DELETE ON nvidia_models TO gnvipi_maintenance;
```

Migration 0002 rejects a runtime role that is a superuser, can create roles or
databases, bypasses row security, owns public objects, or inherits privileges
from another role. Keep `gnvipi_app` non-privileged and free of memberships.

Use `sslmode=verify-full` for every non-local host. The gateway refuses a
non-local DSN without it. `localhost`, `127.0.0.1` and `::1` are treated as
local.

### Pool settings

| Variable | Default | Meaning |
|---|---|---|
| `GNVIPI_DB_MAX_CONNS` | `10` | Maximum runtime connections; at least 1. |
| `GNVIPI_DB_MIN_CONNS` | `1` | Minimum kept connections; 0 to `GNVIPI_DB_MAX_CONNS`. |
| `GNVIPI_DB_CONNECT_TIMEOUT` | `5s` | Per-connection dial timeout. |

The maintenance pool uses one connection.

## Provisioning with Docker Compose

The repository `docker-compose.yml` starts PostgreSQL 17 and the gateway:

```bash
docker compose up -d --wait db
```

Defaults are development-only:

| Variable | Default |
|---|---|
| `GNVIPI_DB_PORT` | `5432` on `127.0.0.1` |
| `GNVIPI_DB_USER` | `gnvipi_dev` |
| `GNVIPI_DB_PASSWORD` | `gnvipi_dev_only` |
| `GNVIPI_DB_NAME` | `gnvipi` |

The compose PostgreSQL user is a superuser, so the compose DSN works for both
migrations and runtime in a local sandbox. Do not use these credentials
elsewhere. For any non-development deployment, create the three roles above and
set both DSNs explicitly.

If you override any `GNVIPI_DB_*` value, set the matching URI-encoded
`GNVIPI_DATABASE_URL` as well. Compose waits for the database health check but
never runs migrations or creates the first superuser; run those steps yourself.

The `serve` service in compose reads:

```yaml
GNVIPI_DATABASE_URL: postgres://...@db:5432/gnvipi?sslmode=disable
GNVIPI_MASTER_KEY: "${GNVIPI_MASTER_KEY:-}"
GNVIPI_DB_MAX_CONNS: "10"
```

`GNVIPI_MASTER_KEY` has no default. Database mode fails at startup when it is
missing, so set it before starting `serve`.

## Migrations

Migrations are embedded in the binary. Apply them with the CLI, not by starting
the server:

```powershell
.\bin\serve.exe migrate
```

Rules verified in the code:

- `serve migrate` accepts no arguments.
- It uses `GNVIPI_MIGRATION_DATABASE_URL` when set.
- Without that variable it falls back to `GNVIPI_DATABASE_URL`, but only when
  `GNVIPI_ENV` is not `production`.
- With `GNVIPI_ENV=production`, a separate migration DSN is required and
  `--auto-migrate` is refused.
- `--auto-migrate` is a server flag for development only; it logs a warning and
  applies pending migrations before opening the runtime pool.

At startup the gateway checks the schema version against the embedded latest
version. A database that is unreachable, unmigrated or half-migrated aborts
startup instead of serving from an unexpected schema. There is no `serve
migrate down` command; restore a backup to roll back.

Example production sequence:

```bash
export GNVIPI_ENV=production
export GNVIPI_MIGRATION_DATABASE_URL='postgres://gnvipi_migrate:<password>@db.example.com:5432/gnvipi?sslmode=verify-full'
export GNVIPI_DATABASE_URL='postgres://gnvipi_app:<password>@db.example.com:5432/gnvipi?sslmode=verify-full'
./serve migrate
```

## Logical backup

`pg_dump` takes a consistent snapshot and is safe while the gateway is running.
Use the custom format so `pg_restore` can restore selectively.

```bash
pg_dump --format=custom --no-owner --no-acl \
  --dbname='postgres://gnvipi_app:<password>@db.example.com:5432/gnvipi?sslmode=verify-full' \
  --file=gnvipi-$(date +%Y%m%dT%H%M%S).dump
```

With the local compose database:

```bash
docker compose exec -T db pg_dump -U gnvipi_dev -d gnvipi -Fc \
  > gnvipi-backup.dump
```

Notes:

- `--no-owner --no-acl` omits role grants from the dump; role setup is a
  provisioning step, not restore data. Drop these flags only if you intend the
  dump to recreate ownership and grants.
- A dump contains identity data, password and token hashes, audit rows and
  provider key ciphertext. It does **not** contain plaintext provider secrets.
  Still encrypt the backup at rest and restrict access.
- A dump does not contain the master key, WAL files or PostgreSQL configuration.
- Test restore into a disposable database, not over the live one.

## WAL archiving and PITR

Point-in-time recovery is an operator responsibility. The gateway does not
configure WAL archiving, prune WAL, or provide PITR commands.

On the PostgreSQL server set at minimum:

```conf
wal_level = replica
archive_mode = on
archive_command = 'test ! -f /archive/%f && cp %p /archive/%f'
```

Use an archive destination outside the data directory, and an
`archive_cleanup_command` (or your storage lifecycle policy) to bound how long
WAL is kept. Retention is whatever your archive keeps; there is no gateway-side
WAL limit. RPO is the archive interval, and an `archive_command` failure stalls
WAL removal until it is fixed.

Take a base backup:

```bash
pg_basebackup -D /backup/base-$(date +%Y%m%dT%H%M%S) \
  -h db.example.com -U gnvipi_migrate -Fp -X stream -P
```

Restore to a point in time on a separate host:

1. Stop the gateway.
2. Restore the base backup into an empty data directory.
3. Create `recovery.signal` and set:
   ```conf
   restore_command = 'cp /archive/%f %p'
   recovery_target_time = '2026-11-15 12:00:00+00'
   recovery_target_action = 'promote'
   ```
4. Start PostgreSQL and confirm it reaches the target time and promotes.
5. Point `GNVIPI_DATABASE_URL` at the restored instance, start the gateway with
   the same `GNVIPI_MASTER_KEY`, and check `/healthz`.

PITR restores everything, including rows the gateway's retention job would have
deleted. Treat a restored database as a copy of the old state.

## Restore and integrity check

### Logical restore

Restore into a new, empty database with a role that can create schema objects.
Use the migration or owner DSN. The runtime role cannot create tables, even
when the dump was taken with `--no-owner --no-acl`.

```bash
createdb -h db.example.com -U gnvipi_migrate gnvipi_restore
pg_restore --exit-on-error --single-transaction \
  --dbname='postgres://gnvipi_migrate:<password>@db.example.com:5432/gnvipi_restore?sslmode=verify-full' \
  gnvipi-backup.dump
```

`--single-transaction` makes a failed restore leave no partial schema. For a
large restore, omit it and rely on `--exit-on-error`.

### Integrity checks

Run these on the restored database before switching the gateway over:

```sql
-- Schema version is the embedded latest and not dirty.
SELECT version, dirty FROM schema_migrations;

-- Exactly one config row, with a positive version.
SELECT count(*) AS row_count, max(version) FROM config_state WHERE version > 0;

-- At least one active superuser remains.
SELECT count(*) FROM users WHERE role = 'superuser' AND status = 'active';

-- Stored secrets are complete: ciphertext, nonce and version always travel together.
SELECT count(*) FROM provider_keys
 WHERE secret_enc IS NOT NULL AND (secret_nonce IS NULL OR secret_kver IS NULL);

-- No plaintext secret column exists; these should be the only secret columns.
SELECT column_name FROM information_schema.columns
 WHERE table_name = 'provider_keys' AND column_name LIKE 'secret%'
 ORDER BY column_name;
```

Expected results: `dirty = false`; one `config_state` row with a version greater
than zero; at least one active superuser; zero incomplete secret rows. The secret
columns are `secret_enc`, `secret_nonce`, `secret_kver` and `secret_last4`;
there is no plaintext key column.

Optional checks:

```bash
pg_amcheck --dbname='postgres://gnvipi_migrate:<password>@db.example.com:5432/gnvipi_restore?sslmode=verify-full'
pg_verifybackup /backup/base-20261115T120000
```

`pg_amcheck` checks table and index structure. `pg_verifybackup` verifies a
plain-format physical base backup, not a logical dump or a tar backup.

### Restore drill checklist

- [ ] Backup file is readable and its size is plausible.
- [ ] Restore targets a disposable database or host.
- [ ] `schema_migrations` matches the binary's embedded version and `dirty` is
  false.
- [ ] Integrity queries above pass.
- [ ] Start the gateway against the restored database with the same master key.
- [ ] `/healthz` reports `degraded: false` and a fresh `snapshot_version`.
- [ ] A provider key with a stored secret resolves; if its requests return 503
  because the credential cannot be decrypted, the master key does not match.
- [ ] Sign-in works for a superuser, and one request succeeds end to end.
- [ ] Record elapsed time and any manual steps.

Sessions and API tokens restored from a backup remain valid until they expire or
are revoked. If the restore is part of incident recovery, reset the affected
passwords and revoke tokens (see [auth.md](auth.md)).

## Retention job

The gateway runs one retention pass at startup and then every 24 hours. A
transaction-scoped advisory lock makes the pass single-flight across gateway
processes; a process that loses the race skips its turn without an error.

| Data | Removed when | Configurable |
|---|---|---|
| Sessions | Expired or revoked | Session timeouts only |
| Login attempts | Older than 30 days | No |
| Audit rows | Older than the audit window, default 365 days | `GNVIPI_AUDIT_RETENTION_DAYS`, 1 to 36500 |

Audit pruning uses `GNVIPI_MAINTENANCE_DATABASE_URL`. If that DSN is absent,
unset or unusable at startup, audit pruning is skipped and logged; the rest of
retention still runs. The runtime role cannot delete audit rows, so it cannot
be used as a fallback.

Limits:

- No `stats.jsonl` files, provider configuration, users, keys or backups are
  removed. Stats retention is a separate, file-backed setting in
  [operations.md](operations.md).
- No backup or WAL retention. That belongs to the backup tooling.
- Failures are logged only. Retention never blocks serving admin requests.
- A pass interrupted after the session/login transaction commits leaves audit
  pruning for the next pass; the cutoff makes it idempotent.

## Monitoring

`GET /healthz` carries the database state when `GNVIPI_DATABASE_URL` is set:

| Field | Meaning |
|---|---|
| `ok` | `false` only when degraded. |
| `status` | `ok` or `degraded`. |
| `degraded` | Serving the last known good snapshot because the database is down, the watcher is not refreshing, or the last reload failed. |
| `db_ok` | The database answered this probe. |
| `watcher_ok` | The `LISTEN` invalidation watcher is connected. |
| `snapshot_version` | Version of the snapshot in service. |
| `config_version` | Last `config_state.version` observed; may lead the snapshot while a reload is pending. |
| `snapshot_age` | Seconds since the serving snapshot was published. |
| `detail` | `database unreachable`, `reload failed` or `watcher not refreshing`. Driver text stays in server logs. |

A degraded gateway keeps HTTP 200 because requests are still served from the
last validated snapshot. Alert on `degraded: true`, on `ok: false`, and on a
`snapshot_age` that keeps growing. The captcha `pool` object is unrelated to
database health.

Useful database checks:

```sql
SELECT count(*), state FROM pg_stat_activity GROUP BY state;
SELECT pg_database_size(current_database());
SELECT last_archived_time, last_failed_time, failed_count FROM pg_stat_archiver;
```

Watch for long-running transactions, connection saturation against
`GNVIPI_DB_MAX_CONNS`, and a growing `last_failed_time` in `pg_stat_archiver`.
The gateway logs connection, schema, snapshot and retention failures; it never
returns the DSN or driver error text through `/healthz`.

## Development workflow

```powershell
# Start PostgreSQL only.
docker compose up -d --wait db

# Development DSN and master key.
$env:GNVIPI_DATABASE_URL='postgres://gnvipi_dev:gnvipi_dev_only@localhost:5432/gnvipi?sslmode=disable'
$env:GNVIPI_MASTER_KEY='<base64-32-byte-key>'

# Build, migrate, bootstrap a superuser, run.
go build -o .\bin\serve.exe ./cmd/serve
.\bin\serve.exe migrate
'<temporary-password>' | .\bin\serve.exe admin create-superuser --username <superuser> --with-gateway-token
$env:GNVIPI_GATEWAY_TOKEN='gw_...'
.\scripts\run.ps1 -Claude
```

Copy the token printed by `create-superuser` before running `run.ps1`.

The compose volume survives `docker compose down`. Use `docker compose down -v`
only when you intend to delete the development database; there is no undo.

For schema tests, point the test suite at a disposable database:

```bash
export GNVIPI_TEST_DATABASE_URL='postgres://gnvipi_dev:gnvipi_dev_only@localhost:5432/gnvipi_test?sslmode=disable'
export GNVIPI_REQUIRE_DB_TESTS=true
go test ./internal/db/...
```

`GNVIPI_REQUIRE_DB_TESTS=true` turns a missing test DSN into a failure instead
of a skip. Never point these tests at a database you care about.