ALTER TABLE weave_team_build_runs
  DROP CONSTRAINT weave_team_build_runs_execution_strategy_check,
  ADD CONSTRAINT weave_team_build_runs_execution_strategy_check CHECK (
    execution_strategy IN ('legacy', 'compiler_v1', 'template_instantiate')
  );

ALTER TABLE weave_team_build_runs
  DROP CONSTRAINT weave_team_build_runs_publish_eligible_check,
  ADD CONSTRAINT weave_team_build_runs_publish_eligible_check CHECK (
    publish_eligible = (
      status = 'publishing'
      OR (status = 'passed' AND execution_strategy <> 'template_instantiate')
    )
  );
