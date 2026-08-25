ALTER TABLE weave_teams
  DROP CONSTRAINT weave_teams_status_check,
  ADD CONSTRAINT weave_teams_status_check
    CHECK (status IN ('active', 'archived', 'needs_repair', 'building'));

ALTER TABLE weave_teams
  DROP CONSTRAINT weave_teams_active_lead_check,
  ADD CONSTRAINT weave_teams_active_lead_check
    CHECK (status NOT IN ('active', 'building') OR lead_avatar_id IS NOT NULL);

