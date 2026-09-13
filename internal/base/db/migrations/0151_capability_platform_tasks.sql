-- Capability invocations remain product records; the platform queue is the
-- only owner of their physical claim, lease, cancellation, and terminal fact.
ALTER TABLE weave_task_queue
 ADD COLUMN capability_invocation_id TEXT,
 DROP CONSTRAINT weave_task_queue_identity_kind_check,
 DROP CONSTRAINT weave_task_queue_identity_shape_check;

ALTER TABLE weave_task_queue
 ADD CONSTRAINT weave_task_queue_identity_kind_check
 CHECK (identity_kind IN ('agent','team_workflow','audit','team_build','capability')),
 ADD CONSTRAINT weave_task_queue_capability_invocation_fk
 FOREIGN KEY (workspace_id,capability_invocation_id)
 REFERENCES weave_capability_invocations(workspace_id,invocation_id) ON DELETE RESTRICT,
 ADD CONSTRAINT weave_task_queue_identity_shape_check CHECK (
 (build_run_id IS NULL AND capability_invocation_id IS NULL AND (
    (identity_schema_version=1 AND identity_kind='agent' AND agent IS NOT NULL
      AND execution_scope IS NULL AND workflow_id IS NULL AND workflow_version IS NULL AND run_snapshot_id IS NULL)
    OR (identity_schema_version=2 AND (
      (identity_kind='agent' AND agent IS NOT NULL AND agent<>'' AND agent_id IS NOT NULL AND agent_version IS NOT NULL
       AND execution_scope IS NOT NULL AND workflow_id IS NULL AND workflow_version IS NULL
       AND ((execution_scope IN ('team_free_collab','team_worker_leaf') AND run_snapshot_id IS NOT NULL AND run_snapshot_id<>'')
         OR (execution_scope NOT IN ('team_free_collab','team_worker_leaf') AND run_snapshot_id IS NULL)))
      OR (identity_kind='team_workflow' AND agent IS NULL AND agent_id IS NULL AND agent_version IS NULL
       AND execution_scope IS NULL AND workflow_id IS NOT NULL AND workflow_id<>'' AND workflow_version IS NOT NULL
       AND workflow_version>0 AND run_snapshot_id IS NOT NULL AND run_snapshot_id<>'')
      OR (identity_kind='audit' AND agent IS NULL AND agent_id IS NULL AND agent_version IS NULL
       AND execution_scope IS NULL AND workflow_id IS NULL AND workflow_version IS NULL AND run_snapshot_id IS NULL
       AND status IN ('completed','failed','cancelled','superseded','cut','timed_out'))
    )))
 ) OR (
   identity_kind='team_build' AND identity_schema_version=2 AND kind='team_build' AND source='team_build'
   AND build_run_id IS NOT NULL AND build_run_id<>'' AND capability_invocation_id IS NULL
   AND agent IS NULL AND agent_id IS NULL AND agent_version IS NULL AND execution_scope IS NULL
   AND workflow_id IS NULL AND workflow_version IS NULL AND run_snapshot_id IS NULL AND runtime_id IS NULL
 ) OR (
   identity_kind='capability' AND identity_schema_version=2
   AND kind='capability_invocation' AND source='capability'
   AND capability_invocation_id IS NOT NULL AND capability_invocation_id<>'' AND build_run_id IS NULL
   AND agent IS NULL AND agent_id IS NULL AND agent_version IS NULL AND execution_scope IS NULL
   AND workflow_id IS NULL AND workflow_version IS NULL AND run_snapshot_id IS NULL
 ));

CREATE UNIQUE INDEX weave_task_queue_capability_invocation_idx
 ON weave_task_queue(workspace_id,capability_invocation_id) WHERE identity_kind='capability';
CREATE TRIGGER weave_task_queue_capability_identity_immutable
 BEFORE UPDATE OF capability_invocation_id,kind ON weave_task_queue
 FOR EACH ROW WHEN (OLD.capability_invocation_id IS DISTINCT FROM NEW.capability_invocation_id OR OLD.kind IS DISTINCT FROM NEW.kind)
 EXECUTE FUNCTION weave_task_queue_reject_identity_rewrite();

DROP TABLE weave_capability_invocation_tasks;
