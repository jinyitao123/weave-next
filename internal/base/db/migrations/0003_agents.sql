-- Agent records in loom_store are intentionally not backfilled. This rebuild has no production agent data; agents are seeded or recreated.
CREATE TABLE IF NOT EXISTS weave_agents (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES weave_workspaces(id) ON DELETE CASCADE,
  team_id TEXT REFERENCES weave_teams(id) ON DELETE SET NULL,
  name TEXT NOT NULL,
  display_name TEXT NOT NULL DEFAULT '',
  role TEXT NOT NULL DEFAULT 'peer' CHECK (role IN ('peer','worker')),
  spec JSONB NOT NULL,
  version INT NOT NULL DEFAULT 1,
  deleted BOOLEAN NOT NULL DEFAULT false,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (workspace_id, name)
);

CREATE TABLE IF NOT EXISTS weave_agent_versions (
  agent_id TEXT NOT NULL REFERENCES weave_agents(id) ON DELETE CASCADE,
  version INT NOT NULL,
  spec JSONB NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (agent_id, version)
);

CREATE TABLE IF NOT EXISTS weave_agent_links (
  workspace_id TEXT NOT NULL REFERENCES weave_workspaces(id) ON DELETE CASCADE,
  from_agent_id TEXT NOT NULL REFERENCES weave_agents(id) ON DELETE CASCADE,
  to_agent_id TEXT NOT NULL REFERENCES weave_agents(id) ON DELETE CASCADE,
  type TEXT NOT NULL DEFAULT 'manages',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, from_agent_id, to_agent_id, type)
);
