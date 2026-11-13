DO $$
DECLARE
  elevated boolean;
  owns_tables boolean;
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'gnvipi_app') THEN
    SELECT rolsuper OR rolcreaterole OR rolcreatedb OR rolreplication OR rolbypassrls
      INTO elevated FROM pg_roles WHERE rolname = 'gnvipi_app';
    SELECT EXISTS (
      SELECT 1 FROM pg_class c
      JOIN pg_namespace n ON n.oid = c.relnamespace
      WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p', 'v', 'm', 'S')
        AND pg_get_userbyid(c.relowner) = 'gnvipi_app'
    ) INTO owns_tables;
    IF elevated OR owns_tables THEN
      RAISE EXCEPTION 'gnvipi_app must be a non-privileged runtime role and must not own public objects';
    END IF;

    EXECUTE 'REVOKE ALL PRIVILEGES ON SCHEMA public FROM gnvipi_app';
    EXECUTE 'REVOKE ALL PRIVILEGES ON ALL TABLES IN SCHEMA public FROM gnvipi_app';
    EXECUTE 'REVOKE ALL PRIVILEGES ON ALL SEQUENCES IN SCHEMA public FROM gnvipi_app';
    EXECUTE 'REVOKE ALL PRIVILEGES ON ALL FUNCTIONS IN SCHEMA public FROM gnvipi_app';

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

    IF has_schema_privilege('gnvipi_app', 'public', 'CREATE')
       OR has_table_privilege('gnvipi_app', 'audit_log', 'UPDATE')
       OR has_table_privilege('gnvipi_app', 'audit_log', 'DELETE')
       OR has_table_privilege('gnvipi_app', 'audit_log', 'TRUNCATE')
       OR has_table_privilege('gnvipi_app', 'audit_log', 'REFERENCES')
       OR has_table_privilege('gnvipi_app', 'audit_log', 'TRIGGER') THEN
      RAISE EXCEPTION 'gnvipi_app retains forbidden effective privileges after grant normalization';
    END IF;
  END IF;
END $$;
