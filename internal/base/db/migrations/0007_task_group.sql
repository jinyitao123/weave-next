CREATE TABLE IF NOT EXISTS weave_task_group (
  id               TEXT PRIMARY KEY,
  workspace_id     TEXT NOT NULL REFERENCES weave_workspaces(id) ON DELETE CASCADE,
  avatar_agent     TEXT NOT NULL,
  status           TEXT NOT NULL DEFAULT 'active'
                   CHECK (status IN ('active','resolving','resolved')),
  original_request TEXT NOT NULL DEFAULT '',
  quorum           INT  NOT NULL DEFAULT 0,
  deadline_at      TIMESTAMPTZ,
  group_outcome    TEXT,
  card_message_id  TEXT,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  resolved_at      TIMESTAMPTZ
);

ALTER TABLE weave_task_queue ADD COLUMN IF NOT EXISTS task_group_id TEXT
  REFERENCES weave_task_group(id) ON DELETE CASCADE;
ALTER TABLE weave_task_queue ADD COLUMN IF NOT EXISTS subtask_deadline_at TIMESTAMPTZ;

ALTER TABLE weave_task_queue DROP CONSTRAINT IF EXISTS weave_task_queue_status_check;
ALTER TABLE weave_task_queue ADD CONSTRAINT weave_task_queue_status_check
  CHECK (status IN ('queued','dispatched','running','completed','failed',
                    'cancelled','superseded','cut','timed_out'));

CREATE INDEX IF NOT EXISTS idx_task_queue_group ON weave_task_queue (task_group_id)
  WHERE task_group_id IS NOT NULL;
