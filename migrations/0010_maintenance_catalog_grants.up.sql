-- Grant NVIDIA catalog write privileges to the maintenance role when present (#331).
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'gnvipi_maintenance') THEN
    EXECUTE 'GRANT USAGE ON SCHEMA public TO gnvipi_maintenance';
    EXECUTE 'GRANT SELECT, INSERT, DELETE ON nvidia_models TO gnvipi_maintenance';
  END IF;
END $$;
