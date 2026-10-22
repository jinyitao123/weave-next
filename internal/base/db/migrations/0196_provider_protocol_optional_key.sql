-- A provider speaks one wire protocol: OpenAI-compatible chat completions
-- (the default and every existing row) or the System One decision protocol.
-- The protocol is functional configuration, so each immutable revision
-- records it; the revision content hash includes it when it is not 'openai'.
ALTER TABLE weave_provider_credentials
  ADD COLUMN protocol TEXT NOT NULL DEFAULT 'openai',
  ADD CONSTRAINT weave_provider_credentials_protocol_check
    CHECK (protocol IN ('openai', 'systemone'));

ALTER TABLE weave_provider_revisions
  ADD COLUMN protocol TEXT NOT NULL DEFAULT 'openai',
  ADD CONSTRAINT weave_provider_revisions_protocol_check
    CHECK (protocol IN ('openai', 'systemone'));

-- API keys are optional. api_key_set lets metadata reads tell a stored key
-- from none without opening the secret. Existing rows are classified by the
-- ciphertext's shape only: an empty key seals to base64(12-byte nonce ||
-- 16-byte tag), 28 bytes, i.e. exactly 40 characters ending in "=="; a key of
-- any length is at least 29 bytes, which never encodes that way. Unreadable
-- rows count as keyed.
ALTER TABLE weave_provider_credentials
  ADD COLUMN api_key_set BOOLEAN NOT NULL DEFAULT false;

UPDATE weave_provider_credentials
SET api_key_set = NOT (length(api_key_cipher) = 40 AND right(api_key_cipher, 2) = '==');
