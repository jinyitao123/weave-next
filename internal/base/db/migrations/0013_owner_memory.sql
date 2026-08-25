CREATE TABLE IF NOT EXISTS agent_memory_profile (
  workspace_id TEXT NOT NULL REFERENCES weave_workspaces(id) ON DELETE CASCADE,
  agent_id     TEXT NOT NULL REFERENCES weave_agents(id) ON DELETE CASCADE,
  user_id      TEXT NOT NULL,
  slots        JSONB NOT NULL DEFAULT '{}',
  version      INT  NOT NULL DEFAULT 0,
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, agent_id, user_id)
);
