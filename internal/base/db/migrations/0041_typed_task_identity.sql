LOCK TABLE weave_task_queue IN ACCESS EXCLUSIVE MODE;

ALTER TABLE weave_task_queue
  ADD COLUMN identity_kind TEXT,
  ADD COLUMN identity_schema_version INT,
  ADD COLUMN execution_scope TEXT,
  ADD COLUMN workflow_id TEXT,
  ADD COLUMN workflow_version INT,
  ADD COLUMN run_snapshot_id TEXT;

UPDATE weave_task_queue
SET identity_kind = 'agent',
    identity_schema_version = 1;

ALTER TABLE weave_task_queue
  ALTER COLUMN agent DROP NOT NULL,
  ALTER COLUMN identity_kind SET NOT NULL,
  ALTER COLUMN identity_schema_version SET NOT NULL,
  ADD CONSTRAINT weave_task_queue_workspace_id_id_key
    UNIQUE (workspace_id, id),
  ADD CONSTRAINT weave_task_queue_identity_kind_check
    CHECK (identity_kind IN ('agent', 'team_workflow', 'audit')),
  ADD CONSTRAINT weave_task_queue_identity_schema_version_check
    CHECK (identity_schema_version IN (1, 2)),
  ADD CONSTRAINT weave_task_queue_execution_scope_check CHECK (
    execution_scope IS NULL
    OR execution_scope IN (
      'legacy_orchestrator',
      'team_worker_leaf',
      'team_free_collab'
    )
  ),
  ADD CONSTRAINT weave_task_queue_identity_shape_check CHECK (
    (
      identity_schema_version = 1
      AND identity_kind = 'agent'
      AND agent IS NOT NULL
      AND execution_scope IS NULL
      AND workflow_id IS NULL
      AND workflow_version IS NULL
      AND run_snapshot_id IS NULL
    )
    OR (
      identity_schema_version = 2
      AND (
        (
          identity_kind = 'agent'
          AND agent IS NOT NULL
          AND agent <> ''
          AND agent_id IS NOT NULL
          AND agent_version IS NOT NULL
          AND execution_scope IS NOT NULL
          AND workflow_id IS NULL
          AND workflow_version IS NULL
          AND run_snapshot_id IS NULL
        )
        OR (
          identity_kind = 'team_workflow'
          AND agent IS NULL
          AND agent_id IS NULL
          AND agent_version IS NULL
          AND execution_scope IS NULL
          AND workflow_id IS NOT NULL
          AND workflow_id <> ''
          AND workflow_version IS NOT NULL
          AND workflow_version > 0
          AND run_snapshot_id IS NOT NULL
          AND run_snapshot_id <> ''
        )
        OR (
          identity_kind = 'audit'
          AND agent IS NULL
          AND agent_id IS NULL
          AND agent_version IS NULL
          AND execution_scope IS NULL
          AND workflow_id IS NULL
          AND workflow_version IS NULL
          AND run_snapshot_id IS NULL
          AND status IN (
            'completed',
            'failed',
            'cancelled',
            'superseded',
            'cut',
            'timed_out'
          )
        )
      )
    )
  ),
  ADD CONSTRAINT weave_task_queue_workflow_version_fk
    FOREIGN KEY (workspace_id, workflow_id, workflow_version)
    REFERENCES weave_team_workflow_versions (
      workspace_id,
      workflow_id,
      version
    ) ON DELETE RESTRICT,
  ADD CONSTRAINT weave_task_queue_run_snapshot_fk
    FOREIGN KEY (workspace_id, run_snapshot_id)
    REFERENCES weave_team_run_snapshots (
      workspace_id,
      run_id
    ) ON DELETE RESTRICT;

CREATE FUNCTION weave_task_queue_require_current_identity()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.identity_schema_version = 1 THEN
    IF NEW.identity_kind <> 'agent'
       OR NEW.agent IS NULL
       OR NEW.agent = ''
       OR NEW.source <> 'group_result'
       OR NEW.status <> 'queued'
       OR NEW.agent_id IS NOT NULL
       OR NEW.agent_version IS NOT NULL
       OR NEW.execution_scope IS NOT NULL
       OR NEW.workflow_id IS NOT NULL
       OR NEW.workflow_version IS NOT NULL
       OR NEW.run_snapshot_id IS NOT NULL THEN
      RAISE EXCEPTION 'new schema-1 task identity is limited to queued group_result synthesis'
        USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
  END IF;

  IF NEW.identity_schema_version <> 2 THEN
    RAISE EXCEPTION 'new task identity must use schema version 2'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER weave_task_queue_current_identity_insert
BEFORE INSERT ON weave_task_queue
FOR EACH ROW
EXECUTE FUNCTION weave_task_queue_require_current_identity();

DROP TRIGGER weave_task_queue_agent_version_immutable
  ON weave_task_queue;
DROP FUNCTION weave_task_queue_reject_agent_version_rewrite();

CREATE FUNCTION weave_task_queue_reject_identity_rewrite()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'queued task identity is immutable'
    USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER weave_task_queue_identity_immutable
BEFORE UPDATE OF
  id,
  workspace_id,
  agent,
  agent_id,
  agent_version,
  identity_kind,
  identity_schema_version,
  execution_scope,
  workflow_id,
  workflow_version,
  run_snapshot_id,
  source
ON weave_task_queue
FOR EACH ROW
WHEN (
  OLD.id IS DISTINCT FROM NEW.id
  OR OLD.workspace_id IS DISTINCT FROM NEW.workspace_id
  OR OLD.agent IS DISTINCT FROM NEW.agent
  OR OLD.agent_id IS DISTINCT FROM NEW.agent_id
  OR OLD.agent_version IS DISTINCT FROM NEW.agent_version
  OR OLD.identity_kind IS DISTINCT FROM NEW.identity_kind
  OR OLD.identity_schema_version IS DISTINCT FROM NEW.identity_schema_version
  OR OLD.execution_scope IS DISTINCT FROM NEW.execution_scope
  OR OLD.workflow_id IS DISTINCT FROM NEW.workflow_id
  OR OLD.workflow_version IS DISTINCT FROM NEW.workflow_version
  OR OLD.run_snapshot_id IS DISTINCT FROM NEW.run_snapshot_id
  OR OLD.source IS DISTINCT FROM NEW.source
)
EXECUTE FUNCTION weave_task_queue_reject_identity_rewrite();
