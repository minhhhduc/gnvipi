# DB-backed provider, key and model rework

Status: Audit-revised design (2026-11-12). Normative for implementation.
Date: 2026-11-12
Related issues: #185–#212, #211
Supersedes: file-backed `providers.json`, `secrets.json` and the single shared admin token

Revision note: this version resolves the design audit findings A–I. The
important changes are: exactly one credential source per enabled key; disabled
keys may have no credential; key/model scope is immutable and enforced with a
composite foreign key; every configuration write bumps the version through
statement triggers; snapshots load under `REPEATABLE READ`; JSONB shapes are
constrained; sensitive provider configuration is separated from credentials;
routing and fallback semantics are explicit; audit has explicit subject
columns; `must_change_password` and the last-owner invariant are enforced;
and the existing `docker-compose.yml` is extended rather than duplicated.

## 1. Context, goals and non-goals

The gateway currently reads providers, model mappings and credentials from a
versioned JSON file and protects `/admin` with one shared token. That cannot
support personal API keys or per-user routing.

Goals:

- Move providers, provider keys, model mappings, users and settings into
  PostgreSQL.
- Make API keys personal: a key belongs to one user and is used only for that
  user's requests.
- Keep one shared credential path: providers whose auth scheme is `none`.
- Provide real authentication (owner/admin/user), sessions, personal gateway
  tokens and an audit trail.
- Keep the existing v2 admin API shape working through `LegacyView`, with an
  exact list of removed fields.
- Keep the three-file admin embed contract: `web/admin/index.html`,
  `web/admin/app.css`, `web/admin/app.js`.

Non-goals, documented as not implemented:

- Key failover or rotation between credentials.
- Per-user quotas and cost limits.
- Credit, RPM or TPM metrics in the UI.
- End-to-end encryption. Provider secrets are encrypted at rest, but the
  server holds the master key.
- Multi-tenant organisations, SSO, OAuth or external identity providers.
- High-availability orchestration. Multiple gateway instances are supported
  through the shared DB and snapshot invalidation, but stats remain per-process
  (see §8.4).

Success criteria:

- A fresh install starts with no provider JSON file.
- Two users can use the same provider with different keys and can never route
  through each other's key.
- No secret is returned by an API, written to a log, stored in stats or written
  to audit details.
- The existing admin SPA still builds and serves through the same three
  embedded files.
- Every configuration write is visible to every instance without a restart.
- No sequence of user, role or key operations can leave zero active owners or
  a cross-user routing candidate.

## 2. Preconditions and compatibility

- **PR #211** must be merged before the admin UI work in #188. PR #184 is marked
  merged, but `main` was reset to `777f99a`; the UI rebuild is preserved in
  #211. Treat #211 as the gate.
- During the transition `/admin/login` continues to work. The legacy shared-
  token login is disabled only after the new owner bootstrap succeeds.
- The v2-shaped `GET/PUT /admin/api/providers/{id}` remains available through
  `LegacyView` for owner/admin clients. Exact mapping is in Appendix A.
- The embedded admin artifacts remain exactly three files. Any UI PR that
  changes them must rebuild and commit all three.
- Existing v2 providers using `auth.scheme = "x-api-key"` map to the internal
  `header` scheme with `auth_header_name = "x-api-key"`. No existing provider
  loses its credential or header behavior.

## 3. Architecture

Components:

- PostgreSQL: authoritative store for users, sessions, tokens, providers,
  keys, models, settings, audit and the configuration version.
- pgx v5 pool: one pool per process, bounded by `GNVIPI_DB_MAX_CONNS`
  (default 10).
- Auth core: password hashing, sessions, token validation, lockout, audit
  writer.
- Admin API: permission and ownership middleware in front of every
  `/admin/api/*` route.
- Routing snapshot: in-memory, per-user index rebuilt from the DB after each
  committed configuration write.
- Admin SPA: role-aware screens served from the three embedded files.
- CLI: `serve migrate`, `serve admin create-owner`,
  `serve admin reset-password`, `serve admin import-v2`,
  `serve admin rotate-master-key` (phase 2).

Request paths:

- Browser: `gw_session` cookie -> session lookup -> user -> permission
  middleware.
- Admin API client: `Authorization: Bearer adm_...` -> token lookup -> user ->
  scope check.
- Gateway client: `Authorization: Bearer gw_...` -> token lookup -> user ->
  per-user routing.

Startup:

1. Load env; refuse a missing `GNVIPI_DATABASE_URL` in production.
2. Connect the pgx pool and run the schema guard.
3. Load the configuration snapshot.
4. Start HTTP.
5. If the schema is newer than the binary, refuse to start.
6. If the DB is unreachable on a cold start, refuse to start. A running
   instance keeps its last good snapshot and reports degraded health.

## 4. Data model

### 4.1 Conventions

- IDs: `uuid` with `gen_random_uuid()` defaults; providers keep a stable text
  slug.
- Times: `timestamptz`.
- JSON: `jsonb`.
- Binary secrets and hashes: `bytea`.
- Case-insensitive usernames and emails: `citext`.
- Enums: `text` plus `CHECK` constraints, so adding a value is a deliberate
  migration.
- Foreign keys use `ON DELETE CASCADE` where the child cannot outlive the
  parent; `created_by` and `updated_by` use `ON DELETE SET NULL`.
- All mutable tables have `updated_at` maintained by a trigger.
- Audit identifiers are deliberately not foreign keys: an audit row must
  survive deletion of the user, key or provider it names.

### 4.2 Extensions and helper functions

```sql
CREATE EXTENSION IF NOT EXISTS citext;

CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  NEW.updated_at = now();
  RETURN NEW;
END $$;
```

### 4.3 Identity tables

```sql
CREATE TABLE users (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  username citext NOT NULL UNIQUE
    CHECK (length(username) BETWEEN 3 AND 64),
  email citext UNIQUE,
  display_name text NOT NULL DEFAULT ''
    CHECK (length(display_name) <= 200),
  role text NOT NULL CHECK (role IN ('owner', 'admin', 'user')),
  status text NOT NULL DEFAULT 'active'
    CHECK (status IN ('active', 'disabled')),
  password_hash text NOT NULL CHECK (length(password_hash) > 0),
  password_updated_at timestamptz NOT NULL DEFAULT now(),
  must_change_password boolean NOT NULL DEFAULT false,
  failed_attempts integer NOT NULL DEFAULT 0 CHECK (failed_attempts >= 0),
  locked_until timestamptz,
  last_login_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  created_by uuid REFERENCES users(id) ON DELETE SET NULL
);

CREATE TABLE sessions (
  token_hash bytea PRIMARY KEY CHECK (octet_length(token_hash) = 32),
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at timestamptz NOT NULL DEFAULT now(),
  last_seen_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL,
  revoked_at timestamptz,
  ip inet,
  user_agent text NOT NULL DEFAULT '' CHECK (length(user_agent) <= 512),
  CHECK (expires_at > created_at)
);
CREATE INDEX sessions_user_id_idx ON sessions(user_id);
CREATE INDEX sessions_expires_at_idx ON sessions(expires_at);
CREATE INDEX sessions_last_seen_idx ON sessions(last_seen_at);

CREATE TABLE api_tokens (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  kind text NOT NULL CHECK (kind IN ('gateway', 'admin')),
  name text NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
  token_prefix text NOT NULL CHECK (length(token_prefix) BETWEEN 4 AND 16),
  token_hash bytea NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
  scope text CHECK (scope IN ('read', 'write')),
  expires_at timestamptz,
  last_used_at timestamptz,
  revoked_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  CHECK (
    (kind = 'admin' AND scope IS NOT NULL) OR
    (kind = 'gateway' AND scope IS NULL)
  ),
  CHECK (expires_at IS NULL OR expires_at > created_at)
);
CREATE INDEX api_tokens_user_id_idx ON api_tokens(user_id);
CREATE INDEX api_tokens_expires_at_idx ON api_tokens(expires_at);

CREATE TABLE login_attempts (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  ts timestamptz NOT NULL DEFAULT now(),
  username citext NOT NULL,
  ip inet,
  success boolean NOT NULL
);
CREATE INDEX login_attempts_username_ts_idx
  ON login_attempts(username, ts DESC);
CREATE INDEX login_attempts_ip_ts_idx
  ON login_attempts(ip, ts DESC) WHERE ip IS NOT NULL;

CREATE TABLE audit_log (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  ts timestamptz NOT NULL DEFAULT now(),
  actor_type text NOT NULL CHECK (actor_type IN ('user', 'token', 'system')),
  actor_id uuid,
  -- Denormalised subjects, intentionally not foreign keys: they must survive
  -- deletion of the subject and are never used for authorization joins.
  user_id uuid,
  key_id uuid,
  provider_id text,
  request_id uuid,
  action text NOT NULL CHECK (length(action) BETWEEN 1 AND 80),
  target_type text NOT NULL CHECK (length(target_type) BETWEEN 1 AND 40),
  target_id text,
  ip inet,
  details jsonb NOT NULL DEFAULT '{}'::jsonb
    CHECK (jsonb_typeof(details) = 'object'),
  CHECK (length(details::text) <= 8192)
);
CREATE INDEX audit_log_ts_id_idx ON audit_log(ts DESC, id DESC);
CREATE INDEX audit_log_actor_idx ON audit_log(actor_type, actor_id, ts DESC);
CREATE INDEX audit_log_user_idx ON audit_log(user_id, ts DESC);
CREATE INDEX audit_log_key_idx ON audit_log(key_id, ts DESC);
CREATE INDEX audit_log_provider_idx ON audit_log(provider_id, ts DESC);
```

`audit_log` is append-only for the application role: it receives `INSERT` and
`SELECT` only. Retention and deletion run under a separate maintenance role
(phase 2). `user_id`, `key_id`, `provider_id` and `request_id` are denormalised
identifiers; `target_type`/`target_id` remain the canonical target.

### 4.4 Configuration tables

```sql
CREATE TABLE providers (
  id text PRIMARY KEY
    CHECK (id ~ '^[a-z0-9][a-z0-9._-]{0,63}$'),
  name text NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
  protocol text NOT NULL CHECK (length(protocol) BETWEEN 1 AND 64),
  base_url text NOT NULL
    CHECK (length(base_url) BETWEEN 1 AND 2048 AND base_url ~ '^https?://'),
  proxy_url text NOT NULL DEFAULT ''
    CHECK (length(proxy_url) <= 2048
           AND (proxy_url = '' OR proxy_url ~ '^(https?|socks5h?)://')),
  enabled boolean NOT NULL DEFAULT true,
  tls text NOT NULL DEFAULT 'default'
    CHECK (tls IN ('default', 'chrome')),
  endpoint_paths jsonb NOT NULL DEFAULT '{}'::jsonb
    CHECK (jsonb_typeof(endpoint_paths) = 'object'
           AND length(endpoint_paths::text) <= 8192),
  forward_headers jsonb NOT NULL DEFAULT '[]'::jsonb
    CHECK (jsonb_typeof(forward_headers) = 'array'
           AND jsonb_array_length(forward_headers) <= 64),
  headers jsonb NOT NULL DEFAULT '{}'::jsonb
    CHECK (jsonb_typeof(headers) = 'object'
           AND length(headers::text) <= 16384),
  extra_body jsonb NOT NULL DEFAULT '{}'::jsonb
    CHECK (jsonb_typeof(extra_body) = 'object'
           AND length(extra_body::text) <= 16384
           AND NOT (extra_body ? 'model')
           AND NOT (extra_body ? 'stream')),
  extensions jsonb NOT NULL DEFAULT '{}'::jsonb
    CHECK (jsonb_typeof(extensions) = 'object'
           AND length(extensions::text) <= 65536),
  max_concurrency integer NOT NULL DEFAULT 0 CHECK (max_concurrency >= 0),
  queue_timeout bigint NOT NULL DEFAULT 0 CHECK (queue_timeout >= 0),
  timeouts jsonb NOT NULL DEFAULT '{}'::jsonb
    CHECK (jsonb_typeof(timeouts) = 'object'
           AND length(timeouts::text) <= 2048),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE provider_keys (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  provider_id text NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
  owner_id uuid REFERENCES users(id) ON DELETE CASCADE,
  name text NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
  enabled boolean NOT NULL DEFAULT true,
  auth_scheme text NOT NULL
    CHECK (auth_scheme IN ('none', 'bearer', 'header', 'basic')),
  auth_header_name text,
  key_env text,
  key_ref text,
  secret_enc bytea,
  secret_nonce bytea,
  secret_kver integer,
  secret_last4 text,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  -- Computed, non-null scope used as a composite FK target. The zero UUID is
  -- a sentinel; real user IDs are random UUIDs and cannot equal it.
  scope_owner uuid GENERATED ALWAYS AS (
    COALESCE(owner_id, '00000000-0000-0000-0000-000000000000'::uuid)
  ) STORED,
  CHECK (owner_id IS NOT NULL OR auth_scheme = 'none'),
  CHECK ((secret_enc IS NULL) = (secret_nonce IS NULL)),
  CHECK ((secret_enc IS NULL) = (secret_kver IS NULL)),
  CHECK (secret_enc IS NULL OR octet_length(secret_nonce) = 12),
  CHECK (secret_kver IS NULL OR secret_kver > 0),
  CHECK ((secret_enc IS NULL) = (secret_last4 IS NULL)),
  CHECK (secret_last4 IS NULL OR length(secret_last4) <= 8),
  CHECK (key_env IS NULL
         OR key_env ~ '^[A-Za-z_][A-Za-z0-9_]{0,127}$'),
  CHECK (key_ref IS NULL
         OR (length(key_ref) BETWEEN 1 AND 512
             AND key_ref !~ '[[:cntrl:]]')),
  CHECK (auth_scheme <> 'header'
         OR (auth_header_name IS NOT NULL
             AND auth_header_name ~ '^[!#$%&''*+\-.^_`|~0-9A-Za-z]+$')),
  CHECK (auth_scheme = 'header' OR auth_header_name IS NULL),
  -- Exactly one credential source for an enabled non-none key. A disabled key
  -- may have zero sources, which is what makes clear_secret + enabled=false
  -- a legal state.
  CHECK (num_nonnulls(secret_enc, key_env, key_ref) <= 1),
  CHECK (auth_scheme = 'none'
         OR NOT enabled
         OR num_nonnulls(secret_enc, key_env, key_ref) = 1),
  CHECK (auth_scheme <> 'none'
         OR (secret_enc IS NULL AND secret_nonce IS NULL
             AND secret_kver IS NULL AND secret_last4 IS NULL
             AND key_env IS NULL AND key_ref IS NULL
             AND auth_header_name IS NULL)),
  UNIQUE (id, provider_id, scope_owner)
);
CREATE UNIQUE INDEX provider_keys_owner_name_uq
  ON provider_keys(provider_id, owner_id, name) WHERE owner_id IS NOT NULL;
CREATE UNIQUE INDEX provider_keys_shared_name_uq
  ON provider_keys(provider_id, name) WHERE owner_id IS NULL;
CREATE INDEX provider_keys_owner_enabled_idx
  ON provider_keys(owner_id) WHERE enabled AND owner_id IS NOT NULL;

CREATE TABLE key_models (
  key_id uuid NOT NULL,
  provider_id text NOT NULL,
  owner_id uuid,
  alias text NOT NULL CHECK (length(alias) BETWEEN 1 AND 200),
  upstream_model text NOT NULL DEFAULT ''
    CHECK (length(upstream_model) <= 200),
  hidden boolean NOT NULL DEFAULT false,
  fallbacks jsonb NOT NULL DEFAULT '[]'::jsonb
    CHECK (jsonb_typeof(fallbacks) = 'array'
           AND jsonb_array_length(fallbacks) <= 16
           AND NOT (fallbacks @> jsonb_build_array(alias))),
  fallback_statuses jsonb NOT NULL DEFAULT '[]'::jsonb
    CHECK (jsonb_typeof(fallback_statuses) = 'array'
           AND jsonb_array_length(fallback_statuses) <= 32),
  allow_shadow boolean NOT NULL DEFAULT false,
  legacy_position integer NOT NULL DEFAULT 0 CHECK (legacy_position >= 0),
  scope_owner uuid GENERATED ALWAYS AS (
    COALESCE(owner_id, '00000000-0000-0000-0000-000000000000'::uuid)
  ) STORED,
  PRIMARY KEY (key_id, alias),
  CHECK (upstream_model <> '' OR alias LIKE '%/*'),
  CHECK (alias !~ '\*' OR (alias LIKE '%/*' AND alias <> '/*')),
  FOREIGN KEY (key_id, provider_id, scope_owner)
    REFERENCES provider_keys(id, provider_id, scope_owner)
    ON DELETE CASCADE
);
CREATE UNIQUE INDEX key_models_owned_alias_uq
  ON key_models(provider_id, owner_id, alias) WHERE owner_id IS NOT NULL;
CREATE UNIQUE INDEX key_models_shared_alias_uq
  ON key_models(provider_id, alias) WHERE owner_id IS NULL;
CREATE INDEX key_models_alias_idx ON key_models(alias);
CREATE INDEX key_models_owner_alias_idx
  ON key_models(owner_id, alias) WHERE owner_id IS NOT NULL;

CREATE TABLE model_state (
  model_id text PRIMARY KEY CHECK (length(model_id) BETWEEN 1 AND 200),
  hidden boolean NOT NULL DEFAULT false,
  deleted_at timestamptz,
  updated_at timestamptz NOT NULL DEFAULT now(),
  updated_by uuid REFERENCES users(id) ON DELETE SET NULL
);

CREATE TABLE settings (
  key text PRIMARY KEY CHECK (key IN ('automode', 'cycle', 'search')),
  value jsonb NOT NULL CHECK (jsonb_typeof(value) = 'object'),
  updated_at timestamptz NOT NULL DEFAULT now(),
  updated_by uuid REFERENCES users(id) ON DELETE SET NULL
);

CREATE TABLE system_state (
  key text PRIMARY KEY,
  value jsonb NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now()
);
-- Import marker only; routing versions live in config_state.
INSERT INTO system_state(key, value) VALUES
  ('v2_import', '{}'::jsonb)
ON CONFLICT (key) DO NOTHING;

CREATE TABLE config_state (
  id boolean PRIMARY KEY DEFAULT true CHECK (id),
  version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
  updated_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO config_state(id, version) VALUES (true, 1)
ON CONFLICT (id) DO NOTHING;
```

`provider_keys` rules:

- `owner_id IS NULL` is a shared key and is valid only for
  `auth_scheme = 'none'`.
- `owner_id IS NOT NULL` is a personal key; it may be `none` but the UI creates
  personal keys only for credential-bearing schemes.
- `auth_scheme = 'basic'` stores `{"username":"...","password":"..."}` as the
  plaintext inside the encrypted envelope, or the same JSON in `key_env` /
  `key_ref`.
- `auth_scheme = 'header'` requires `auth_header_name`.
- `auth_scheme = 'none'` requires all credential columns to be null.
- An enabled non-`none` key has **exactly one** credential source:
  `secret_enc`, `key_env` or `key_ref`. They never coexist. A disabled key may
  have zero sources. This removes all runtime precedence/fallback ambiguity.
- `secret_last4` is display-only. For `basic` it is the password's last four
  characters. It is null for `key_env`/`key_ref`.

### 4.5 Scope and integrity triggers

`provider_keys.provider_id` and `owner_id` are immutable after insert. Changing
them would invalidate the AAD and could move a key between users. Ownership
transfer is not supported; delete and recreate with audit.

```sql
CREATE OR REPLACE FUNCTION provider_keys_scope_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.id IS DISTINCT FROM OLD.id
     OR NEW.provider_id IS DISTINCT FROM OLD.provider_id
     OR NEW.owner_id IS DISTINCT FROM OLD.owner_id THEN
    RAISE EXCEPTION 'provider key scope is immutable';
  END IF;
  RETURN NEW;
END $$;

CREATE TRIGGER provider_keys_scope_immutable_bu
BEFORE UPDATE ON provider_keys
FOR EACH ROW EXECUTE FUNCTION provider_keys_scope_immutable();
```

`key_models.provider_id` and `owner_id` are derived from the key. The insert
trigger copies them; the update path rejects drift. The composite foreign key
in §4.4 is the final enforcement, including shared keys (`scope_owner` is the
zero-UUID sentinel).

```sql
CREATE OR REPLACE FUNCTION key_models_sync_scope() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE k record;
BEGIN
  SELECT provider_id, owner_id INTO k FROM provider_keys WHERE id = NEW.key_id;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'key_models.key_id % does not exist', NEW.key_id;
  END IF;
  IF TG_OP = 'INSERT' THEN
    NEW.provider_id := k.provider_id;
    NEW.owner_id := k.owner_id;
  ELSE
    IF NEW.provider_id IS DISTINCT FROM OLD.provider_id
       OR NEW.owner_id IS DISTINCT FROM OLD.owner_id THEN
      RAISE EXCEPTION 'key_models provider_id/owner_id are immutable';
    END IF;
    IF NEW.provider_id IS DISTINCT FROM k.provider_id
       OR NEW.owner_id IS DISTINCT FROM k.owner_id THEN
      RAISE EXCEPTION 'key_models scope must match provider_keys';
    END IF;
  END IF;
  RETURN NEW;
END $$;

CREATE TRIGGER key_models_sync_scope_bi
BEFORE INSERT ON key_models
FOR EACH ROW EXECUTE FUNCTION key_models_sync_scope();

CREATE TRIGGER key_models_sync_scope_bu
BEFORE UPDATE OF key_id, provider_id, owner_id ON key_models
FOR EACH ROW EXECUTE FUNCTION key_models_sync_scope();
```

The last-owner invariant is enforced in the database. The advisory lock
serialises concurrent demotions; user-management transactions use the default
`READ COMMITTED` isolation so each waiter sees the committed result.

```sql
CREATE OR REPLACE FUNCTION users_guard_last_owner() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE remaining integer;
BEGIN
  IF TG_OP = 'DELETE' THEN
    IF OLD.role = 'owner' AND OLD.status = 'active' THEN
      PERFORM pg_advisory_xact_lock(hashtext('gnvipi:owners'));
      SELECT count(*) INTO remaining FROM users
       WHERE role = 'owner' AND status = 'active' AND id <> OLD.id;
      IF remaining = 0 THEN
        RAISE EXCEPTION 'last active owner cannot be deleted';
      END IF;
    END IF;
    RETURN OLD;
  END IF;
  IF OLD.role = 'owner' AND OLD.status = 'active'
     AND (NEW.role <> 'owner' OR NEW.status <> 'active') THEN
    PERFORM pg_advisory_xact_lock(hashtext('gnvipi:owners'));
    SELECT count(*) INTO remaining FROM users
     WHERE role = 'owner' AND status = 'active' AND id <> OLD.id;
    IF remaining = 0 THEN
      RAISE EXCEPTION 'last active owner cannot be demoted or disabled';
    END IF;
  END IF;
  RETURN NEW;
END $$;

CREATE TRIGGER users_guard_last_owner_bu
BEFORE UPDATE OF role, status ON users
FOR EACH ROW EXECUTE FUNCTION users_guard_last_owner();

CREATE TRIGGER users_guard_last_owner_bd
BEFORE DELETE ON users
FOR EACH ROW EXECUTE FUNCTION users_guard_last_owner();
```

Bootstrap is concurrency-safe with `pg_advisory_xact_lock(hashtext
('gnvipi:bootstrap'))`, then an empty-`users` check and insert in one
transaction. `GNVIPI_ADMIN_TOKEN` uses the same lock and is ignored after the
first owner exists.

### 4.6 JSONB contracts

All JSON columns are `NOT NULL`. The API rejects explicit `null`; an empty
object/array means "no overrides/defaults". Application validation returns
`field_errors`; the DB checks below catch structural corruption.

| Column | Shape | Rules |
|---|---|---|
| `providers.endpoint_paths` | object `protocol -> path` | Protocol keys must exist in the adapter registry; values start `/`, contain no query/fragment/control characters, max 2048 bytes; empty means adapter defaults. |
| `providers.forward_headers` | array of strings | HTTP header names, case-insensitively unique, no hop-by-hop headers, no `Host`/`Content-Length`/`Content-Type`/`Content-Encoding`/`Accept*`; max 64. |
| `providers.headers` | object `name -> value` | Valid HTTP header names/values; no CR/LF; no credential headers (`Authorization`, `Proxy-Authorization`, `Cookie`, `Set-Cookie`, `X-Api-Key`, names containing `token`, `secret`, `password`, `credential`); credentials belong in `provider_keys`; max 64 entries. |
| `providers.extra_body` | object | Shallow-merged into the request body; `model` and `stream` are forbidden; no credential-like keys (`api_key`, `token`, `secret`, `password`, `authorization`); max 16 KiB. |
| `providers.extensions` | object | Adapter-specific; each adapter validates its own keys; no secrets; max 64 KiB. |
| `providers.timeouts` | object | Keys `connect`, `tls`, `response_header`, `idle`, `total`; values are duration strings (`"5s"`) or integer nanoseconds; non-negative, max 24h; zero/absent means client default. |
| `key_models.fallbacks` | array of strings | Exact aliases only (no `*`), unique, max 16, no self-reference; resolved within the same user's candidates. |
| `key_models.fallback_statuses` | array of integers | HTTP 100–599, unique, max 32; empty means the default 408, 429 and 5xx set in §8.1. |
| `settings.automode` | object | `{"model": string, "chat_responses": bool}`; `model` empty means default automode. |
| `settings.cycle` | object | `{"name":"cycle","models":[string,...]}`; non-empty unique list; unknown models are ignored by the resolver but preserved. |
| `settings.search` | object | `{"url": string}`; empty disables; otherwise HTTPS and contains `{query}`. |
| `system_state.v2_import` | object | `{"source_sha256": hex, "imported_at": RFC3339, "owner": string}`. |
| `audit_log.details` | object | Allowlisted metadata only; max 8 KiB; never secret, header, body or prompt values. |

`endpoint_paths`/`timeouts` accept the legacy shapes because the Go types
already decode both strings and integer nanoseconds. `extra_body` uses
`json.Decoder` without float coercion in the implementation.

### 4.7 Config version and snapshot invalidation

Every configuration write bumps `config_state.version` through statement
triggers, so a forgotten application call cannot leave a stale snapshot.
`config_state` is deliberately not one of the triggered tables.

```sql
CREATE OR REPLACE FUNCTION bump_config_version() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE v bigint;
BEGIN
  UPDATE config_state
     SET version = version + 1, updated_at = now()
   WHERE id
   RETURNING version INTO v;
  IF v IS NULL THEN
    RAISE EXCEPTION 'config_state row is missing';
  END IF;
  PERFORM pg_notify('gnvipi_config', v::text);
  RETURN NULL;
END $$;

CREATE TRIGGER providers_bump_config
AFTER INSERT OR UPDATE OR DELETE ON providers
FOR EACH STATEMENT EXECUTE FUNCTION bump_config_version();
CREATE TRIGGER provider_keys_bump_config
AFTER INSERT OR UPDATE OR DELETE ON provider_keys
FOR EACH STATEMENT EXECUTE FUNCTION bump_config_version();
CREATE TRIGGER key_models_bump_config
AFTER INSERT OR UPDATE OR DELETE ON key_models
FOR EACH STATEMENT EXECUTE FUNCTION bump_config_version();
CREATE TRIGGER model_state_bump_config
AFTER INSERT OR UPDATE OR DELETE ON model_state
FOR EACH STATEMENT EXECUTE FUNCTION bump_config_version();
CREATE TRIGGER settings_bump_config
AFTER INSERT OR UPDATE OR DELETE ON settings
FOR EACH STATEMENT EXECUTE FUNCTION bump_config_version();
CREATE TRIGGER users_bump_config_on_delete
AFTER DELETE ON users
FOR EACH STATEMENT EXECUTE FUNCTION bump_config_version();
```

`NOTIFY` is delivered on commit. Multiple statement triggers in one transaction
may bump more than once; versions are monotonic and duplicate notifications are
harmless.

Snapshot load:

1. `BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY`.
2. Read `config_state.version`, then all config tables in that transaction.
3. Validate/decode every row; on any error, roll back and keep the previous
   snapshot.
4. Commit, publish the snapshot atomically, and store its version.
5. If `config_state.version` is greater than the loaded version, reload
   immediately; otherwise wait for `LISTEN` or the 30-second poll.

A `LISTEN` connection is dedicated to notifications. On connection loss,
reconnect with backoff and reload. The 30-second poll is the fallback for lost
notifications and DB reconnects. A running process keeps the last good snapshot
when the DB is unavailable; a cold start fails closed.

### 4.8 Secret envelope

- Algorithm: AES-256-GCM.
- Master key: 32 raw bytes, base64 in `GNVIPI_MASTER_KEY`; version in
  `GNVIPI_MASTER_KEY_VERSION` (integer, default 1).
- `secret_kver` is the master-key version used to encrypt; unknown versions are
  a hard decrypt failure.
- AAD: UTF-8 `gnvipi:provider-key:v1:<key_id>:<scope_owner>:<auth_scheme>:<secret_kver>`.
  This binds the ciphertext to the key, owner scope, scheme and key version.
- `secret_nonce`: exactly 12 random bytes. `secret_enc` is ciphertext including
  the GCM tag.
- Plaintext:
  - `bearer`/`header`: raw credential string.
  - `basic`: JSON `{"username":"...","password":"..."}`.
  - `none`: no secret.
- The API never returns `secret_enc`, `secret_nonce`, `secret_kver` or
  plaintext. It returns `has_secret`, `has_key_env`, `has_key_ref` and
  `secret_last4` (when a stored secret exists).
- `GNVIPI_MASTER_KEY_PREVIOUS` holds `version:base64` pairs for the rotation
  window (phase 2).

### 4.9 Lifecycle and cascades

- User delete -> sessions, api_tokens and personal provider_keys cascade;
  key_models cascade from keys. Shared `none` keys remain. Audit rows are
  written before the delete and retain the deleted IDs.
- User disable -> sessions and tokens revoked; keys remain but are unusable
  because authentication fails. No config bump is required for token/session
  revocation.
- Provider delete -> its keys and their models cascade. Audit before delete.
- Key delete -> its models cascade. Audit before delete.
- Model mapping delete -> only that alias disappears.
- `provider_keys` cannot change provider or owner; delete/recreate.
- `key_models` cannot change key/provider/owner; delete/recreate.
- `model_state` is global; only owner/admin may hide/delete a model, and the
  action is audited.

## 5. Migrations, roles and startup

- Runner: `golang-migrate` with an embedded `iofs` source and the pgx v5
  driver.
- Each migration runs in a transaction. PostgreSQL DDL is transactional.
- `serve migrate` takes `pg_advisory_lock(hashtext('gnvipi:migrations'))` for
  the duration. A concurrent migration waits or fails with a clear message.
- Production migration role: DDL on the application schema. Runtime app role:
  DML only.
- `GNVIPI_MIGRATION_DATABASE_URL` is used by `serve migrate`. If unset in
  development only, it falls back to `GNVIPI_DATABASE_URL`.
- `serve --auto-migrate` is a development-only flag. It refuses to run when
  `GNVIPI_ENV=production`; it logs a warning; it is never required in
  production.
- Startup runs the schema guard only:
  - read the applied version;
  - if `dirty`, refuse to start;
  - if the DB version is newer than the binary's embedded maximum, refuse to
    start;
  - if older, refuse and instruct the operator to run `serve migrate`.
- The app role must not own tables and must not have `CREATE` on the schema.
- The app role needs `SELECT` on `schema_migrations`, `SELECT` on
  `config_state`, and `EXECUTE` on `bump_config_version()` (the function is
  `SECURITY DEFINER` and owns the version update).

Role grants:

```sql
GRANT USAGE ON SCHEMA public TO gnvipi_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON
  users, sessions, api_tokens, login_attempts,
  providers, provider_keys, key_models,
  model_state, settings, system_state
TO gnvipi_app;
GRANT SELECT ON config_state, schema_migrations TO gnvipi_app;
GRANT SELECT, INSERT ON audit_log TO gnvipi_app;
GRANT USAGE, SELECT ON SEQUENCE audit_log_id_seq TO gnvipi_app;
GRANT USAGE, SELECT ON SEQUENCE login_attempts_id_seq TO gnvipi_app;
GRANT EXECUTE ON FUNCTION
  set_updated_at(),
  provider_keys_scope_immutable(),
  key_models_sync_scope(),
  users_guard_last_owner(),
  bump_config_version()
TO gnvipi_app;
```

Audit retention runs as a maintenance role (phase 2). The app role never
receives `UPDATE` or `DELETE` on `audit_log`.

Pool and statement settings:

- `GNVIPI_DB_MAX_CONNS` default 10; `GNVIPI_DB_MIN_CONNS` default 1.
- `GNVIPI_DB_CONNECT_TIMEOUT` default 5s.
- Admin requests use `statement_timeout` 10s and `lock_timeout` 2s.
- `idle_in_transaction_session_timeout` 30s.
- On startup, and every 6 hours, delete expired sessions and login attempts
  older than 30 days (app role has DELETE). Audit retention remains phase 2.

## 6. Authentication and security

### 6.1 Passwords

- Argon2id PHC string: `$argon2id$v=19$m=65536,t=3,p=2$<salt>$<hash>`.
- 16-byte random salt per user; parameters configurable.
- Optional pepper: `GNVIPI_PASSWORD_PEPPER`. Apply
  `HMAC-SHA256(pepper, password)` before Argon2id so the pepper is not
  length-limited. A pepper change requires rehash.
- Constant-time compare; rehash on successful login when parameters or pepper
  are outdated.
- Length 12–128 characters; no composition rules.
- Reject a case-insensitive common-password denylist (embedded top 10k) and
  passwords containing the username or email local part.
- No default password. `serve admin create-owner` prompts for one.

### 6.2 Sessions

- 32 random bytes, base64url; only SHA-256 stored.
- Cookie `gw_session`; `HttpOnly`, `SameSite=Strict`, `Secure` when TLS is
  enabled, `Path=/admin`.
- New token on every login; old session row revoked.
- Idle timeout 12h (`last_seen_at`), absolute timeout 7d (`expires_at`).
- `last_seen_at` is updated at most once per minute.
- Logout deletes/revokes the session; revoke-all deletes all sessions for a
  user.
- Disabling or deleting a user revokes all sessions and gateway tokens.
- A session is valid only while the user is `active`.

### 6.3 Forced password change

- `users.must_change_password` is checked in session and token middleware.
- While true, only `GET /admin/api/me`, `POST /admin/api/me/password` and
  logout are allowed. Admin API and gateway API return 403
  `password_change_required`.
- A successful password change clears the flag, revokes all other sessions and
  audits the action. Current session remains valid.
- Owner reset (`serve admin reset-password` or
  `POST /admin/api/users/{id}/password`) sets a new temporary password,
  `must_change_password=true`, and revokes sessions and tokens. Passwords are
  never returned in API responses or audit details.

### 6.4 Brute force and lockout

- Per-account: 5 consecutive failures -> lock 1 minute; doubling to 15 minutes.
- Per-IP: 20 failures in 15 minutes -> generic 429 for that IP.
- Generic error message for unknown user, wrong password and disabled account.
- Unknown users still run a dummy Argon2id verification to equalise timing.
- `login_attempts` records success/failure, username and IP; IP storage can be
  disabled by config.

### 6.5 API tokens

- Gateway token: `gw_` + base64url(32 random bytes).
- Admin token: `adm_` + base64url(32 random bytes).
- Store SHA-256 only; store a short display prefix.
- Gateway token default expiry: **365 days**. No-expiry is allowed and shown
  explicitly.
- Admin token default expiry: 90 days.
- `last_used_at` updated on successful use (at most once per minute).
- Revoked/expired tokens return a generic 401.
- Admin token scope: `read` or `write`. Gateway tokens have no scope.
- Admin tokens authenticate only `/admin/api/*`; gateway tokens authenticate
  only the gateway API. Neither kind is accepted on the other surface.
- Per-token and per-IP rate limiting is phase 2.

### 6.6 CSRF

- Cookie-authenticated writes require:
  - `Origin` or `Sec-Fetch-Site` validation against the configured admin
    origin;
  - a double-submit CSRF token in `X-CSRF-Token` matching the `gw_csrf`
    cookie.
- Bearer-token requests are exempt from CSRF but still require the token.
- `SameSite=Strict` is defence in depth, not the only control.

### 6.7 Bootstrap and recovery

- `serve admin create-owner --username <name>` prompts for a password and
  creates the first owner.
- `--with-gateway-token` prints a one-time gateway token for that owner. The
  token is never written to disk by the command.
- `GNVIPI_ADMIN_TOKEN` may create the first owner only while `users` is empty.
  After that it is ignored and logs a one-time warning.
- `serve admin reset-password <user>` runs on the host, prompts for a new
  password, revokes sessions and gateway tokens, sets
  `must_change_password=true`, and audits the action.
- No default password exists.
- If the master key is lost, encrypted provider secrets are unrecoverable;
  re-enter them. The runbook says this explicitly.

### 6.8 Provider secrets and sensitive configuration

- AES-256-GCM as specified in §4.8.
- Exactly one credential source per enabled key; resolution is deterministic:
  - `secret_enc` -> decrypt with `secret_kver`; failure is a hard key error,
    never a fallback.
  - `key_env` -> read the named environment variable at request time; absence
    is a hard key error.
  - `key_ref` -> resolve the reference in the imported secrets map; absence is
    a hard key error.
- A key whose credential cannot be resolved is excluded from routing. If no
  other candidate exists, return 503 `provider_credential_unavailable` with a
  generic message and audit `provider_key_credential_unavailable` (no secret).
  Do not fall back to another user's key.
- Decryption failure audits `provider_key_decrypt_failed`, returns 503 and
  alerts. It never silently uses a different credential.
- Never returned by any API; only `has_secret`, `has_key_env`, `has_key_ref`
  and `secret_last4`.
- Never in logs, stats, audit details or error text.
- Tests grep API responses, logs, stats and audit rows for the known test
  secret.
- `providers.base_url` and `proxy_url` must not contain userinfo
  (`user:pass@`). Proxy credentials come from the process environment or the
  deployment secret store, not the DB.
- `providers.headers` rejects credential headers and credential-like names.
  `extra_body`/`extensions` reject credential-like keys. These fields are
  non-secret configuration; credentials belong in `provider_keys`.
- Provider test responses return status, latency and a truncated, redacted
  upstream message. Request headers are never echoed; response headers and
  bodies are redacted against known secret values.
- Audit details use an allowlist (IDs, names, booleans, counts). No headers,
  bodies, prompts, keys, env values or refs.

### 6.9 Threat model

- Stolen session cookie: CSRF + idle/absolute expiry + revoke.
- Stolen gateway token: revoke, expiry, `last_used_at`, phase-2 rate limit.
- DB backup leak: ciphertext only; master key separate.
- Cross-user IDOR: ownership middleware returns 404.
- SSRF: only owner/admin may create providers or supply `base_url`; user keys
  cannot change provider URLs. URL policy rejects `file`, `gopher`, and
  non-HTTP(S) schemes; private/link-local addresses require an explicit
  deployment allowlist.
- Timing enumeration: dummy hash for unknown users.
- Secret leakage: write-only secret columns; redaction tests.
- Config corruption: statement triggers bump the version; snapshot reloads are
  all-or-nothing and `REPEATABLE READ`.

## 7. Authorization

Default deny. Every `/admin/api/*` route declares a permission and an ownership
rule. Cross-user access returns **404**, not 403.

| Action | owner | admin | user |
|---|---|---|---|
| Provider definitions (create/update/delete) | yes | yes | no |
| Provider list / enabled providers | yes | yes | read enabled |
| Shared `none` keys | yes | yes | no |
| Own provider keys | yes (all, metadata only for secrets) | metadata + disable only | own CRUD |
| Read a provider secret | never | never | never |
| Per-key model mappings | yes | read metadata | own keys |
| Users | CRUD | no | self read + change password |
| Sessions | revoke any | own | own |
| Admin API tokens | any | own | no |
| Gateway tokens | any | own | own |
| Audit log | read | no | no |
| Stats / requests | all | all aggregate + request metadata | own only |
| Settings (automode/cycle/search) | yes | yes | no |
| Chrome pool / catalog toggles | yes | yes | no |

Notes:

- Owner and admin can disable a key but cannot read its secret.
- Owner can delete any key; the owner of a key can always delete their own key.
  There is no last-key restriction.
- A disabled or deleted user loses sessions and gateway tokens; deleting a user
  cascades personal keys with an audit entry.
- The last active owner cannot be deleted, disabled or demoted; the DB trigger
  is the final authority.
- Admin cannot change a user's key secret, delete another user's key, or read
  audit logs.
- `must_change_password` blocks all routes except the password-change surface,
  regardless of role.

## 8. Routing and snapshot

### 8.1 Resolution

Authenticate the gateway token to an active user. For `(user, alias)`:

1. Personal exact alias, ordered by `legacy_position` (positive positions
   first, zero last), `provider_id`, `key_id`.
2. Personal wildcard, longest prefix first, then the same order.
3. Shared `none` exact alias, then shared wildcard, same ordering.
4. Never another user's key.
5. Fallbacks are appended after each candidate: resolve each exact fallback
   alias with the same user, in list order, skipping duplicates and visited
   aliases, maximum 8 candidates. Cross-provider fallback is allowed; cross-user
   fallback is impossible.

Alias matching:

- Exact match wins over wildcard.
- Wildcards are a trailing `/*` only, after a non-empty prefix; `*` is invalid
  anywhere else. `vendor/*` matches `vendor/<anything with at least one
  character>` and does not match `vendor` or `vendor/`.
- Case-sensitive.
- `upstream_model`:
  - exact alias: literal non-empty model ID;
  - wildcard with empty/`*` `upstream_model`: the requested alias suffix after
    the prefix;
  - wildcard with `upstream_model` ending `/*`: replace the `*` with the suffix;
  - otherwise literal override.
- Duplicate exact aliases across providers/owners are allowed; deterministic
  order decides. Duplicate wildcard prefixes are allowed; longest prefix then
  deterministic order decides. The resolver emits a warning for collisions.

`fallback_statuses`:

- Empty -> default trigger set `408`, `429`, `500`, `502`, `503`, `504` and any
  other 5xx (matching current behavior).
- Non-empty -> only those statuses trigger a fallback.
- Network/TLS/connect errors trigger the next candidate regardless.
- Other 4xx do not trigger fallback.
- No fallback if no candidate remains.

`hidden` and deleted:

- `key_models.hidden` and `model_state.hidden` mean "not listed and not
  routable" (404). This matches the current resolver.
- `model_state.deleted_at` is a soft delete; it wins over `hidden` and also
  makes the model non-routable and non-listed for everyone.
- Disabled providers and keys are excluded.
- A shared `none` key is eligible only when the provider has no credential
  requirement.

`allow_shadow`:

- Existing protected NVIDIA model IDs cannot be shadowed by a custom exact
  alias or wildcard unless `allow_shadow=true`. When true, the custom mapping
  wins. This is the existing v2 behavior, not a general traffic-shadowing
  feature.

If the alias is known but the user has no usable key, return 403
`provider_key_required`; if the alias is unknown, return 404. If a configured
key's credential is unresolvable, return 503
`provider_credential_unavailable` after other candidates are exhausted.

### 8.2 Snapshot

The snapshot contains:

- `version` from `config_state.version`.
- providers by id.
- user id -> enabled keys.
- provider id -> shared `none` keys.
- user id -> alias -> candidates.
- model_state hidden/deleted set.
- settings (automode/cycle/search).

Rebuild rules:

- Rebuild only after a committed write; statement triggers guarantee a version
  bump and `NOTIFY`.
- `LISTEN gnvipi_config`; on notification, reload.
- Poll `config_state.version` every 30 seconds as a fallback.
- Load under `REPEATABLE READ READ ONLY`; publish only a fully validated
  snapshot.
- On `LISTEN` disconnect, reconnect with backoff and reload.
- Two instances must converge after one write; tested with two pools/instances.

### 8.3 Database outage

- A running process keeps serving from the last good snapshot.
- `/healthz` returns degraded and includes `db_ok=false`, `snapshot_version`,
  `config_version`, `snapshot_age`.
- Admin writes return 503; reads may continue from the snapshot.
- A cold start without a DB fails closed.
- After reconnect, reload immediately; do not wait for the 30-second poll.

### 8.4 Stats and audit

Stats/request logs remain the existing JSONL store, not PostgreSQL. This is a
deliberate phase-1 boundary:

- Add additive fields to each persisted `RequestEvent`:
  `request_id` (UUID), `user_id`, `key_id`, `provider_id`, `status` (HTTP).
  Existing fields (`time`, `model`, tokens, latency, TTFB, streamed, error) are
  unchanged.
- Never store prompts, completions, request/response bodies, headers or
  credentials.
- Retention stays the existing 7 days / 16 MiB default.
- `/admin/api/stats` and history filter by the authenticated user for role
  `user`; owner/admin see all records for the instance.
- Known limitation: JSONL is per-process, so multiple gateway instances do not
  aggregate stats. Phase 1 does not claim cross-instance stats. If cross-
  instance stats become a requirement, add a PostgreSQL `request_events` table
  in phase 2; do not silently merge local files.
- Audit is PostgreSQL `audit_log`, append-only for the app role, with explicit
  `user_id`/`key_id`/`provider_id` subject columns and indexes. Audit is
  written in the same transaction as the mutation for user, role, key,
  provider, settings and import changes. Audit failure rolls back the mutation.

## 9. Admin API

### 9.1 Error envelope

All errors use the existing shape:

```json
{
  "error": "human readable",
  "field": "optional.single.field",
  "errors": [{"field": "...", "problem": "..."}],
  "fields": {"...": "..."},
  "field_errors": [{"field": "...", "problem": "..."}]
}
```

Validation errors populate `field_errors`; duplicate alias errors use
`field_errors` on `alias`.

### 9.2 Routes and permissions

| Method | Path | Permission |
|---|---|---|
| GET | `/admin/api/providers` | owner/admin; user sees enabled providers |
| POST | `/admin/api/providers` | owner/admin |
| GET/PUT/DELETE | `/admin/api/providers/{id}` | owner/admin (user read only) |
| POST | `/admin/api/providers/test` | owner/admin |
| POST | `/admin/api/providers/{id}/test` | owner/admin |
| GET/POST | `/admin/api/providers/{id}/keys` | owner/admin (admin metadata), user own list/create own |
| GET/PUT/DELETE | `/admin/api/providers/{id}/keys/{keyID}` | owner; key owner; admin metadata/disable only |
| POST | `/admin/api/providers/{id}/keys/{keyID}/test` | owner; key owner; admin metadata only |
| GET/PUT | `/admin/api/providers/{id}/keys/{keyID}/models` | key owner; owner read/write; admin read |
| GET | `/admin/api/users` | owner |
| POST | `/admin/api/users` | owner |
| GET/PUT/DELETE | `/admin/api/users/{id}` | owner; self read/password |
| POST | `/admin/api/users/{id}/password` | owner; self |
| POST | `/admin/api/users/{id}/revoke` | owner; self |
| GET/POST | `/admin/api/tokens` | owner; admin own; user own gateway |
| DELETE | `/admin/api/tokens/{id}` | owner; token owner |
| GET | `/admin/api/audit` | owner |
| GET/POST | `/admin/api/config/{automode,cycle,search}` | owner/admin |
| GET/POST | `/admin/api/chromes` | owner/admin |
| GET/POST/DELETE | `/admin/api/models...` | owner/admin |
| GET | `/admin/api/stats` | owner/admin all; user own |
| GET | `/admin/api/stats/history` | owner/admin all; user own |

Every route runs the permission middleware before its handler. A route without
a declaration fails closed (500 in tests, 403 in production), so a forgotten
route is caught.

### 9.3 Key CRUD semantics

- `GET` never returns secrets. It returns `has_secret`, `has_key_env`,
  `has_key_ref` and `secret_last4`.
- `POST` accepts `secret` (write-only), `key_env`, `key_ref`.
- `PUT` treats credential fields as a patch when present. The final row is
  validated by the DB:
  - an enabled non-`none` key must end with exactly one source;
  - `clear_secret: true` is legal only if the final row has
    `enabled=false` or another source (`key_env`/`key_ref`);
  - otherwise 422 `field_errors.credential`.
- Clearing a secret also clears `secret_nonce`, `secret_kver` and
  `secret_last4` in the same statement.
- Changing to `none` clears all credential fields.
- Changing to `header` requires `auth_header_name`; changing to `bearer` or
  `basic` clears it.
- Duplicate `name` within the owner scope -> `field_errors.name`.
- Duplicate `alias` within the owner scope -> `field_errors.alias`.
- A key owner may always delete their key. There is no last-key rule.
- Admin may set `enabled=false`; admin may not delete a user's key or change its
  secret.
- Owner may delete any key; before deleting, write an audit row. Personal key
  deletion cascades models.

### 9.4 v2 compatibility

`GET/PUT /admin/api/providers/{id}` remains through `LegacyView`:

- `auth.scheme` mapping:
  - DB `none` -> v2 `none`.
  - DB `bearer` -> v2 `bearer`.
  - DB `header` -> v2 `x-api-key` plus `auth.header_name` (omit
    `header_name` when it is `x-api-key`). This is the existing v2 shape and
    avoids exposing a new scheme to old clients.
  - DB `basic` -> v2 `basic` (additive; old clients that do not know it must
    not edit those providers).
- `auth.key` is never returned.
- `auth.key_env` and `auth.key_ref` are returned only for keys the caller may
  see.
- `models` is a read-only aggregate of the caller's visible key models. A write
  to `models` is accepted only when the caller has exactly one key for that
  provider; otherwise the API returns 409 with `field_errors.models`
  instructing the client to use the per-key models endpoint.
- `endpoint_paths` is the v2 map; the legacy singular `endpoint_path` is
  accepted as a one-entry map only when the protocol is unambiguous.
- `tls` is the profile string `default|chrome`.
- Removed from v2: `auth.key`, `secret`, `secret_enc`, `secret_nonce`,
  `secret_kver`, `secret_last4`, and writable provider-level `models` when
  multiple keys exist. `auth.key_env`, `auth.key_ref`, single-key `models` and
  `endpoint_paths` remain.

### 9.5 Pagination

- Audit: cursor pagination by `(ts, id)`.
  - `GET /admin/api/audit?limit=50&after=<opaque>`; `before` for previous page.
  - The cursor encodes the last row's `ts` and `id`; invalid cursor -> 400.
  - Order: `ts DESC, id DESC`.
- Users and tokens: `limit`/`offset`.
  - Default 50, maximum 200.
  - Stable order: `created_at DESC, id DESC`.
- Stats history: existing frame pagination; no change.

### 9.6 Test endpoint

`POST /admin/api/providers/{id}/keys/{keyID}/test` returns:

```json
{
  "ok": false,
  "status": 401,
  "status_code": 401,
  "latency_ms": 123,
  "message": "upstream rejected the credential",
  "error": "optional redacted upstream error"
}
```

No secret is echoed. Response headers and body are redacted against known
secret values. A failed test is also sent to the page error region.

## 10. Admin UI

- Role-aware shell; no viewer role.
- Screens:
  - Login and forced password change.
  - My Keys: add/edit/delete/test own keys; per-key model mappings.
  - Providers: owner/admin only; shared `none` keys; provider definitions.
  - Users: owner only.
  - Tokens: owner all, admin own, user own gateway tokens.
  - Audit: owner only, cursor pagination.
  - Stats: user own; owner/admin all.
- No Credit, RPM or TPM anywhere.
- Secrets are write-only; show `has_secret`, `last4`, `has_key_env` and
  `has_key_ref` only.
- Base URLs wrap and have a copy button.
- Test results show status, `status_code`, latency and upstream message.
- Charts keep text aspect ratio; top-model colors stay stable across polls.
- Request-log columns sort; hidden-tab polling pauses.
- The three-file embed contract is unchanged. UI-only PRs run the
  backend-untouched check; all UI PRs run `git status --porcelain web/admin`.
- The UI must not offer a "clear secret" action without also disabling the key
  or supplying a replacement source.

## 11. v2 import

Command:

```text
serve admin import-v2 --owner <username> [--file <path>]
```

Behavior:

- Copy the source to `<name>.v2.bak` before reading; never modify the source.
- One transaction under the migration/bootstrap advisory lock.
- Providers with `auth.scheme = none` become shared `none` keys.
- Providers with credentials become owned keys under `--owner`.
- Model mappings become `key_models` rows; `legacy_position` is preserved.
- Idempotency marker: `system_state` key `v2_import` with `source_sha256`,
  `imported_at`, `owner`.
- Re-running with the same source hash is a no-op; a changed source is rejected
  unless `--force` is added.
- On any error, roll back the transaction and leave the source untouched.
- Round-trip test: v2 file -> DB -> `LegacyView` equals the original v2 view,
  except the documented removed fields and the `x-api-key`/`header` mapping.
- If a provider has userinfo in `base_url`/`proxy_url` or credential headers,
  the import fails with a field error; the operator must move them to
  `provider_keys` or the environment.

## 12. Operations and local development

### 12.1 Environment

| Variable | Required | Notes |
|---|---|---|
| `GNVIPI_DATABASE_URL` | yes | secret; never logged; TLS required outside localhost |
| `GNVIPI_MIGRATION_DATABASE_URL` | production | separate migration role |
| `GNVIPI_MAINTENANCE_DATABASE_URL` | phase 2 | audit retention role |
| `GNVIPI_MASTER_KEY` | yes | base64 32-byte AES key |
| `GNVIPI_MASTER_KEY_VERSION` | yes | integer, default 1 |
| `GNVIPI_MASTER_KEY_PREVIOUS` | rotation | `version:base64` pairs |
| `GNVIPI_PASSWORD_PEPPER` | optional | HMAC pepper before Argon2id |
| `GNVIPI_ENV` | optional | `production` disables `--auto-migrate` |
| `GNVIPI_DB_MAX_CONNS` | optional | default 10 |
| `GNVIPI_DB_MIN_CONNS` | optional | default 1 |
| `GNVIPI_STORE_IP` | optional | default true; false stores null IPs |
| `GNVIPI_GATEWAY_TOKEN` | dev script | token for `scripts/run.ps1 -Claude` |
| `GNVIPI_ADMIN_TOKEN` | bootstrap only | first owner only; ignored afterwards |

DSN rules:

- Outside `localhost`/`127.0.0.1`/`::1`, `sslmode=verify-full` is required.
- Reject `sslmode=disable` for non-local hosts.
- Never log the DSN; redact it in errors.

### 12.2 Local PostgreSQL

The repository already has a root `docker-compose.yml` for the `serve`
container. Extend it; do not create a second compose file:

- Add a `db` service: `postgres:17-alpine`, dev-only credentials, healthcheck
  `pg_isready`, named volume, port `5432:5432` (does not touch port 8080).
- Add `GNVIPI_DATABASE_URL` to the `serve` service and `depends_on: db:
  condition: service_healthy`.
- Dev DSN:
  `postgres://gnvipi_dev:gnvipi_dev_only@localhost:5432/gnvipi?sslmode=disable`

`scripts/run.ps1`:

- Before building or starting, check that the DB port is reachable.
- If not, exit with:
  `PostgreSQL is not ready. Start it with: docker compose up -d db`
- Do not auto-start Docker without an explicit flag.
- `-Claude` requires `GNVIPI_GATEWAY_TOKEN`.
- If `GNVIPI_GATEWAY_TOKEN` is absent, reject the legacy literal
  `local-gateway` and print:
  `Create one with: .\bin\serve.exe admin create-owner --username <owner> --with-gateway-token`
  `then set: $env:GNVIPI_GATEWAY_TOKEN='gw_...'`
- Set `ANTHROPIC_AUTH_TOKEN` to the gateway token, never to a hardcoded
  literal.
- Never print the token.

### 12.3 Backup, restore and retention

- Logical backup: `pg_dump`; WAL archiving/PITR is operator-owned.
- Restore runbook includes an integrity check: migrations apply cleanly and
  `SELECT count(*)` on users/providers matches the backup manifest.
- Back up `GNVIPI_MASTER_KEY` separately; ciphertext in backups is useless
  without it.
- Retention:
  - Phase 1: startup and 6-hour cleanup of expired sessions and login attempts
    older than 30 days.
  - Phase 2: daily audit retention under the maintenance role; prunes audit
    older than `GNVIPI_AUDIT_RETENTION_DAYS` (default 365).
- Stats JSONL retention is the existing 7 days / 16 MiB per process.
- Rollback: restore the pre-cutover DB and the old binary/config. A binary
  older than the schema refuses to start; there are no production down
  migrations.

### 12.4 Master-key rotation (phase 2)

- `serve admin rotate-master-key` re-encrypts all provider secrets from the
  previous version to the current version in batches.
- Dual-key decrypt window: current + previous keys.
- Audit the start, count and completion; never audit key material.
- Retire the previous key only after the command reports zero rows at the old
  version.

## 13. Testing and CI

- Unit tests for pure logic.
- Integration tests against a real PostgreSQL service container; no DB mocks.
- Required suites:
  - migrations up, schema guard, dirty/newer schema refusal;
  - all CHECK, generated-scope, composite-FK and partial-unique constraints;
  - `provider_keys` immutability and `key_models` scope drift rejection;
  - credential transition matrix (none/bearer/header/basic, clear secret,
    disabled key without credential, re-enable rejection);
  - secret envelope: nonce length, AAD binding, unknown key version, decrypt
    failure without fallback;
  - Argon2id vectors, rehash, pepper, common-password rejection;
  - `must_change_password` route blocking for sessions and both token kinds;
  - session idle/absolute expiry and revoke;
  - lockout and unknown-user timing;
  - last-owner concurrency (two simultaneous demotions), bootstrap concurrency;
  - role matrix and cross-user IDOR (404);
  - secret masking and redaction grep (API, logs, audit, stats, test output);
  - sensitive config rejection (userinfo URLs, credential headers, extra-body
    credential keys);
  - CSRF;
  - token lifecycle, kind separation and scope;
  - snapshot consistency (REPEATABLE READ, no partial publish), trigger bump
    for every config table, `LISTEN` loss/reconnect, poll fallback;
  - two-instance convergence;
  - DB-down degradation and cold-start failure;
  - routing: exact/wildcard, longest prefix, hidden/deleted, disabled,
    deterministic order, fallback loops/depth, cross-user isolation;
  - v2 import round-trip;
  - `LegacyView` field mapping including `x-api-key`/`header`.
- Frontend CI is unchanged. The backend-untouched check applies only to UI-only
  PRs. The artifact check runs `git status --porcelain web/admin`.
- Test fixtures contain no real secrets.

## 14. Phases and rollout

Phase 1 issues: #185, #190, #191, #186, #187, #188, #189, #212, plus their
implementation children. Phase 1 includes schema, auth, providers/keys/models,
UI, docs/ops and the dev DB environment.

Phase 2 (kept, labelled, not required for the first cutover):

- `serve admin rotate-master-key` (#197).
- Per-token and per-IP API-token rate limits (#198).
- Daily audit retention using the maintenance role (#196).
- Extra hardening: audit tamper tests, stricter CSRF headers, redaction fuzzing,
  backup restore drill (#200).
- Cross-instance stats aggregation, if required.

Rollout:

1. Freeze config writes.
2. Run `serve migrate`.
3. Run `serve admin import-v2 --owner <owner>`.
4. Verify `LegacyView` and provider/key counts.
5. Switch reads to the DB.
6. Keep the v2 file as a read-only backup; no dual-write.
7. Rollback: restore the pre-cutover DB and the old binary/config. A binary
   older than the schema refuses to start.

## 15. Resolved open questions

- Provider creation: owner/admin only. Users cannot supply `base_url`.
- Regular users get the admin UI with a limited My Keys/models view.
- Viewer role: dropped. Roles are owner, admin, user.
- Sessions: 12h idle, 7d absolute. Admin tokens: 90d default. Gateway tokens:
  365d default, no-expiry allowed, `last_used_at` shown.
- Audit pagination: cursor `(ts, id)`. Users/tokens: limit/offset, default 50,
  max 200.
- `basic` auth secret: JSON `{"username","password"}`.
- `--auto-migrate`: development only; production uses `serve migrate`.
- No last-key rule; #203 is renamed.
- Config file: one-time import, no dual-write, DB authoritative.
- Cold start with DB down: fail closed; running instances keep the last
  snapshot.
- Admin sees all aggregate stats and key metadata; audit is owner-only.
- `scripts/run.ps1 -Claude` uses `GNVIPI_GATEWAY_TOKEN`, never `local-gateway`.
- Credential sources: exactly one per enabled key; no runtime fallback.
- Config version: statement triggers, not application calls.
- Key/model ownership: immutable; composite FK plus generated scope.
- Stats: JSONL per process in phase 1; no cross-instance aggregation.
- `hidden` means not listed and not routable, matching the current resolver.

## Appendix A: v2 provider field mapping

| v2 field | DB source | Notes |
|---|---|---|
| `id` | `providers.id` | stable slug |
| `name` | `providers.name` | |
| `base_url` | `providers.base_url` | owner/admin only |
| `proxy_url` | `providers.proxy_url` | no userinfo; credentials from env |
| `protocol` | `providers.protocol` | validated by the adapter registry |
| `auth.scheme` | `provider_keys.auth_scheme` | `none`/`bearer`; `header` -> `x-api-key` + `auth.header_name`; `basic` additive |
| `auth.header_name` | `provider_keys.auth_header_name` | required for `header` |
| `auth.key` | never returned | write-only `secret_enc` |
| `auth.key_env` | `provider_keys.key_env` | visible per permission |
| `auth.key_ref` | `provider_keys.key_ref` | visible per permission |
| `headers` | `providers.headers` | non-secret only |
| `models` | `key_models` | read-only aggregate; per-key writes |
| `timeouts` | `providers.timeouts` | JSON shape preserved |
| `enabled` | `providers.enabled` | |
| `tls` | `providers.tls` | `default|chrome` |
| `forward_headers` | `providers.forward_headers` | |
| `endpoint_paths` | `providers.endpoint_paths` | map keyed by wire protocol |
| `extra_body` | `providers.extra_body` | model/stream forbidden |
| `extensions` | `providers.extensions` | adapter-specific, non-secret |
| `max_concurrency` | `providers.max_concurrency` | |
| `queue_timeout` | `providers.queue_timeout` | nanoseconds |

Removed/changed:

- `auth.key` is never returned.
- Secret-envelope fields (`secret`, `secret_enc`, `secret_nonce`,
  `secret_kver`, `secret_last4`) are not part of v2.
- Provider-level writable `models` is rejected when the caller has more than
  one key for that provider.
- `basic` is additive; old clients that do not understand it must not edit
  those providers.

## Appendix B: `scripts/run.ps1 -Claude` flow

1. Check the DB port; exit with the `docker compose up -d db` message if not
   ready.
2. Build/start the gateway.
3. Require `GNVIPI_GATEWAY_TOKEN`; never use `local-gateway`.
4. Set `ANTHROPIC_BASE_URL` to the local gateway and
   `ANTHROPIC_AUTH_TOKEN` to the gateway token.
5. Never print the token.

## Appendix C: Local dev environment

- Extend the existing root `docker-compose.yml` with the `db` service.
- Keep the `serve` service; add the DB DSN and `depends_on` healthcheck.
- `scripts/run.ps1` checks readiness before starting.
- See #212 for the implementation.

## Appendix D: Repository history audit (2026-11-12)

- `main` reflog: `81f889d` (PR #184 merge) at 13:24:50 +0700, reset to
  `777f99a` at 13:28:28 +0700.
- Discarded UI chain: `70eeb95`, `1a5258f`, `3a1d63f`, `48171e0`, `70ae791`;
  all reachable from `feat/183-admin-ui-rebuild` and PR #211.
- Older non-ancestor merges from the 2026-11-11 reset have identical trees
  (`da69811`/`1ad5f33`, `78bd179`/`bafff9b`, `f1cd69a`/`a1e98c9`,
  `db750f0`/`e07092c`).
- Conclusion: no merged content was lost beyond the UI rebuild, preserved in
  PR #211.

## Appendix E: Issue phases

- Phase 1: #185 schema/migrations, #190 auth core, #191 auth API, #186
  bootstrap, #187 providers/keys/models, #188 admin UI, #189 docs/ops, #212 dev
  environment.
- Phase 2: #196 audit retention, #197 master-key rotation, #198 per-token
  limits, #200 extra hardening, #209/#210 ops/docs follow-ups.
- Dependency order: #185 -> #190 -> #191 -> #186/#187; #189 and #188 after the
  backend children; #212 independent.