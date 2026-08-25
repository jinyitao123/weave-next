LOCK TABLE weave_projects, weave_teams, weave_agents IN ACCESS EXCLUSIVE MODE;

ALTER TABLE weave_projects
  ADD COLUMN team_id TEXT;

UPDATE weave_projects AS project
SET team_id = matched.team_id
FROM (
  SELECT project.workspace_id, project.id, MIN(team.id) AS team_id
  FROM weave_projects AS project
  JOIN weave_teams AS team
    ON team.workspace_id = project.workspace_id
   AND team.lead_avatar_id = project.avatar_id
   AND team.status = 'active'
  GROUP BY project.workspace_id, project.id
  HAVING COUNT(*) = 1
) AS matched
WHERE project.workspace_id = matched.workspace_id
  AND project.id = matched.id;

CREATE INDEX weave_projects_team_idx
  ON weave_projects (workspace_id, team_id, archived_at, updated_at DESC, id)
  WHERE team_id IS NOT NULL;

DROP INDEX IF EXISTS weave_projects_active_name_key;

CREATE UNIQUE INDEX weave_projects_active_team_name_key
  ON weave_projects (workspace_id, team_id, lower(name))
  WHERE archived_at IS NULL AND team_id IS NOT NULL;

ALTER TABLE weave_projects
  ADD CONSTRAINT weave_projects_team_fk
    FOREIGN KEY (workspace_id, team_id)
    REFERENCES weave_teams (workspace_id, id) ON DELETE RESTRICT,
  ADD CONSTRAINT weave_projects_team_required_check
    CHECK (archived_at IS NOT NULL OR team_id IS NOT NULL) NOT VALID;

DROP INDEX IF EXISTS weave_projects_system_kind_key;

CREATE UNIQUE INDEX weave_projects_unclassified_team_key
  ON weave_projects (workspace_id, team_id)
  WHERE system_kind = 'unclassified' AND archived_at IS NULL;

CREATE TABLE weave_project_collaborators (
  project_id TEXT NOT NULL,
  workspace_id TEXT NOT NULL,
  team_id TEXT NOT NULL,
  added_by TEXT NOT NULL,
  added_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  removed_at TIMESTAMPTZ,
  CONSTRAINT weave_project_collaborators_project_fk
    FOREIGN KEY (workspace_id, project_id)
    REFERENCES weave_projects (workspace_id, id) ON DELETE RESTRICT,
  CONSTRAINT weave_project_collaborators_team_fk
    FOREIGN KEY (workspace_id, team_id)
    REFERENCES weave_teams (workspace_id, id) ON DELETE RESTRICT,
  CONSTRAINT weave_project_collaborators_added_by_check
    CHECK (btrim(added_by) <> '')
);

CREATE UNIQUE INDEX weave_project_collaborators_active_key
  ON weave_project_collaborators (workspace_id, project_id, team_id)
  WHERE removed_at IS NULL;

CREATE INDEX weave_project_collaborators_project_idx
  ON weave_project_collaborators (workspace_id, project_id, removed_at, added_at DESC);

CREATE INDEX weave_project_collaborators_team_idx
  ON weave_project_collaborators (workspace_id, team_id, removed_at, added_at DESC);
