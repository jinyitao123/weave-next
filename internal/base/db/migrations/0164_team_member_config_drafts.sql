CREATE TABLE weave_team_member_config_drafts (
  workspace_id TEXT NOT NULL REFERENCES weave_workspaces(id) ON DELETE CASCADE,
  team_id TEXT NOT NULL REFERENCES weave_teams(id) ON DELETE CASCADE,
  agent_id TEXT NOT NULL REFERENCES weave_agents(id) ON DELETE CASCADE,
  base_agent_version INT NOT NULL CHECK (base_agent_version > 0),
  revision INT NOT NULL CHECK (revision > 0),
  configuration JSONB NOT NULL,
  relationship JSONB NOT NULL,
  updated_by TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, team_id, agent_id)
);

CREATE INDEX weave_team_member_config_drafts_team_idx
  ON weave_team_member_config_drafts(workspace_id, team_id, updated_at DESC);
