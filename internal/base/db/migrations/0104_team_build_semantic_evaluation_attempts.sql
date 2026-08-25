-- One recoverable semantic evaluator execution per immutable build revision.
-- The platform records the evidence-package hash and evaluator run before
-- graph execution, then fills the output exactly once before the terminal
-- marker. Controller recovery therefore reuses the same judgment instead of
-- silently running or charging a second judge.
CREATE TABLE weave_team_build_semantic_evaluation_attempts (
  workspace_id TEXT NOT NULL,
  build_run_id TEXT NOT NULL,
  revision_no INTEGER NOT NULL,
  source_role TEXT NOT NULL,
  evidence_hash TEXT NOT NULL,
  source_run_id TEXT NOT NULL,
  output_text TEXT,
  output_hash TEXT,
  created_at TIMESTAMPTZ NOT NULL,
  output_recorded_at TIMESTAMPTZ,

  CONSTRAINT weave_team_build_semantic_evaluation_attempts_pkey PRIMARY KEY (
    workspace_id, build_run_id, revision_no, source_role
  ),
  CONSTRAINT weave_team_build_semantic_evaluation_attempts_source_run_key UNIQUE (
    workspace_id, source_run_id
  ),
  CONSTRAINT weave_team_build_semantic_evaluation_attempts_run_fkey FOREIGN KEY (
    workspace_id, build_run_id
  ) REFERENCES weave_team_build_runs (workspace_id, build_run_id) ON DELETE RESTRICT,
  CONSTRAINT weave_team_build_semantic_evaluation_attempts_identity_check CHECK (
    workspace_id <> '' AND build_run_id <> '' AND revision_no > 0
    AND source_role = 'semantic_judge' AND source_run_id <> ''
    AND evidence_hash ~ '^[0-9a-f]{64}$'
  ),
  CONSTRAINT weave_team_build_semantic_evaluation_attempts_output_check CHECK (
    (output_text IS NULL AND output_hash IS NULL AND output_recorded_at IS NULL)
    OR
    (output_text IS NOT NULL AND output_hash ~ '^[0-9a-f]{64}$' AND output_recorded_at IS NOT NULL)
  )
);
