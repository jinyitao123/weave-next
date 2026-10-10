-- Who opened the content of a team run that someone else started. Only the
-- fact of the view is kept, never what was read.
CREATE TABLE IF NOT EXISTS weave_run_view_audit (
  workspace_id TEXT        NOT NULL,
  run_id       TEXT        NOT NULL,
  viewer_id    TEXT        NOT NULL,
  viewer_role  TEXT        NOT NULL,
  surface      TEXT        NOT NULL,
  viewed_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS weave_run_view_audit_run_idx ON weave_run_view_audit(workspace_id, run_id, viewed_at DESC);
