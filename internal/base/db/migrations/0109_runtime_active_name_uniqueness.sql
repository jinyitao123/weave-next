ALTER TABLE weave_runtimes
  DROP CONSTRAINT IF EXISTS weave_runtimes_workspace_id_name_key;

CREATE UNIQUE INDEX IF NOT EXISTS weave_runtimes_active_workspace_name_idx
  ON weave_runtimes (workspace_id, name)
  WHERE deleted_at IS NULL;
