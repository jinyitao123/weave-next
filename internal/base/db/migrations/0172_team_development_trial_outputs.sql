ALTER TABLE weave_team_development_trials
  ADD COLUMN stage_outputs JSONB NOT NULL DEFAULT '{}'::jsonb,
  ADD COLUMN stage_outputs_truncated BOOLEAN NOT NULL DEFAULT false,
  ADD CONSTRAINT weave_team_development_trials_stage_outputs_object
    CHECK (jsonb_typeof(stage_outputs) = 'object');
