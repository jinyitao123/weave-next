ALTER TABLE weave_team_build_runs
  DROP CONSTRAINT weave_team_build_runs_planning_shape_check;

ALTER TABLE weave_team_build_runs
  ADD CONSTRAINT weave_team_build_runs_planning_shape_check CHECK (
    status = 'cancelled'
    OR (status = 'planning') = (confirmed_by IS NULL)
  );
