UPDATE weave_team_build_runs
SET execution_strategy = 'compiler_v1',
    updated_at = NOW()
WHERE mode = 'create'
  AND status = 'planning'
  AND execution_strategy = 'legacy';

WITH targets AS (
  SELECT run.workspace_id, run.build_run_id, run.status
  FROM weave_team_build_runs AS run
  WHERE run.mode = 'create'
    AND run.execution_strategy = 'legacy'
    AND run.status IN ('authorized', 'round_running', 'publishing')
    AND NOT EXISTS (
      SELECT 1
      FROM weave_team_build_blueprint_revisions AS revision
      WHERE revision.workspace_id = run.workspace_id
        AND revision.build_run_id = run.build_run_id
    )
),
seqs AS (
  SELECT
    target.workspace_id,
    target.build_run_id,
    target.status,
    COALESCE(MAX(transition.seq), 0) + 1 AS next_seq
  FROM targets AS target
  LEFT JOIN weave_team_build_run_transitions AS transition
    ON transition.workspace_id = target.workspace_id
   AND transition.build_run_id = target.build_run_id
  GROUP BY target.workspace_id, target.build_run_id, target.status
),
ledger AS (
  INSERT INTO weave_team_build_run_transitions (
    workspace_id, build_run_id, seq, from_status, to_status,
    reason, actor, created_at
  )
  SELECT
    workspace_id,
    build_run_id,
    next_seq,
    status,
    'blocked',
    'execution_failed:blueprint_required: legacy create run has no persisted compiler revision',
    'migration:0099',
    NOW()
  FROM seqs
  RETURNING workspace_id, build_run_id
)
UPDATE weave_team_build_runs AS run
SET status = 'blocked',
    updated_at = NOW(),
    decided_at = NOW(),
    publish_eligible = false
FROM ledger
WHERE run.workspace_id = ledger.workspace_id
  AND run.build_run_id = ledger.build_run_id;
