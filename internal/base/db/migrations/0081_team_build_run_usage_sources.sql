-- T14B-1: append-only runtime usage source attribution for team build runs.
--
-- Every cost source (a built-in Loom meta-team employee run in Build) is
-- durably associated with its runtime-owned run id BEFORE the graph or any
-- LLM/tool executes. The row is the crash-recovery bridge between expected
-- run admission (weave_run_attempt_leases) and the T14A budget ledger:
-- expected run admission + usage source association happen before the graph,
-- the terminal marker is written after the graph, and a crash between the
-- marker and the ledger write is recovered by reconciling rows here against
-- weave_run_terminal_markers. The ledger row itself remains the single
-- source of truth for budget consumption; this table only records the
-- association fact and never carries usage numbers, so it cannot be replaced
-- by a zero-value ledger entry.

CREATE TABLE weave_team_build_run_usage_sources (
  workspace_id TEXT NOT NULL,
  build_run_id TEXT NOT NULL,
  round_no INTEGER NOT NULL,
  source_kind TEXT NOT NULL,
  source_role TEXT NOT NULL,
  source_run_id TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL,

  CONSTRAINT weave_team_build_run_usage_sources_pkey PRIMARY KEY (
    workspace_id, build_run_id, round_no, source_kind, source_run_id
  ),
  CONSTRAINT weave_team_build_run_usage_sources_run_fkey FOREIGN KEY (
    workspace_id, build_run_id
  ) REFERENCES weave_team_build_runs (
    workspace_id, build_run_id
  ) ON DELETE RESTRICT,
  CONSTRAINT weave_team_build_run_usage_sources_identity_check CHECK (
    workspace_id <> ''
    AND build_run_id <> ''
    AND round_no > 0
    AND source_kind <> ''
    AND source_role <> ''
    AND source_run_id <> ''
  )
);

-- Round scan index: reconcile and the not-yet-charged lookup both read every
-- source of one (workspace, run, round) key.
CREATE INDEX weave_team_build_run_usage_sources_round_idx
  ON weave_team_build_run_usage_sources (
    workspace_id, build_run_id, round_no
  );

CREATE FUNCTION weave_team_build_run_usage_sources_reject_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'team build run usage source rows are immutable'
    USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER weave_team_build_run_usage_sources_immutable
BEFORE UPDATE OR DELETE ON weave_team_build_run_usage_sources
FOR EACH ROW
EXECUTE FUNCTION weave_team_build_run_usage_sources_reject_mutation();
