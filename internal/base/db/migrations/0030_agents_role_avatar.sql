ALTER TABLE weave_agents
DROP CONSTRAINT weave_agents_role_check;

ALTER TABLE weave_agents
ADD CONSTRAINT weave_agents_role_check
CHECK (role IN ('peer', 'worker', 'avatar'));
