-- Existing deployments must resolve multiple active peers in one team before
-- applying this migration, otherwise the unique index creation will fail.
CREATE UNIQUE INDEX IF NOT EXISTS uniq_weave_team_active_peer
  ON weave_agents (team_id)
  WHERE team_id IS NOT NULL AND role = 'peer' AND deleted = false;

-- Manages links intentionally remain many-to-many so workers can be shared.
