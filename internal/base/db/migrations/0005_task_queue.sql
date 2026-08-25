-- weave_jobs is intentionally not backfilled during the rebuild; its legacy DDL
-- remains in 0001 so existing databases are not damaged.
CREATE TABLE IF NOT EXISTS weave_task_queue (
  id               TEXT PRIMARY KEY,
  workspace_id     TEXT NOT NULL REFERENCES weave_workspaces(id) ON DELETE CASCADE,
  agent            TEXT NOT NULL,
  source           TEXT NOT NULL DEFAULT 'chat',
  status           TEXT NOT NULL DEFAULT 'queued'
                   CHECK (status IN ('queued','dispatched','running','completed','failed','cancelled','superseded')),
  priority         INT NOT NULL DEFAULT 0,
  context_key      TEXT,
  trace_id         TEXT,
  parent_task_id   TEXT,
  payload          JSONB NOT NULL,
  result           JSONB,
  error            TEXT,
  run_id           TEXT,
  worker_id        TEXT,
  lease_expires_at TIMESTAMPTZ,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  started_at       TIMESTAMPTZ,
  completed_at     TIMESTAMPTZ,
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_task_queue_claim
  ON weave_task_queue (status, priority DESC, created_at)
  WHERE status = 'queued';
CREATE INDEX IF NOT EXISTS idx_task_queue_ws
  ON weave_task_queue (workspace_id, status);
CREATE INDEX IF NOT EXISTS idx_task_queue_ctx
  ON weave_task_queue (workspace_id, context_key)
  WHERE context_key IS NOT NULL;
