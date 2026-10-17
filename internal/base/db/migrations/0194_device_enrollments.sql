-- A device is a runtime that one person enrolled for themselves with a
-- one-time code; only that person may control it. Runtimes registered by an
-- administrator keep no owner and are never devices.
ALTER TABLE weave_runtimes ADD COLUMN IF NOT EXISTS device_owner_user_id TEXT;

-- One-time enrollment codes. Only a digest of the code is kept. A row stays
-- after use as the record of who added which device and when.
CREATE TABLE IF NOT EXISTS weave_device_enrollments (
  id           UUID        PRIMARY KEY,
  workspace_id TEXT        NOT NULL REFERENCES weave_workspaces(id) ON DELETE CASCADE,
  user_id      TEXT        NOT NULL,
  code_digest  TEXT        NOT NULL UNIQUE,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at   TIMESTAMPTZ NOT NULL,
  used_at      TIMESTAMPTZ,
  runtime_id   TEXT
);
CREATE INDEX IF NOT EXISTS idx_device_enrollments_user
  ON weave_device_enrollments (workspace_id, user_id, created_at DESC);
