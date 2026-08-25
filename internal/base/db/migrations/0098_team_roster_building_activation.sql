ALTER TABLE weave_team_roster_audits
  DROP CONSTRAINT weave_team_roster_audits_status_check,
  ADD CONSTRAINT weave_team_roster_audits_status_check CHECK (
    old_team_status IN ('active', 'archived', 'needs_repair', 'building')
    AND new_team_status IN ('active', 'archived')
  );
