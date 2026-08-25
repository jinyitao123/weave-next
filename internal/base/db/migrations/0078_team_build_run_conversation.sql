-- T08: link each TeamBuildRun control record to the chat conversation that
-- drove the build task. The column is nullable because admin-created control
-- records may exist without a conversation (e.g. an API-only flow), and no
-- existing constraint is changed. The partial index only covers non-terminal
-- runs so the conversation->active run lookup stays cheap.
ALTER TABLE weave_team_build_runs
  ADD COLUMN conversation_id TEXT;

ALTER TABLE weave_team_build_runs
  ADD CONSTRAINT weave_team_build_runs_conversation_optional_text_check CHECK (
    conversation_id IS NULL OR conversation_id <> ''
  );

CREATE INDEX weave_team_build_runs_conversation_active_idx
  ON weave_team_build_runs (workspace_id, conversation_id, created_at DESC, build_run_id DESC)
  WHERE status NOT IN ('passed', 'blocked', 'cancelled');
