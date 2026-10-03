-- Bounded decisions replace the scenario-specific game decision tables.
-- A service key is bound to one published workflow version plus a caller
-- contract (input/output JSON Schema and option/choice locations). Lanes are
-- opaque caller keys that serialise in-flight decisions. Earlier game rows are
-- dropped: their runs, deliverables and usage stay in the run ledger, and the
-- only consumer re-registers its binding with an explicit contract.
DROP TABLE IF EXISTS weave_game_decision_cancellations;
DROP TABLE IF EXISTS weave_game_decision_admissions;
DROP TABLE IF EXISTS weave_game_decision_bindings;

CREATE TABLE IF NOT EXISTS weave_decision_bindings (
  workspace_id     TEXT        NOT NULL,
  api_key_id       TEXT        NOT NULL REFERENCES weave_api_keys(id) ON DELETE CASCADE,
  team_id          TEXT        NOT NULL,
  workflow_id      TEXT        NOT NULL,
  workflow_version INTEGER     NOT NULL CHECK (workflow_version > 0),
  contract         JSONB       NOT NULL,
  contract_hash    TEXT        NOT NULL CHECK (contract_hash ~ '^[0-9a-f]{64}$'),
  created_by       TEXT        NOT NULL,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (workspace_id, api_key_id),
  FOREIGN KEY (team_id)
    REFERENCES weave_teams(id) ON DELETE CASCADE,
  FOREIGN KEY (workspace_id, workflow_id)
    REFERENCES weave_team_workflows(workspace_id, id) ON DELETE CASCADE
);

-- Admissions freeze the binding and survive API-key revocation for run audit.
-- api_key_id is historical identity here, not a reference to the live binding.
CREATE TABLE IF NOT EXISTS weave_decision_admissions (
  workspace_id       TEXT        NOT NULL,
  api_key_id         TEXT        NOT NULL,
  decision_id        TEXT        NOT NULL,
  client_request_id  UUID        NOT NULL,
  lane               TEXT        NOT NULL CHECK (lane ~ '^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$'),
  input_hash         TEXT        NOT NULL CHECK (input_hash ~ '^[0-9a-f]{64}$'),
  input_json         JSONB       NOT NULL,
  contract           JSONB       NOT NULL,
  run_id             TEXT        NOT NULL,
  task_id            TEXT        NOT NULL,
  team_id            TEXT        NOT NULL,
  workflow_id        TEXT        NOT NULL,
  workflow_version   INTEGER     NOT NULL CHECK (workflow_version > 0),
  created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (workspace_id, decision_id),
  UNIQUE (workspace_id, api_key_id, client_request_id)
);

CREATE INDEX IF NOT EXISTS idx_weave_decision_admissions_lane
  ON weave_decision_admissions(workspace_id, api_key_id, lane, created_at);
CREATE INDEX IF NOT EXISTS idx_weave_decision_admissions_run
  ON weave_decision_admissions(workspace_id, run_id);

-- A cancellation may arrive before the admission response. Keep its tombstone
-- after key revocation so a delayed request can never be admitted on retry.
CREATE TABLE IF NOT EXISTS weave_decision_cancellations (
  workspace_id      TEXT        NOT NULL,
  api_key_id        TEXT        NOT NULL,
  client_request_id UUID        NOT NULL,
  lane              TEXT        NOT NULL,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (workspace_id, api_key_id, client_request_id)
);

-- The decisions scope replaces the scenario-named scope on bound service keys.
UPDATE weave_api_keys SET scopes = ARRAY['decisions']::text[]
 WHERE scopes = ARRAY['game_decisions']::text[];
