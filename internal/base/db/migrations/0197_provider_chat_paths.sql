-- The exact request address of a provider is base_url || chat_path. Existing
-- rows keep an empty path and are refused until reconfigured: the old path was
-- implied by the client library, not recorded, so it is not reconstructed.
ALTER TABLE weave_provider_credentials
 ADD COLUMN chat_path TEXT NOT NULL DEFAULT '';
ALTER TABLE weave_provider_revisions
 ADD COLUMN chat_path TEXT NOT NULL DEFAULT '';
