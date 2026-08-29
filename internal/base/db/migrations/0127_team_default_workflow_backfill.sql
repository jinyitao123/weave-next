-- Existing teams can adopt a default without guessing across identities only
-- when exactly one active workflow currently has a published version. Teams
-- with zero or multiple eligible workflows remain active and surface
-- no_default_workflow until an operator chooses one explicitly.
WITH eligible AS (
  SELECT workspace_id, team_id, MIN(id) AS workflow_id
  FROM weave_team_workflows
  WHERE status='active' AND published_version IS NOT NULL
  GROUP BY workspace_id, team_id
  HAVING COUNT(*)=1
)
UPDATE weave_teams AS team
SET default_workflow_id=eligible.workflow_id,
    updated_at=now()
FROM eligible
WHERE team.workspace_id=eligible.workspace_id
  AND team.id=eligible.team_id
  AND team.default_workflow_id IS NULL;
