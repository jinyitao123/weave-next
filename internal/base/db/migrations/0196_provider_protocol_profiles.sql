ALTER TABLE weave_provider_credentials
 ADD COLUMN protocol TEXT NOT NULL DEFAULT '',
 ADD COLUMN thinking_default_mode TEXT NOT NULL DEFAULT '',
 ADD COLUMN thinking_disable_with_tools BOOLEAN NOT NULL DEFAULT false,
 ADD COLUMN attempt_timeout_seconds INTEGER NOT NULL DEFAULT 0;
ALTER TABLE weave_provider_revisions
 ADD COLUMN protocol TEXT NOT NULL DEFAULT '',
 ADD COLUMN thinking_default_mode TEXT NOT NULL DEFAULT '',
 ADD COLUMN thinking_disable_with_tools BOOLEAN NOT NULL DEFAULT false,
 ADD COLUMN attempt_timeout_seconds INTEGER NOT NULL DEFAULT 0;
