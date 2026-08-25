CREATE TABLE IF NOT EXISTS weave_mcp_servers (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES weave_workspaces(id) ON DELETE CASCADE,
  slug TEXT NOT NULL,
  display_name TEXT NOT NULL,
  transport TEXT NOT NULL CHECK (transport IN ('streamable_http','stdio')),
  url TEXT,
  command TEXT,
  args JSONB NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(args) = 'array'),
  headers_cipher TEXT NOT NULL,
  env_cipher TEXT NOT NULL,
  enabled BOOLEAN NOT NULL DEFAULT true,
  status TEXT NOT NULL DEFAULT 'unknown' CHECK (status IN ('unknown','online','offline')),
  protocol_version TEXT NOT NULL DEFAULT '',
  server_info JSONB NOT NULL DEFAULT '{}',
  last_error TEXT NOT NULL DEFAULT '',
  last_probed_at TIMESTAMPTZ,
  last_handshake_at TIMESTAMPTZ,
  created_by TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  deleted_at TIMESTAMPTZ,
  UNIQUE (workspace_id, slug),
  CHECK (
    (transport = 'streamable_http' AND url IS NOT NULL AND url <> '' AND command IS NULL)
    OR
    (transport = 'stdio' AND url IS NULL AND command IS NOT NULL AND command <> '')
  )
);

CREATE INDEX IF NOT EXISTS idx_mcp_servers_workspace
  ON weave_mcp_servers (workspace_id, slug)
  WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS weave_mcp_tools (
  server_id TEXT NOT NULL REFERENCES weave_mcp_servers(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  input_schema JSONB NOT NULL DEFAULT '{}',
  annotations JSONB NOT NULL DEFAULT '{}',
  read_only_hint BOOLEAN,
  discovered_at TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (server_id, name)
);
