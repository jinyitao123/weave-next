-- 0035 predates the TeamWorker compatibility boundary and could activate a
-- team whose current worker record still contains cross-agent orchestration.
-- Diagnose from authoritative current relationships, then preserve the roster
-- as repair history and only withdraw active status.
CREATE VIEW weave_team_worker_compatibility_diagnostics AS
WITH current_team_workers AS (
  SELECT
    team.workspace_id,
    team.id AS team_id,
    team_worker.worker_agent_id,
    worker.version AS agent_version,
    worker.spec
  FROM weave_teams AS team
  JOIN weave_team_workers AS team_worker
    ON team_worker.workspace_id = team.workspace_id
   AND team_worker.team_id = team.id
  JOIN weave_agents AS worker
    ON worker.workspace_id = team_worker.workspace_id
   AND worker.id = team_worker.worker_agent_id
  WHERE team.status IN ('active', 'needs_repair')
)
SELECT
  workspace_id,
  team_id,
  worker_agent_id,
  agent_version,
  'worker_has_sub_agents'::TEXT AS issue_code
FROM current_team_workers
WHERE CASE
  WHEN jsonb_typeof(spec->'sub_agents') = 'array'
    THEN jsonb_array_length(spec->'sub_agents') > 0
  ELSE false
END

UNION ALL

SELECT
  workspace_id,
  team_id,
  worker_agent_id,
  agent_version,
  'worker_sub_agents_invalid_shape'::TEXT AS issue_code
FROM current_team_workers
WHERE spec->'sub_agents' IS NOT NULL
  AND spec->'sub_agents' <> 'null'::JSONB
  AND jsonb_typeof(spec->'sub_agents') <> 'array'

UNION ALL

SELECT
  workspace_id,
  team_id,
  worker_agent_id,
  agent_version,
  'worker_graph_has_worker_node'::TEXT AS issue_code
FROM current_team_workers
WHERE CASE
  WHEN jsonb_typeof(spec #> '{graph_definition,steps}') = 'array'
    THEN EXISTS (
      SELECT 1
      FROM jsonb_array_elements(spec #> '{graph_definition,steps}') AS step
      WHERE jsonb_typeof(step) = 'object'
        AND jsonb_typeof(step->'type') = 'string'
        AND step->>'type' = 'worker'
    )
  ELSE false
END

UNION ALL

SELECT
  workspace_id,
  team_id,
  worker_agent_id,
  agent_version,
  'worker_graph_invalid_steps_shape'::TEXT AS issue_code
FROM current_team_workers
WHERE CASE
  WHEN spec->'graph_definition' IS NULL
    OR spec->'graph_definition' = 'null'::JSONB
    THEN false
  WHEN jsonb_typeof(spec->'graph_definition') <> 'object'
    THEN true
  WHEN spec #> '{graph_definition,steps}' IS NULL
    OR spec #> '{graph_definition,steps}' = 'null'::JSONB
    THEN false
  WHEN jsonb_typeof(spec #> '{graph_definition,steps}') <> 'array'
    THEN true
  ELSE EXISTS (
    SELECT 1
    FROM jsonb_array_elements(spec #> '{graph_definition,steps}') AS step
    WHERE CASE
      WHEN jsonb_typeof(step) <> 'object' THEN true
      WHEN NOT step ? 'type' THEN false
      ELSE jsonb_typeof(step->'type') <> 'string'
    END
  )
END;

UPDATE weave_teams AS team
SET status = 'needs_repair',
    updated_at = now()
FROM (
  SELECT DISTINCT workspace_id, team_id
  FROM weave_team_worker_compatibility_diagnostics
) AS incompatible
WHERE team.workspace_id = incompatible.workspace_id
  AND team.id = incompatible.team_id
  AND team.status = 'active';
