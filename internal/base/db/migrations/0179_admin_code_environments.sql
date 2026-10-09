-- Admin console environments: the repository a team works on, the commands
-- that verify its result, and the immutable code context of each run that was
-- submitted with an environment. Runtime Hosts fetch repositories with their
-- own git credentials; no repository secret is stored here.
CREATE TABLE IF NOT EXISTS weave_environments (
  workspace_id    TEXT        NOT NULL REFERENCES weave_workspaces(id) ON DELETE CASCADE,
  id              TEXT        NOT NULL,
  name            TEXT        NOT NULL CHECK (btrim(name) <> '' AND length(name) <= 80),
  repository_url  TEXT        NOT NULL CHECK (btrim(repository_url) <> '' AND length(repository_url) <= 1024),
  default_branch  TEXT        NOT NULL CHECK (btrim(default_branch) <> '' AND length(default_branch) <= 200),
  setup_script    TEXT        NOT NULL DEFAULT '' CHECK (length(setup_script) <= 8000),
  verify_commands JSONB       NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(verify_commands) = 'array'),
  push_branches   BOOLEAN     NOT NULL DEFAULT false,
  default_team_id TEXT,
  created_by      TEXT        NOT NULL,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  archived_at     TIMESTAMPTZ,
  PRIMARY KEY (workspace_id, id)
);

CREATE UNIQUE INDEX IF NOT EXISTS weave_environments_active_name
  ON weave_environments (workspace_id, name) WHERE archived_at IS NULL;

CREATE TABLE IF NOT EXISTS weave_run_code_contexts (
  workspace_id    TEXT        NOT NULL,
  run_id          TEXT        NOT NULL,
  environment_id  TEXT        NOT NULL,
  requested_ref   TEXT        NOT NULL DEFAULT '',
  repository_url  TEXT        NOT NULL CHECK (btrim(repository_url) <> ''),
  ref             TEXT        NOT NULL CHECK (btrim(ref) <> ''),
  setup_script    TEXT        NOT NULL DEFAULT '',
  verify_commands JSONB       NOT NULL CHECK (jsonb_typeof(verify_commands) = 'array'),
  push_branch     TEXT        NOT NULL DEFAULT '',
  created_by      TEXT        NOT NULL,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (workspace_id, run_id)
);

-- A run's code context is frozen when the task is submitted.
CREATE OR REPLACE FUNCTION weave_run_code_contexts_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'run code context is immutable' USING ERRCODE = '23514';
END;
$$;

DROP TRIGGER IF EXISTS weave_run_code_contexts_immutable ON weave_run_code_contexts;
CREATE TRIGGER weave_run_code_contexts_immutable
  BEFORE UPDATE ON weave_run_code_contexts
  FOR EACH ROW EXECUTE FUNCTION weave_run_code_contexts_immutable();
