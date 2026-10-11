-- Starter teams created on request. A caller that lost the response sends the
-- same request again and gets the same team; the row also records who asked.
CREATE TABLE IF NOT EXISTS weave_team_development_creations (
  workspace_id TEXT        NOT NULL,
  request_id   UUID        NOT NULL,
  digest       TEXT        NOT NULL,
  actor_id     TEXT        NOT NULL,
  team_id      TEXT,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, request_id)
);
