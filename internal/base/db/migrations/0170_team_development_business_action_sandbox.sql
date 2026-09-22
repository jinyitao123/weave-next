ALTER TABLE weave_team_development_trials
  ADD COLUMN business_actions JSONB NOT NULL DEFAULT '[]'::jsonb;
