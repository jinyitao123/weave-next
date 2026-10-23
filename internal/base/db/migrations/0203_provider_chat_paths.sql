-- The exact request address of a provider is base_url || chat_path. An empty
-- chat path is the form every existing provider has: its address keeps the
-- rule its client always applied (a trailing /v1 dropped from the base, then
-- the protocol's default path appended), so no address changes on upgrade.
ALTER TABLE weave_provider_credentials
 ADD COLUMN chat_path TEXT NOT NULL DEFAULT '';
ALTER TABLE weave_provider_revisions
 ADD COLUMN chat_path TEXT NOT NULL DEFAULT '';
