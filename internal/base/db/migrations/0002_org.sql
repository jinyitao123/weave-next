CREATE TABLE IF NOT EXISTS weave_workspaces (
  id TEXT PRIMARY KEY,
  slug TEXT NOT NULL UNIQUE,
  name TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS weave_members (
  workspace_id TEXT NOT NULL REFERENCES weave_workspaces(id) ON DELETE CASCADE,
  user_id TEXT NOT NULL,
  role TEXT NOT NULL CHECK (role IN ('owner','member')),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, user_id)
);

CREATE TABLE IF NOT EXISTS weave_teams (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES weave_workspaces(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (workspace_id, name)
);

INSERT INTO weave_workspaces (id, slug, name)
SELECT DISTINCT tenant_id, tenant_id, tenant_id
FROM weave_users
ON CONFLICT DO NOTHING;

INSERT INTO weave_members (workspace_id, user_id, role)
SELECT tenant_id, id, CASE WHEN role = 'admin' THEN 'owner' ELSE 'member' END
FROM weave_users
ON CONFLICT DO NOTHING;
