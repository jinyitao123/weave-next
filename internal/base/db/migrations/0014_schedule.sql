CREATE TABLE IF NOT EXISTS weave_schedule (
  id            TEXT PRIMARY KEY,
  workspace_id  TEXT NOT NULL REFERENCES weave_workspaces(id) ON DELETE CASCADE,
  agent         TEXT NOT NULL,
  message       TEXT NOT NULL DEFAULT '',
  kind          TEXT NOT NULL CHECK (kind IN ('daily','once')),
  time_of_day   TEXT NOT NULL DEFAULT '',
  run_at        TIMESTAMPTZ,
  enabled       BOOLEAN NOT NULL DEFAULT true,
  last_run_date TEXT NOT NULL DEFAULT '',
  last_run_at   TIMESTAMPTZ,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_schedule_ws ON weave_schedule (workspace_id);
CREATE INDEX IF NOT EXISTS idx_schedule_enabled ON weave_schedule (enabled) WHERE enabled;
