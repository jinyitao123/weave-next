-- One logical frozen member invocation survives parent execution epochs.
-- Loom checkpoints and operation receipts remain in the shared loom_store.
CREATE TABLE weave_workflow_member_runs (
  workspace_id TEXT NOT NULL,
  parent_run_id TEXT NOT NULL,
  member_run_id TEXT NOT NULL,
  call_id TEXT NOT NULL,
  run_snapshot_id TEXT NOT NULL,
  node_id TEXT NOT NULL,
  parent_generation BIGINT NOT NULL CHECK (parent_generation >= 0),
  identity_hash TEXT NOT NULL CHECK (length(identity_hash) = 64),
  initial_state JSONB NOT NULL,
  checkpoint_seq BIGINT NOT NULL DEFAULT 0 CHECK (checkpoint_seq >= 0),
  result JSONB,
  created_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
  PRIMARY KEY (workspace_id, parent_run_id, call_id),
  UNIQUE (workspace_id, member_run_id),
  CHECK (workspace_id <> '' AND parent_run_id <> '' AND member_run_id <> ''
    AND call_id <> '' AND run_snapshot_id <> '' AND node_id <> '')
);
