DO $$
DECLARE
  role_oid oid;
  elevated boolean;
  owns_objects boolean;
  forbidden_tables boolean;
  forbidden_sequences boolean;
BEGIN
  SELECT oid, rolsuper OR rolcreaterole OR rolcreatedb OR rolreplication OR rolbypassrls
    INTO role_oid, elevated FROM pg_roles WHERE rolname = 'gnvipi_app';
  IF role_oid IS NOT NULL THEN
    SELECT EXISTS (
      SELECT 1 FROM pg_auth_members WHERE member = role_oid
    ) INTO forbidden_tables;
    IF forbidden_tables THEN
      RAISE EXCEPTION 'gnvipi_app must not inherit privileges from other roles';
    END IF;

    SELECT EXISTS (
      SELECT 1 FROM pg_class c
      JOIN pg_namespace n ON n.oid = c.relnamespace
      WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p', 'v', 'm', 'S', 'f')
        AND c.relowner = role_oid
      UNION ALL
      SELECT 1 FROM pg_proc p
      JOIN pg_namespace n ON n.oid = p.pronamespace
      WHERE n.nspname = 'public' AND p.proowner = role_oid
    ) INTO owns_objects;
    IF elevated OR owns_objects THEN
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

    SELECT EXISTS (
      SELECT 1 FROM pg_class c
      JOIN pg_namespace n ON n.oid = c.relnamespace
      WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p', 'v', 'm', 'f')
        AND (
          has_table_privilege('gnvipi_app', c.oid, 'TRUNCATE')
          OR has_table_privilege('gnvipi_app', c.oid, 'REFERENCES')
          OR has_table_privilege('gnvipi_app', c.oid, 'TRIGGER')
          OR (c.relname = 'audit_log' AND (
            has_table_privilege('gnvipi_app', c.oid, 'UPDATE')
            OR has_table_privilege('gnvipi_app', c.oid, 'DELETE')))
          OR (c.relname IN ('config_state', 'schema_migrations') AND (
            has_table_privilege('gnvipi_app', c.oid, 'INSERT')
            OR has_table_privilege('gnvipi_app', c.oid, 'UPDATE')
            OR has_table_privilege('gnvipi_app', c.oid, 'DELETE')))
        )
    ) INTO forbidden_tables;
    SELECT EXISTS (
      SELECT 1 FROM pg_class c
      JOIN pg_namespace n ON n.oid = c.relnamespace
      WHERE n.nspname = 'public' AND c.relkind = 'S'
        AND (has_sequence_privilege('gnvipi_app', c.oid, 'UPDATE')
          OR (c.relname NOT IN ('audit_log_id_seq', 'login_attempts_id_seq') AND (
            has_sequence_privilege('gnvipi_app', c.oid, 'USAGE')
            OR has_sequence_privilege('gnvipi_app', c.oid, 'SELECT'))))
    ) INTO forbidden_sequences;
    IF has_schema_privilege('gnvipi_app', 'public', 'CREATE')
       OR forbidden_tables OR forbidden_sequences THEN
      RAISE EXCEPTION 'gnvipi_app retains forbidden effective privileges after grant normalization';
    END IF;
  END IF;
END $$;
