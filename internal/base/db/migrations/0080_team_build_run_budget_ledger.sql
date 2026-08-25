-- T14A: append-only per-source budget ledger for team build runs.
--
-- Every cost source (a meta-team employee run in Build, a candidate test
-- run in Evaluate, or any future infrastructure usage) appends exactly one
-- row keyed by its own identity (round_no, source_kind, source_run_id).
-- The ledger is the single source of truth for round and total budget
-- consumption; build_run rows never carry drift-prone cumulative columns.
-- T14B owns production usage attribution; this migration only provides the
-- durable ledger and its hard invariants.

CREATE TABLE weave_team_build_run_budget_ledger (
  workspace_id TEXT NOT NULL,
  build_run_id TEXT NOT NULL,
  round_no INTEGER NOT NULL,
  source_kind TEXT NOT NULL,
  source_role TEXT NOT NULL,
  source_run_id TEXT NOT NULL,
  input_tokens BIGINT NOT NULL,
  output_tokens BIGINT NOT NULL,
  cost_usd DOUBLE PRECISION NOT NULL,
  tool_calls BIGINT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL,

  CONSTRAINT weave_team_build_run_budget_ledger_pkey PRIMARY KEY (
    workspace_id, build_run_id, round_no, source_kind, source_run_id
  ),
  CONSTRAINT weave_team_build_run_budget_ledger_run_fkey FOREIGN KEY (
    workspace_id, build_run_id
  ) REFERENCES weave_team_build_runs (
    workspace_id, build_run_id
  ) ON DELETE RESTRICT,
  CONSTRAINT weave_team_build_run_budget_ledger_identity_check CHECK (
    workspace_id <> ''
    AND build_run_id <> ''
    AND round_no > 0
    AND source_kind <> ''
    AND source_role <> ''
    AND source_run_id <> ''
  ),
  CONSTRAINT weave_team_build_run_budget_ledger_usage_check CHECK (
    input_tokens >= 0
    AND output_tokens >= 0
    AND tool_calls >= 0
    AND cost_usd >= 0
    AND cost_usd <> 'Infinity'::double precision
    AND cost_usd <> '-Infinity'::double precision
    AND cost_usd <> 'NaN'::double precision
  )
);

-- Round aggregation index: every budget gate reads one round's usage plus
-- the whole-run total, both starting from this (workspace, run, round) key.
CREATE INDEX weave_team_build_run_budget_ledger_round_idx
  ON weave_team_build_run_budget_ledger (
    workspace_id, build_run_id, round_no
  );

CREATE FUNCTION weave_team_build_run_budget_ledger_reject_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'team build run budget ledger is append-only'
    USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER weave_team_build_run_budget_ledger_append_only
BEFORE UPDATE OR DELETE ON weave_team_build_run_budget_ledger
FOR EACH ROW
EXECUTE FUNCTION weave_team_build_run_budget_ledger_reject_mutation();
