-- Existing deployments with multiple active peers in one team must resolve
-- them before applying this migration, otherwise creation of the avatar lead
-- unique index will fail. This preserves the invariant introduced by 0020.

-- 1) Backfill team peers (leads) to avatar and independent peers to worker.
UPDATE weave_agents SET role = 'avatar' WHERE role = 'peer' AND team_id IS NOT NULL;
UPDATE weave_agents SET role = 'worker' WHERE role = 'peer' AND team_id IS NULL;
UPDATE weave_agents SET spec = jsonb_set(spec, '{role}', '"avatar"')
  WHERE spec->>'role' = 'peer' AND team_id IS NOT NULL;
UPDATE weave_agents SET spec = jsonb_set(spec, '{role}', '"worker"')
  WHERE spec->>'role' = 'peer' AND team_id IS NULL;

-- 2) Preserve one active avatar lead per team.
DROP INDEX IF EXISTS uniq_weave_team_active_peer;
CREATE UNIQUE INDEX IF NOT EXISTS uniq_weave_team_active_avatar
  ON weave_agents (team_id)
  WHERE team_id IS NOT NULL AND role = 'avatar' AND deleted = false;

-- 3) Restrict new role values and default new agents to worker.
ALTER TABLE weave_agents DROP CONSTRAINT weave_agents_role_check;
ALTER TABLE weave_agents ADD CONSTRAINT weave_agents_role_check
  CHECK (role IN ('worker', 'avatar'));
ALTER TABLE weave_agents ALTER COLUMN role SET DEFAULT 'worker';
