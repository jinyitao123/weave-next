-- Complete, line-oriented logs of one physical execution: the member's public
-- output and the output of Host-run commands. Unlike the bounded public
-- progress projection, every line is kept up to a per-task ceiling, so the
-- console can page and search the whole log. Lines are idempotent by stream
-- and sequence.
CREATE TABLE IF NOT EXISTS weave_task_logs (
  workspace_id TEXT        NOT NULL,
  task_id      TEXT        NOT NULL,
  stream       TEXT        NOT NULL CHECK (stream ~ '^[a-z][a-z0-9_:-]{0,39}$'),
  seq          BIGINT      NOT NULL CHECK (seq > 0),
  occurred_at  TIMESTAMPTZ NOT NULL,
  text         TEXT        NOT NULL CHECK (length(text) <= 8192),
  PRIMARY KEY (workspace_id, task_id, stream, seq)
);
