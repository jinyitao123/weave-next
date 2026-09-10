-- Workbench records the exact user-sourced input before it asks an execution
-- tool to dispatch. These records bind the existing workflow queue; they are
-- not an additional task system.
CREATE TABLE weave_dispatch_input_revisions (
  workspace_id TEXT NOT NULL REFERENCES weave_workspaces(id) ON DELETE CASCADE,
  user_id TEXT NOT NULL,
  workbench_session_id TEXT NOT NULL,
  input_revision_id TEXT NOT NULL,
  registration_id TEXT NOT NULL,
  registration_sha256 TEXT NOT NULL CHECK (registration_sha256 ~ '^[0-9a-f]{64}$'),
  source_messages JSONB NOT NULL CHECK (jsonb_typeof(source_messages) = 'array' AND jsonb_array_length(source_messages) > 0),
  task TEXT NOT NULL CHECK (task <> ''),
  task_sha256 TEXT NOT NULL CHECK (task_sha256 ~ '^[0-9a-f]{64}$'),
  team_id TEXT NOT NULL,
  mode TEXT NOT NULL CHECK (mode = 'workflow'),
  workflow_id TEXT NOT NULL,
  workflow_version INTEGER NOT NULL CHECK (workflow_version > 0),
  project_id TEXT NOT NULL DEFAULT '',
  client_request_id TEXT NOT NULL,
  is_current BOOLEAN NOT NULL DEFAULT true,
  consumed_run_id TEXT,
  consumed_task_id TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
  consumed_at TIMESTAMPTZ,
  closed_at TIMESTAMPTZ,
  PRIMARY KEY (workspace_id, user_id, input_revision_id),
  UNIQUE (workspace_id, user_id, registration_id),
  UNIQUE (workspace_id, user_id, client_request_id),
  CHECK (user_id <> '' AND workbench_session_id <> '' AND team_id <> '' AND workflow_id <> ''),
  CHECK ((consumed_run_id IS NULL AND consumed_task_id IS NULL AND consumed_at IS NULL)
    OR (consumed_run_id IS NOT NULL AND consumed_run_id <> '' AND consumed_task_id IS NOT NULL
      AND consumed_task_id <> '' AND consumed_at IS NOT NULL)),
  CHECK (closed_at IS NULL OR consumed_run_id IS NULL)
);

CREATE UNIQUE INDEX weave_dispatch_input_current
  ON weave_dispatch_input_revisions(workspace_id, user_id, workbench_session_id)
  WHERE is_current;
