ALTER TABLE weave_task_queue
  DROP CONSTRAINT weave_task_queue_identity_shape_check;

ALTER TABLE weave_task_queue
  DISABLE TRIGGER weave_task_queue_identity_immutable;

WITH candidate_matches AS (
  SELECT
    task.workspace_id,
    task.id AS task_id,
    run.run_snapshot_id,
    count(*) OVER (
      PARTITION BY task.workspace_id, task.id
    ) AS match_count
  FROM weave_task_queue AS task
  JOIN weave_team_runs AS run
    ON run.workspace_id = task.workspace_id
   AND task.created_at >= run.created_at
   AND task.created_at <= COALESCE(run.terminal_at, run.updated_at)
  WHERE task.identity_schema_version = 2
    AND task.identity_kind = 'agent'
    AND task.execution_scope = 'team_worker_leaf'
    AND (
      task.run_snapshot_id IS NULL
      OR task.run_snapshot_id = ''
    )
),
unique_matches AS (
  SELECT workspace_id, task_id, run_snapshot_id
  FROM candidate_matches
  WHERE match_count = 1
)
UPDATE weave_task_queue AS task
SET run_snapshot_id = unique_matches.run_snapshot_id
FROM unique_matches
WHERE task.workspace_id = unique_matches.workspace_id
  AND task.id = unique_matches.task_id;

UPDATE weave_task_queue AS task
SET execution_scope = 'legacy_orchestrator'
WHERE task.identity_schema_version = 2
  AND task.identity_kind = 'agent'
  AND task.execution_scope = 'team_worker_leaf'
  AND (
    task.run_snapshot_id IS NULL
    OR task.run_snapshot_id = ''
  )
  AND task.status IN (
    'completed',
    'failed',
    'cancelled',
    'superseded',
    'cut',
    'timed_out'
  )
  AND NOT EXISTS (
    SELECT 1
    FROM weave_team_runs AS run
    WHERE run.workspace_id = task.workspace_id
      AND task.created_at >= run.created_at
      AND task.created_at <= COALESCE(run.terminal_at, run.updated_at)
  );

ALTER TABLE weave_task_queue
  ENABLE TRIGGER weave_task_queue_identity_immutable;

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
  );
