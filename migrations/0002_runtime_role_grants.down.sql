DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'gnvipi_app') THEN
    EXECUTE 'REVOKE EXECUTE ON FUNCTION '
         || 'set_updated_at(), provider_keys_scope_immutable(), '
         || 'key_models_sync_scope(), users_guard_last_owner(), '
         || 'bump_config_version() '
         || 'FROM gnvipi_app';
    EXECUTE 'REVOKE USAGE, SELECT ON SEQUENCE login_attempts_id_seq FROM gnvipi_app';
    EXECUTE 'REVOKE USAGE, SELECT ON SEQUENCE audit_log_id_seq FROM gnvipi_app';
    EXECUTE 'REVOKE SELECT, INSERT ON audit_log FROM gnvipi_app';
    EXECUTE 'REVOKE SELECT ON config_state, schema_migrations FROM gnvipi_app';
    EXECUTE 'REVOKE SELECT, INSERT, UPDATE, DELETE ON '
         || 'users, sessions, api_tokens, login_attempts, '
         || 'providers, provider_keys, key_models, '
         || 'model_state, settings, system_state '
         || 'FROM gnvipi_app';
    EXECUTE 'REVOKE USAGE ON SCHEMA public FROM gnvipi_app';
  END IF;
END $$;
