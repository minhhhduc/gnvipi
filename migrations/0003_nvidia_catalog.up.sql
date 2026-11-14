-- NVIDIA Playground catalog (#237).
-- The table holds exactly what the last successful rescan fetched from
-- build.nvidia.com; there is no built-in or committed catalog.
CREATE TABLE nvidia_models (
  model text PRIMARY KEY
    CHECK (length(model) BETWEEN 1 AND 200),
  slug text NOT NULL
    CHECK (length(slug) BETWEEN 1 AND 200),
  namespace text NOT NULL
    CHECK (length(namespace) BETWEEN 1 AND 64),
  function_id uuid NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now()
);

-- The runtime role reads the catalog; writes run under the migration or
-- maintenance role, which owns / is granted the table. Mirror 0002's
-- conditional grant so a disposable database without gnvipi_app still
-- migrates.
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'gnvipi_app') THEN
    EXECUTE 'GRANT SELECT ON nvidia_models TO gnvipi_app';
  END IF;
END $$;
