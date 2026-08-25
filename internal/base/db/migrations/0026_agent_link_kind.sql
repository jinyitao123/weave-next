ALTER TABLE weave_agent_links
ADD COLUMN kind TEXT NOT NULL DEFAULT 'handoff'
CHECK (kind IN ('handoff', 'consult'));
