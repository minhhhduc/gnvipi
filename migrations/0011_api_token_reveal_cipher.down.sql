ALTER TABLE api_tokens
  DROP COLUMN IF EXISTS token_kver,
  DROP COLUMN IF EXISTS token_nonce,
  DROP COLUMN IF EXISTS token_enc;
