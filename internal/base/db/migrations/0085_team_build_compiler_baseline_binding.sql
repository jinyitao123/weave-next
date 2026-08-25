ALTER TABLE weave_team_build_blueprint_revisions
  ADD COLUMN baseline_captured_at TIMESTAMPTZ;

ALTER TABLE weave_team_build_blueprint_revisions
  ADD CONSTRAINT weave_team_build_blueprint_revisions_baseline_capture_check CHECK (
    (
      blueprint_json ->> 'mode' = 'create'
      AND baseline_captured_at IS NULL
    )
    OR (
      blueprint_json ->> 'mode' = 'optimize'
      AND baseline_captured_at IS NOT NULL
    )
  ) NOT VALID;
