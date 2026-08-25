CREATE TABLE IF NOT EXISTS weave_channels (
  workspace_id TEXT NOT NULL REFERENCES weave_workspaces(id) ON DELETE CASCADE,
  agent_id TEXT NOT NULL REFERENCES weave_agents(id) ON DELETE CASCADE,
  id TEXT NOT NULL,
  name TEXT NOT NULL CHECK (btrim(name) <> ''),
  position INT NOT NULL CHECK (position >= 0),
  is_default BOOLEAN NOT NULL DEFAULT false,
  PRIMARY KEY (workspace_id, agent_id, id),
  UNIQUE (workspace_id, agent_id, name),
  CHECK (is_default = (id = 'default')),
  CHECK (NOT is_default OR position = 0),
  CHECK (
    is_default OR id ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'
  )
);

CREATE OR REPLACE FUNCTION weave_insert_default_channel()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  INSERT INTO weave_channels (workspace_id, agent_id, id, name, position, is_default)
  VALUES (NEW.workspace_id, NEW.id, 'default', 'default', 0, true)
  ON CONFLICT DO NOTHING;
  RETURN NEW;
END;
$$;

CREATE TRIGGER weave_agents_default_channel
AFTER INSERT ON weave_agents
FOR EACH ROW
EXECUTE FUNCTION weave_insert_default_channel();

INSERT INTO weave_channels (workspace_id, agent_id, id, name, position, is_default)
SELECT workspace_id, id, 'default', 'default', 0, true
FROM weave_agents
ON CONFLICT DO NOTHING;
