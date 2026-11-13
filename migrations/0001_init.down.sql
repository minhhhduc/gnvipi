-- Reverse 0001_init. Safe to run repeatedly; the citext extension is left in
-- place because it may be shared with other schemas in the database.

DROP TABLE IF EXISTS config_state;
DROP TABLE IF EXISTS system_state;
DROP TABLE IF EXISTS settings;
DROP TABLE IF EXISTS model_state;
DROP TABLE IF EXISTS key_models;
DROP TABLE IF EXISTS provider_keys;
DROP TABLE IF EXISTS providers;
DROP TABLE IF EXISTS audit_log;
DROP TABLE IF EXISTS login_attempts;
DROP TABLE IF EXISTS api_tokens;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS users;

DROP FUNCTION IF EXISTS bump_config_version();
DROP FUNCTION IF EXISTS users_guard_last_owner();
DROP FUNCTION IF EXISTS key_models_sync_scope();
DROP FUNCTION IF EXISTS provider_keys_scope_immutable();
DROP FUNCTION IF EXISTS set_updated_at();
