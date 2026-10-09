-- Pull requests opened from admin console tasks. A task's result branch is
-- shared by its follow-ups, so one pull request is kept per environment branch.
CREATE TABLE IF NOT EXISTS weave_environment_pull_requests (
  workspace_id   TEXT        NOT NULL,
  environment_id TEXT        NOT NULL,
  branch         TEXT        NOT NULL CHECK (btrim(branch) <> ''),
  url            TEXT        NOT NULL CHECK (url LIKE 'https://%' OR url LIKE 'http://%'),
  number         BIGINT      NOT NULL CHECK (number > 0),
  created_by     TEXT        NOT NULL,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (workspace_id, environment_id, branch)
);
