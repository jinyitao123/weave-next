-- Every output a development trial produced, kept whole. The trial's activity
-- shows a bounded preview of each; a developer reads the whole body from here.
-- Outputs recorded before this table existed stay in the trial row's
-- stage_outputs index, where only their preview was kept.
-- body_state is 'stored' when body holds every byte, or 'storage_failed' when
-- the output was larger than the trial may keep (body is then NULL); the
-- second is shown as a storage failure, never as a complete output.
CREATE TABLE IF NOT EXISTS weave_team_development_trial_output_bodies (
  workspace_id   TEXT        NOT NULL,
  request_id     UUID        NOT NULL,
  output_id      TEXT        NOT NULL,
  team_id        TEXT        NOT NULL,
  actor_id       TEXT        NOT NULL,
  run_id         TEXT        NOT NULL,
  node_id        TEXT        NOT NULL,
  member_id      TEXT        NOT NULL DEFAULT '',
  node_label     TEXT        NOT NULL DEFAULT '',
  node_type      TEXT        NOT NULL DEFAULT '',
  kind           TEXT        NOT NULL,
  path           TEXT        NOT NULL DEFAULT '',
  content_type   TEXT        NOT NULL,
  content_sha256 TEXT        NOT NULL,
  content_bytes  BIGINT      NOT NULL,
  body_state     TEXT        NOT NULL,
  body           BYTEA,
  created_at     TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (workspace_id, request_id, output_id),
  CONSTRAINT weave_team_development_trial_output_bodies_state
    CHECK ((body_state = 'stored' AND body IS NOT NULL AND octet_length(body) = content_bytes)
        OR (body_state = 'storage_failed' AND body IS NULL))
);

CREATE INDEX IF NOT EXISTS weave_team_development_trial_output_bodies_run
  ON weave_team_development_trial_output_bodies (workspace_id, run_id, created_at, node_id, output_id);
