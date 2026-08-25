CREATE TABLE IF NOT EXISTS weave_team_dispatch_rules (
  team_id            TEXT PRIMARY KEY REFERENCES weave_teams(id) ON DELETE CASCADE,
  leg_timeout_sec    INT NOT NULL DEFAULT 180,
  group_deadline_sec INT NOT NULL DEFAULT 480,
  quorum             INT NOT NULL DEFAULT 0
);
