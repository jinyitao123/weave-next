ALTER TABLE weave_capability_invocations
  ADD COLUMN IF NOT EXISTS actor_user_id TEXT NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS runtime_id TEXT NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS checkpoint JSONB NOT NULL DEFAULT '{}'::jsonb,
  ADD COLUMN IF NOT EXISTS used_steps INTEGER NOT NULL DEFAULT 0 CHECK (used_steps >= 0),
  ADD COLUMN IF NOT EXISTS max_steps INTEGER NOT NULL DEFAULT 100 CHECK (max_steps BETWEEN 1 AND 1000),
  ADD COLUMN IF NOT EXISTS started_at TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS completed_at TIMESTAMPTZ;

ALTER TABLE weave_capability_invocation_tasks DROP CONSTRAINT IF EXISTS weave_capability_invocation_tasks_status_check;
ALTER TABLE weave_capability_invocation_tasks ADD CONSTRAINT weave_capability_invocation_tasks_status_check
  CHECK (status IN ('queued','running','waiting','completed','failed','cancelled'));

CREATE TABLE IF NOT EXISTS weave_capability_invocation_events (
  workspace_id TEXT NOT NULL,
  invocation_id TEXT NOT NULL,
  sequence BIGSERIAL,
  event_type TEXT NOT NULL,
  step_id TEXT NOT NULL DEFAULT '',
  step_kind TEXT NOT NULL DEFAULT '',
  detail JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, invocation_id, sequence),
  FOREIGN KEY (workspace_id, invocation_id) REFERENCES weave_capability_invocations(workspace_id, invocation_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS weave_capability_human_tasks (
  workspace_id TEXT NOT NULL,
  invocation_id TEXT NOT NULL,
  step_id TEXT NOT NULL,
  title TEXT NOT NULL,
  response_schema JSONB NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('waiting','completed','cancelled')),
  response JSONB,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  completed_at TIMESTAMPTZ,
  PRIMARY KEY (workspace_id, invocation_id, step_id),
  FOREIGN KEY (workspace_id, invocation_id) REFERENCES weave_capability_invocations(workspace_id, invocation_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS weave_capability_quotas (
  workspace_id TEXT PRIMARY KEY,
  max_steps_per_invocation INTEGER NOT NULL DEFAULT 100 CHECK (max_steps_per_invocation BETWEEN 1 AND 1000),
  max_active_invocations INTEGER NOT NULL DEFAULT 20 CHECK (max_active_invocations BETWEEN 1 AND 1000),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS weave_capability_invocation_actor_history ON weave_capability_invocations(workspace_id,actor_user_id,created_at DESC);
