-- A provider key is identified by its id; the name is a display label, like
-- providers.name. Each provider manages its own keys, so two keys may carry
-- the same label without losing track of them: the console renders
-- secret_last4 next to it.
DROP INDEX IF EXISTS provider_keys_owner_name_uq;
DROP INDEX IF EXISTS provider_keys_shared_name_uq;