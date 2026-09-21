-- A cancellation may arrive before the admission response. Retain a tombstone
-- so a delayed retry of that exact request can never create a new model run.
CREATE TABLE IF NOT EXISTS weave_game_decision_cancellations (
  workspace_id TEXT NOT NULL,
  api_key_id TEXT NOT NULL,
  client_request_id UUID NOT NULL,
  room_id TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (workspace_id, api_key_id, client_request_id),
  FOREIGN KEY (workspace_id, api_key_id)
    REFERENCES weave_game_decision_bindings(workspace_id, api_key_id) ON DELETE RESTRICT
);
