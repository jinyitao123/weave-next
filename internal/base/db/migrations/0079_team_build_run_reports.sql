-- T10: immutable per-round evaluation reports for team build runs.
--
-- Each round's EvaluationReport is content-addressed exactly like the frozen
-- publication candidates: the round ledger's report_ref must resolve to a row
-- here, and the row can never be updated or deleted once inserted. The round
-- controller writes the report row and the round ledger row in one
-- transaction, so report_ref is always resolvable.

CREATE TABLE weave_team_build_run_reports (
  workspace_id TEXT NOT NULL,
  build_run_id TEXT NOT NULL,
  round_no INTEGER NOT NULL,
  report_hash TEXT NOT NULL,
  report_json JSONB NOT NULL,
  created_at TIMESTAMPTZ NOT NULL,

  CONSTRAINT weave_team_build_run_reports_pkey PRIMARY KEY (
    workspace_id, build_run_id, round_no
  ),
  CONSTRAINT weave_team_build_run_reports_run_fkey FOREIGN KEY (
    workspace_id, build_run_id
  ) REFERENCES weave_team_build_runs (
    workspace_id, build_run_id
  ) ON DELETE RESTRICT,
  CONSTRAINT weave_team_build_run_reports_identity_check CHECK (
    workspace_id <> '' AND build_run_id <> '' AND round_no > 0
  ),
  CONSTRAINT weave_team_build_run_reports_hash_format_check CHECK (
    report_hash ~ '^[0-9a-f]{64}$'
  ),
  CONSTRAINT weave_team_build_run_reports_json_shape_check CHECK (
    jsonb_typeof(report_json) = 'object'
    AND report_json->>'round_no' = round_no::text
    AND report_json->>'conclusion' IN ('pass', 'revise', 'blocked')
  )
);

CREATE INDEX weave_team_build_run_reports_hash_idx
  ON weave_team_build_run_reports (workspace_id, build_run_id, report_hash);

CREATE FUNCTION weave_team_build_run_reports_reject_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'team build run report rows are immutable'
    USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER weave_team_build_run_reports_immutable
BEFORE UPDATE OR DELETE ON weave_team_build_run_reports
FOR EACH ROW
EXECUTE FUNCTION weave_team_build_run_reports_reject_mutation();
