-- Add the mechanical version-lock seam for queued Agent work. Existing rows
-- remain unversioned because their historical selection cannot be proven.
ALTER TABLE weave_task_queue
  ADD COLUMN agent_id TEXT,
  ADD COLUMN agent_version INT;

ALTER TABLE weave_task_queue
  ADD CONSTRAINT weave_task_queue_agent_version_pair_check CHECK (
    (agent_id IS NULL AND agent_version IS NULL)
    OR (agent_id IS NOT NULL AND agent_version IS NOT NULL AND agent_version > 0)
  ),
  ADD CONSTRAINT weave_task_queue_agent_workspace_fk
    FOREIGN KEY (workspace_id, agent_id)
    REFERENCES weave_agents (workspace_id, id) ON DELETE RESTRICT,
  ADD CONSTRAINT weave_task_queue_agent_version_fk
    FOREIGN KEY (agent_id, agent_version)
    REFERENCES weave_agent_versions (agent_id, version) ON DELETE RESTRICT;

CREATE FUNCTION weave_task_queue_reject_agent_version_rewrite()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF OLD.agent_id IS DISTINCT FROM NEW.agent_id
     OR OLD.agent_version IS DISTINCT FROM NEW.agent_version THEN
    RAISE EXCEPTION 'queued task agent version reference is immutable'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER weave_task_queue_agent_version_immutable
BEFORE UPDATE OF agent_id, agent_version ON weave_task_queue
FOR EACH ROW
EXECUTE FUNCTION weave_task_queue_reject_agent_version_rewrite();

CREATE FUNCTION weave_agent_versions_reject_update()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'agent version records are immutable'
    USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER weave_agent_versions_immutable
BEFORE UPDATE ON weave_agent_versions
FOR EACH ROW
EXECUTE FUNCTION weave_agent_versions_reject_update();
