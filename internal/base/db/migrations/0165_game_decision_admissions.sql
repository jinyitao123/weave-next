CREATE TABLE IF NOT EXISTS weave_game_decision_bindings (
  workspace_id     TEXT        NOT NULL,
  api_key_id       TEXT        NOT NULL REFERENCES weave_api_keys(id) ON DELETE CASCADE,
  team_id          TEXT        NOT NULL,
  workflow_id      TEXT        NOT NULL,
  workflow_version INTEGER     NOT NULL CHECK (workflow_version > 0),
  created_by       TEXT        NOT NULL,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (workspace_id, api_key_id),
  FOREIGN KEY (team_id)
    REFERENCES weave_teams(id) ON DELETE CASCADE,
  FOREIGN KEY (workspace_id, workflow_id)
    REFERENCES weave_team_workflows(workspace_id, id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS weave_game_decision_admissions (
  workspace_id       TEXT        NOT NULL,
  api_key_id         TEXT        NOT NULL,
  decision_id        TEXT        NOT NULL,
  client_request_id  UUID        NOT NULL,
  input_hash         TEXT        NOT NULL CHECK (input_hash ~ '^[0-9a-f]{64}$'),
  input_json         JSONB       NOT NULL,
  run_id             TEXT        NOT NULL,
  task_id            TEXT        NOT NULL,
  team_id            TEXT        NOT NULL,
  workflow_id        TEXT        NOT NULL,
  workflow_version   INTEGER     NOT NULL CHECK (workflow_version > 0),
  created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (workspace_id, decision_id),
  UNIQUE (workspace_id, api_key_id, client_request_id),
  FOREIGN KEY (workspace_id, api_key_id)
    REFERENCES weave_game_decision_bindings(workspace_id, api_key_id) ON DELETE RESTRICT
);

CREATE INDEX IF NOT EXISTS idx_weave_game_decision_admissions_run
  ON weave_game_decision_admissions(workspace_id, run_id);
