-- GNVIPI initial schema (#185).
-- Normative source: docs/superpowers/specs/2026-11-12-db-backed-provider-key-model-design.md
-- golang-migrate executes this file as one PostgreSQL simple-query message,
-- which runs inside an implicit transaction; every statement below is
-- transactional. No plaintext secret is stored: credentials live only in
-- write-only provider_keys columns.

CREATE EXTENSION IF NOT EXISTS citext;

CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  NEW.updated_at = now();
  RETURN NEW;
END $$;

-- 4.3 Identity tables -------------------------------------------------------

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

-- 4.4 Configuration tables --------------------------------------------------

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
  CONSTRAINT provider_keys_owner_none
    CHECK (owner_id IS NOT NULL OR auth_scheme = 'none'),
  CONSTRAINT provider_keys_secret_nonce_pair
    CHECK ((secret_enc IS NULL) = (secret_nonce IS NULL)),
  CONSTRAINT provider_keys_secret_kver_pair
    CHECK ((secret_enc IS NULL) = (secret_kver IS NULL)),
  CONSTRAINT provider_keys_nonce_length
    CHECK (secret_enc IS NULL OR octet_length(secret_nonce) = 12),
  CONSTRAINT provider_keys_kver_positive
    CHECK (secret_kver IS NULL OR secret_kver > 0),
  CONSTRAINT provider_keys_secret_last4_pair
    CHECK ((secret_enc IS NULL) = (secret_last4 IS NULL)),
  CONSTRAINT provider_keys_last4_length
    CHECK (secret_last4 IS NULL OR length(secret_last4) <= 8),
  CONSTRAINT provider_keys_key_env_syntax
    CHECK (key_env IS NULL
           OR key_env ~ '^[A-Za-z_][A-Za-z0-9_]{0,127}$'),
  CONSTRAINT provider_keys_key_ref_syntax
    CHECK (key_ref IS NULL
           OR (length(key_ref) BETWEEN 1 AND 512
               AND key_ref !~ '[[:cntrl:]]')),
  CONSTRAINT provider_keys_header_name_required
    CHECK (auth_scheme <> 'header'
           OR (auth_header_name IS NOT NULL
               AND auth_header_name ~ '^[!#$%&''*+\-.^_`|~0-9A-Za-z]+$')),
  CONSTRAINT provider_keys_header_name_only
    CHECK (auth_scheme = 'header' OR auth_header_name IS NULL),
  -- Exactly one credential source for an enabled non-none key. A disabled key
  -- may have zero sources, which is what makes clear_secret + enabled=false
  -- a legal state.
  CONSTRAINT provider_keys_credential_single
    CHECK (num_nonnulls(secret_enc, key_env, key_ref) <= 1),
  CONSTRAINT provider_keys_credential_required
    CHECK (auth_scheme = 'none'
           OR NOT enabled
           OR num_nonnulls(secret_enc, key_env, key_ref) = 1),
  CONSTRAINT provider_keys_none_clears_credentials
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

-- 4.1 updated_at triggers ---------------------------------------------------

CREATE TRIGGER users_set_updated_at
BEFORE UPDATE ON users
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER providers_set_updated_at
BEFORE UPDATE ON providers
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER provider_keys_set_updated_at
BEFORE UPDATE ON provider_keys
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER model_state_set_updated_at
BEFORE UPDATE ON model_state
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER settings_set_updated_at
BEFORE UPDATE ON settings
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER system_state_set_updated_at
BEFORE UPDATE ON system_state
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER config_state_set_updated_at
BEFORE UPDATE ON config_state
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- 4.5 Scope and integrity triggers ------------------------------------------

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

-- key_models.provider_id and owner_id are derived from the key. The insert
-- trigger copies them; the update path rejects drift. The composite foreign
-- key above is the final enforcement, including shared keys (zero-UUID
-- scope_owner sentinel).
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

-- The advisory lock serialises concurrent demotions; user-management
-- transactions use READ COMMITTED so each waiter sees the committed result.
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

-- 4.7 Config version and snapshot invalidation ------------------------------

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

-- 5 Roles and grants --------------------------------------------------------
-- The application role is created by the operator before `serve migrate`.
-- Grants are applied only when the role exists so a disposable test/dev
-- database can apply this migration without cluster-wide role creation.
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'gnvipi_app') THEN
    EXECUTE 'GRANT USAGE ON SCHEMA public TO gnvipi_app';
    EXECUTE 'GRANT SELECT, INSERT, UPDATE, DELETE ON '
         || 'users, sessions, api_tokens, login_attempts, '
         || 'providers, provider_keys, key_models, '
         || 'model_state, settings, system_state '
         || 'TO gnvipi_app';
    EXECUTE 'GRANT SELECT ON config_state, schema_migrations TO gnvipi_app';
    EXECUTE 'GRANT SELECT, INSERT ON audit_log TO gnvipi_app';
    EXECUTE 'GRANT USAGE, SELECT ON SEQUENCE audit_log_id_seq TO gnvipi_app';
    EXECUTE 'GRANT USAGE, SELECT ON SEQUENCE login_attempts_id_seq TO gnvipi_app';
    EXECUTE 'GRANT EXECUTE ON FUNCTION '
         || 'set_updated_at(), provider_keys_scope_immutable(), '
         || 'key_models_sync_scope(), users_guard_last_owner(), '
         || 'bump_config_version() '
         || 'TO gnvipi_app';
  END IF;
END $$;
