-- Read-only diagnostics for the R6 organization migration. This view keeps
-- unsafe legacy rows visible without mutating or guessing their target state.
CREATE VIEW weave_org_migration_diagnostics AS
SELECT
  team.workspace_id,
  'team_without_active_avatar'::TEXT AS issue_code,
  team.id AS team_id,
  NULL::TEXT AS agent_id,
  NULL::INT AS agent_version,
  NULL::TEXT AS link_id
FROM weave_teams AS team
WHERE NOT EXISTS (
  SELECT 1
  FROM weave_agents AS lead
  WHERE lead.workspace_id = team.workspace_id
    AND lead.team_id = team.id
    AND lead.role = 'avatar'
    AND lead.deleted = false
)

UNION ALL

SELECT
  team.workspace_id,
  'team_without_migratable_worker'::TEXT AS issue_code,
  team.id AS team_id,
  NULL::TEXT AS agent_id,
  NULL::INT AS agent_version,
  NULL::TEXT AS link_id
FROM weave_teams AS team
WHERE NOT EXISTS (
  SELECT 1
  FROM weave_agents AS lead
  JOIN weave_agent_links AS link
    ON link.workspace_id = team.workspace_id
   AND link.from_agent_id = lead.id
   AND link.type = 'manages'
  JOIN weave_agents AS worker
    ON worker.workspace_id = team.workspace_id
   AND worker.id = link.to_agent_id
   AND worker.role = 'worker'
   AND worker.deleted = false
  WHERE lead.workspace_id = team.workspace_id
    AND lead.team_id = team.id
    AND lead.role = 'avatar'
    AND lead.deleted = false
)

UNION ALL

SELECT
  worker.workspace_id,
  'team_worker_without_manages_relation'::TEXT AS issue_code,
  team.id AS team_id,
  worker.id AS agent_id,
  NULL::INT AS agent_version,
  NULL::TEXT AS link_id
FROM weave_agents AS worker
JOIN weave_teams AS team
  ON team.workspace_id = worker.workspace_id
 AND team.id = worker.team_id
WHERE worker.role = 'worker'
  AND worker.deleted = false
  AND NOT EXISTS (
    SELECT 1
    FROM weave_agents AS lead
    JOIN weave_agent_links AS link
      ON link.workspace_id = team.workspace_id
     AND link.from_agent_id = lead.id
     AND link.to_agent_id = worker.id
     AND link.type = 'manages'
    WHERE lead.workspace_id = team.workspace_id
      AND lead.team_id = team.id
      AND lead.role = 'avatar'
      AND lead.deleted = false
  )

UNION ALL

SELECT
  link.workspace_id,
  'manages_source_not_team_avatar'::TEXT AS issue_code,
  CASE
    WHEN source.workspace_id = link.workspace_id THEN source.team_id
    ELSE NULL
  END AS team_id,
  source.id AS agent_id,
  NULL::INT AS agent_version,
  link.id::TEXT AS link_id
FROM weave_agent_links AS link
LEFT JOIN weave_agents AS source ON source.id = link.from_agent_id
WHERE link.type = 'manages'
  AND (
    source.id IS NULL
    OR source.workspace_id <> link.workspace_id
    OR source.deleted = true
    OR source.role <> 'avatar'
    OR source.team_id IS NULL
    OR NOT EXISTS (
      SELECT 1
      FROM weave_teams AS source_team
      WHERE source_team.workspace_id = link.workspace_id
        AND source_team.id = source.team_id
    )
  )

UNION ALL

SELECT
  link.workspace_id,
  'manages_target_not_active_worker'::TEXT AS issue_code,
  CASE
    WHEN source.workspace_id = link.workspace_id THEN source.team_id
    ELSE NULL
  END AS team_id,
  target.id AS agent_id,
  NULL::INT AS agent_version,
  link.id::TEXT AS link_id
FROM weave_agent_links AS link
LEFT JOIN weave_agents AS source ON source.id = link.from_agent_id
LEFT JOIN weave_agents AS target ON target.id = link.to_agent_id
WHERE link.type = 'manages'
  AND (
    target.id IS NULL
    OR target.workspace_id <> link.workspace_id
    OR target.deleted = true
    OR target.role <> 'worker'
  )

UNION ALL

SELECT
  worker.workspace_id,
  'worker_has_sub_agents'::TEXT AS issue_code,
  worker.team_id,
  worker.id AS agent_id,
  version.version AS agent_version,
  NULL::TEXT AS link_id
FROM weave_agent_versions AS version
JOIN weave_agents AS worker ON worker.id = version.agent_id
WHERE worker.role = 'worker'
  AND worker.deleted = false
  AND CASE
    WHEN jsonb_typeof(version.spec->'sub_agents') = 'array'
      THEN jsonb_array_length(version.spec->'sub_agents') > 0
    ELSE false
  END

UNION ALL

SELECT
  worker.workspace_id,
  'worker_sub_agents_invalid_shape'::TEXT AS issue_code,
  worker.team_id,
  worker.id AS agent_id,
  version.version AS agent_version,
  NULL::TEXT AS link_id
FROM weave_agent_versions AS version
JOIN weave_agents AS worker ON worker.id = version.agent_id
WHERE worker.role = 'worker'
  AND worker.deleted = false
  AND version.spec->'sub_agents' IS NOT NULL
  AND version.spec->'sub_agents' <> 'null'::JSONB
  AND jsonb_typeof(version.spec->'sub_agents') <> 'array'

UNION ALL

SELECT
  worker.workspace_id,
  'worker_graph_has_worker_node'::TEXT AS issue_code,
  worker.team_id,
  worker.id AS agent_id,
  version.version AS agent_version,
  NULL::TEXT AS link_id
FROM weave_agent_versions AS version
JOIN weave_agents AS worker ON worker.id = version.agent_id
WHERE worker.role = 'worker'
  AND worker.deleted = false
  AND CASE
    WHEN jsonb_typeof(version.spec #> '{graph_definition,steps}') = 'array'
      THEN EXISTS (
        SELECT 1
        FROM jsonb_array_elements(version.spec #> '{graph_definition,steps}') AS step
        WHERE step->>'type' = 'worker'
      )
    ELSE false
  END

UNION ALL

SELECT
  worker.workspace_id,
  'worker_graph_invalid_steps_shape'::TEXT AS issue_code,
  worker.team_id,
  worker.id AS agent_id,
  version.version AS agent_version,
  NULL::TEXT AS link_id
FROM weave_agent_versions AS version
JOIN weave_agents AS worker ON worker.id = version.agent_id
WHERE worker.role = 'worker'
  AND worker.deleted = false
  AND version.spec #> '{graph_definition,steps}' IS NOT NULL
  AND version.spec #> '{graph_definition,steps}' <> 'null'::JSONB
  AND jsonb_typeof(version.spec #> '{graph_definition,steps}') <> 'array';
