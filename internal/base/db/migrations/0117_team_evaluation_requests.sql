ALTER TABLE weave_team_build_runs
  ADD COLUMN evaluation_team_id TEXT;

ALTER TABLE weave_team_build_runs
  ADD CONSTRAINT weave_team_build_runs_evaluation_team_check CHECK (
    evaluation_team_id IS NULL
    OR (
      evaluation_team_id <> ''
      AND mode = 'optimize'
      AND execution_strategy = 'compiler_v1'
      AND brief_json->>'team_id' = evaluation_team_id
    )
  );

CREATE UNIQUE INDEX weave_team_build_runs_active_evaluation_unique_idx
  ON weave_team_build_runs (workspace_id, evaluation_team_id)
  WHERE evaluation_team_id IS NOT NULL
    AND status NOT IN ('passed', 'blocked', 'cancelled');

CREATE TABLE weave_team_evaluation_requests (
  workspace_id TEXT NOT NULL,
  idempotency_key UUID NOT NULL,
  request_fingerprint TEXT NOT NULL,
  build_run_id TEXT NOT NULL,
  team_id TEXT NOT NULL,
  created_by TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL,

  CONSTRAINT weave_team_evaluation_requests_pkey PRIMARY KEY (
    workspace_id, idempotency_key
  ),
  CONSTRAINT weave_team_evaluation_requests_identity_check CHECK (
    workspace_id <> '' AND build_run_id <> '' AND team_id <> '' AND created_by <> ''
  ),
  CONSTRAINT weave_team_evaluation_requests_fingerprint_check CHECK (
    request_fingerprint ~ '^[0-9a-f]{64}$'
  ),
  CONSTRAINT weave_team_evaluation_requests_build_run_unique UNIQUE (
    workspace_id, build_run_id
  )
);
