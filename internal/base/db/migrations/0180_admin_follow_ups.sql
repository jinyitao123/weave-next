-- A follow-up is a new run of the same task that starts from the commit the
-- previous run delivered. The seed is that run's Host-written version and
-- cumulative patch, frozen with the follow-up so any node can rebuild the
-- exact commit even when it was never pushed.
ALTER TABLE weave_run_code_contexts
  ADD COLUMN IF NOT EXISTS parent_run_id TEXT NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS root_run_id   TEXT NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS seed_version  TEXT NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS seed_patch    TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS weave_run_code_contexts_thread
  ON weave_run_code_contexts (workspace_id, root_run_id) WHERE root_run_id <> '';
