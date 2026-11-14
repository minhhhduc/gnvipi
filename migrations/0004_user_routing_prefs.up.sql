-- Per-user automode and cycle preferences (#240).
-- One row per account; deleting the user removes its routing preferences.
CREATE TABLE user_routing_prefs (
  user_id uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  auto_model text NOT NULL DEFAULT ''
    CHECK (length(auto_model) <= 200),
  cycle_models jsonb NOT NULL DEFAULT '[]'::jsonb
    CHECK (jsonb_typeof(cycle_models) = 'array'
           AND jsonb_array_length(cycle_models) <= 64),
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TRIGGER user_routing_prefs_set_updated_at
BEFORE UPDATE ON user_routing_prefs
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Admin writes run under the application role, as every other admin write
-- does; mirror 0002/0003's conditional grant so a disposable database without
-- gnvipi_app still migrates.
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'gnvipi_app') THEN
    EXECUTE 'GRANT SELECT, INSERT, UPDATE, DELETE ON user_routing_prefs TO gnvipi_app';
  END IF;
END $$;