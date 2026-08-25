ALTER TABLE weave_team_build_runs
  DROP CONSTRAINT weave_team_build_runs_planning_shape_check;

ALTER TABLE weave_team_build_runs
  ADD CONSTRAINT weave_team_build_runs_planning_shape_check CHECK (
    status IN ('cancelled', 'blocked')
    OR (status = 'planning') = (confirmed_by IS NULL)
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
      AND to_status IN ('authorized', 'publishing', 'blocked', 'cancelled')
    )
    OR (
      from_status = 'publishing'
      AND to_status IN ('passed', 'blocked', 'cancelled')
    )
  );
