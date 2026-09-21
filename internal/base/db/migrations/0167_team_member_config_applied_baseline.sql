ALTER TABLE weave_team_member_config_drafts
  DROP CONSTRAINT weave_team_member_config_drafts_revision_check;

ALTER TABLE weave_team_member_config_drafts
  ADD CONSTRAINT weave_team_member_config_drafts_revision_check
  CHECK (revision >= 0);
