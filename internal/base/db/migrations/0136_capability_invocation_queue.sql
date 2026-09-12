ALTER TABLE weave_capability_invocations
  ADD COLUMN IF NOT EXISTS task_id TEXT;

CREATE TABLE IF NOT EXISTS weave_capability_invocation_tasks (
    task_id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    invocation_id TEXT NOT NULL,
    capability_id TEXT NOT NULL,
    revision BIGINT NOT NULL CHECK (revision > 0),
    payload JSONB NOT NULL,
    status TEXT NOT NULL DEFAULT 'queued' CHECK (status IN ('queued','running','completed','failed','cancelled')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, invocation_id),
    FOREIGN KEY (workspace_id, invocation_id)
      REFERENCES weave_capability_invocations(workspace_id, invocation_id)
      ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS weave_capability_invocation_tasks_claim
  ON weave_capability_invocation_tasks (status, created_at);
