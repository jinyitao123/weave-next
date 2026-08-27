ALTER TABLE weave_team_build_runs
  DROP CONSTRAINT weave_team_build_runs_brief_derived_check;

-- Budget reauthorization deliberately preserves the frozen brief and its
-- hash. The effective budget columns may therefore differ from the original
-- brief after a budget_exhausted recovery.
ALTER TABLE weave_team_build_runs
  ADD CONSTRAINT weave_team_build_runs_brief_derived_check CHECK (
    brief_json->>'mode' = mode
    AND asset_scope_json = brief_json->'allowed_assets'
  );

ALTER TABLE weave_team_build_run_transitions
  DROP CONSTRAINT weave_team_build_run_transitions_allowed_check;

ALTER TABLE weave_team_build_run_transitions
  ADD CONSTRAINT weave_team_build_run_transitions_allowed_check CHECK (
    (from_status IS NULL AND to_status = 'planning')
    OR (
      from_status = 'planning'
      AND to_status IN ('authorized', 'blocked', 'cancelled')
    )
    OR (
      from_status = 'authorized'
      AND to_status IN ('round_running', 'blocked', 'cancelled')
    )
    OR (
      from_status = 'round_running'
      AND to_status IN ('authorized', 'publishing', 'passed', 'blocked', 'cancelled')
    )
    OR (
      from_status = 'publishing'
      AND to_status IN ('passed', 'blocked', 'cancelled')
    )
    OR (from_status = 'blocked' AND to_status = 'authorized')
  );

ALTER TABLE weave_team_build_operation_attempts
  DROP CONSTRAINT weave_team_build_operation_attempts_terminal_check;

-- RetryOperationStep now recognizes the existing budget_exhausted failure
-- class. Attempts remain immutable after this running -> retryable_failed
-- transition, exactly like compiler and runtime infrastructure retries.
ALTER TABLE weave_team_build_operation_attempts
  ADD CONSTRAINT weave_team_build_operation_attempts_terminal_check CHECK (
    (
      status = 'running'
      AND completed_at IS NULL
      AND error_class IS NULL
      AND error_code IS NULL
      AND evidence_json IS NULL
      AND output_hash IS NULL
    )
    OR (
      status IN ('succeeded', 'skipped')
      AND completed_at IS NOT NULL
      AND error_class IS NULL
      AND error_code IS NULL
      AND evidence_json IS NOT NULL
      AND output_hash IS NOT NULL
    )
    OR (
      status = 'retryable_failed'
      AND completed_at IS NOT NULL
      AND error_class IN (
        'compile_failure',
        'runtime_infrastructure_failure',
        'budget_exhausted'
      )
      AND error_code IS NOT NULL
      AND error_code <> ''
      AND evidence_json IS NOT NULL
    )
    OR (
      status = 'failed'
      AND completed_at IS NOT NULL
      AND error_class IS NOT NULL
      AND error_class <> ''
      AND error_code IS NOT NULL
      AND error_code <> ''
      AND evidence_json IS NOT NULL
    )
  );
