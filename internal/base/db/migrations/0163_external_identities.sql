CREATE TABLE weave_external_identities (
  issuer        TEXT NOT NULL,
  subject       TEXT NOT NULL,
  workspace_id  TEXT NOT NULL REFERENCES weave_workspaces(id),
  user_id       TEXT NOT NULL REFERENCES weave_users(id),
  created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  last_login_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (issuer, subject, workspace_id),
  UNIQUE (workspace_id, user_id),
  CHECK (issuer <> '' AND subject <> '' AND workspace_id <> '' AND user_id <> '')
);
