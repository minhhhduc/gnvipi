DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'gnvipi_maintenance') THEN
    EXECUTE 'REVOKE SELECT, INSERT, DELETE ON nvidia_models FROM gnvipi_maintenance';
  END IF;
END $$;
