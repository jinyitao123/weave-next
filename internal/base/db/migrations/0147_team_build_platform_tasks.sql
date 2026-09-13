-- TeamBuild keeps business progress; the platform queue owns physical execution.
ALTER TABLE weave_task_queue
 ADD COLUMN build_run_id TEXT,
 ADD COLUMN available_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 DROP CONSTRAINT weave_task_queue_identity_kind_check,
 DROP CONSTRAINT weave_task_queue_identity_shape_check;
ALTER TABLE weave_task_queue
 ADD CONSTRAINT weave_task_queue_identity_kind_check
 CHECK (identity_kind IN ('agent','team_workflow','audit','team_build')),
 ADD CONSTRAINT weave_task_queue_build_run_fk FOREIGN KEY (workspace_id,build_run_id)
 REFERENCES weave_team_build_runs(workspace_id,build_run_id) ON DELETE RESTRICT,
 ADD CONSTRAINT weave_task_queue_identity_shape_check CHECK (
 (build_run_id IS NULL AND (
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
          AND (
            (
              execution_scope IN ('team_free_collab', 'team_worker_leaf')
              AND run_snapshot_id IS NOT NULL
              AND run_snapshot_id <> ''
            )
            OR (
              execution_scope NOT IN ('team_free_collab', 'team_worker_leaf')
              AND run_snapshot_id IS NULL
            )
          )
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
  )) OR (
 identity_kind='team_build' AND identity_schema_version=2
 AND kind='team_build' AND source='team_build'
 AND build_run_id IS NOT NULL AND build_run_id<>''
 AND agent IS NULL AND agent_id IS NULL AND agent_version IS NULL
 AND execution_scope IS NULL AND workflow_id IS NULL AND workflow_version IS NULL
 AND run_snapshot_id IS NULL AND runtime_id IS NULL
 ));
CREATE UNIQUE INDEX weave_task_queue_build_run_idx
 ON weave_task_queue(workspace_id,build_run_id) WHERE identity_kind='team_build';
CREATE TRIGGER weave_task_queue_build_identity_immutable
 BEFORE UPDATE OF build_run_id,kind ON weave_task_queue
 FOR EACH ROW WHEN (OLD.build_run_id IS DISTINCT FROM NEW.build_run_id OR OLD.kind IS DISTINCT FROM NEW.kind)
 EXECUTE FUNCTION weave_task_queue_reject_identity_rewrite();
DROP TABLE weave_team_build_execution_jobs;
