-- MCP clients that asked to sign people in. They are public clients: the name
-- is what a person sees when asked to allow one, and the redirect addresses
-- are the only places a sign-in may return to.
CREATE TABLE IF NOT EXISTS weave_mcp_clients (
  id            TEXT        PRIMARY KEY,
  name          TEXT        NOT NULL,
  redirect_uris JSONB       NOT NULL,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One sign-in of one person from one MCP client. A row starts when the person
-- allows the client and holds a one-time code until the client exchanges it
-- for a token; only digests of either are kept. Rows stay after a session
-- ends: they are the record of who signed in, from which client, and who
-- ended it.
CREATE TABLE IF NOT EXISTS weave_mcp_sessions (
  id              UUID        PRIMARY KEY,
  workspace_id    TEXT        NOT NULL,
  user_id         TEXT        NOT NULL,
  client_id       TEXT        NOT NULL,
  client_name     TEXT        NOT NULL,
  role            TEXT        NOT NULL,
  identity_source TEXT        NOT NULL DEFAULT '',
  permission_sets JSONB       NOT NULL DEFAULT '[]',
  code_digest     TEXT        UNIQUE,
  code_challenge  TEXT,
  redirect_uri    TEXT,
  code_expires_at TIMESTAMPTZ,
  token_digest    TEXT        UNIQUE,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_used_at    TIMESTAMPTZ,
  expires_at      TIMESTAMPTZ,
  revoked_at      TIMESTAMPTZ,
  revoked_by      TEXT
);

CREATE INDEX IF NOT EXISTS weave_mcp_sessions_owner ON weave_mcp_sessions (workspace_id, user_id);
