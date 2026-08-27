ALTER TABLE weave_run_terminal_markers
  ADD COLUMN usage_tool_calls BIGINT NOT NULL DEFAULT 0,
  ADD CONSTRAINT weave_run_terminal_markers_usage_tool_calls_check
    CHECK (usage_tool_calls >= 0);

-- Existing marker and team-build ledger rows intentionally remain zero.
-- Only terminal records produced after this ABI extension carry measured
-- tool-call counts; append-only historical usage is never backfilled.
