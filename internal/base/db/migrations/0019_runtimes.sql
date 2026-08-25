CREATE TABLE IF NOT EXISTS weave_runtimes (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES weave_workspaces(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  engines JSONB NOT NULL DEFAULT '[]',
  token_hash TEXT NOT NULL,
  last_heartbeat_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (workspace_id, name)
);
ALTER TABLE weave_task_queue ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'chat';
ALTER TABLE weave_task_queue ADD COLUMN IF NOT EXISTS runtime_id TEXT;
CREATE INDEX IF NOT EXISTS idx_task_queue_claim
  ON weave_task_queue (status, kind, runtime_id, priority DESC, created_at);
