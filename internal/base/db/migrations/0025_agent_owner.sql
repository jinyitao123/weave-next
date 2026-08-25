ALTER TABLE weave_agents
  ADD COLUMN owner_user_id TEXT NULL REFERENCES weave_users(id) ON DELETE SET NULL;
