-- Saved groups of controlled edits to a team draft. A caller that lost the
-- response sends the same request again and gets the first outcome back; the
-- row is also the record of who changed the draft. What was written is not kept.
CREATE TABLE IF NOT EXISTS weave_team_development_requests (
  workspace_id    TEXT        NOT NULL,
  team_id         TEXT        NOT NULL,
  request_id      UUID        NOT NULL,
  digest          TEXT        NOT NULL,
  actor_id        TEXT        NOT NULL,
  revision_before BIGINT      NOT NULL,
  revision_after  BIGINT      NOT NULL,
  kinds           JSONB       NOT NULL,
  changes         JSONB       NOT NULL,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, team_id, request_id)
);
