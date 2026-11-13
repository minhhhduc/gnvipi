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
