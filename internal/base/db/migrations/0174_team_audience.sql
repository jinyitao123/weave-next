-- Decision 002 (weave-workbench): which employees may use a team is decided by
-- Forge permission sets. The team development document declares them and
-- publishing freezes them here; an empty array means the whole organization.
ALTER TABLE weave_teams
  ADD COLUMN audience JSONB NOT NULL DEFAULT '[]'::jsonb
  CHECK (jsonb_typeof(audience) = 'array' AND jsonb_array_length(audience) <= 32);
