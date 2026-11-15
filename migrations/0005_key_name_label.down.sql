-- Restoring the name indexes fails if duplicate labels already exist; rename
-- the extra rows first.
CREATE UNIQUE INDEX IF NOT EXISTS provider_keys_owner_name_uq
  ON provider_keys(provider_id, owner_id, name) WHERE owner_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS provider_keys_shared_name_uq
  ON provider_keys(provider_id, name) WHERE owner_id IS NULL;