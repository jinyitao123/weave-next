ALTER TABLE weave_task_queue
  DROP CONSTRAINT weave_task_queue_identity_shape_check;

ALTER TABLE weave_task_queue
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
          AND (
            (
              execution_scope = 'team_free_collab'
              AND run_snapshot_id IS NOT NULL
              AND run_snapshot_id <> ''
            )
            OR (
              execution_scope <> 'team_free_collab'
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
  );
