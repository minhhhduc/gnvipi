ALTER TABLE api_tokens
  ADD COLUMN IF NOT EXISTS token_enc bytea,
  ADD COLUMN IF NOT EXISTS token_nonce bytea,
  ADD COLUMN IF NOT EXISTS token_kver integer;
