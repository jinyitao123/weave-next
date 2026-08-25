ALTER TABLE weave_team_build_runs
  DROP CONSTRAINT weave_team_build_runs_baseline_shape_check;

ALTER TABLE weave_team_build_runs
  ADD CONSTRAINT weave_team_build_runs_baseline_shape_check CHECK (
    (
      status IN ('cancelled', 'blocked')
      AND (
        (mode = 'create' AND baseline_snapshot_json IS NULL)
        OR mode = 'optimize'
      )
    )
    OR (
      status NOT IN ('cancelled', 'blocked')
      AND (mode = 'optimize' AND status <> 'planning') = (
        baseline_snapshot_json IS NOT NULL
      )
    )
  );
