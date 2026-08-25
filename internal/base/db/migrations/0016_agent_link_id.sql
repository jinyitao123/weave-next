ALTER TABLE weave_agent_links
ADD COLUMN IF NOT EXISTS id UUID NOT NULL DEFAULT gen_random_uuid();

CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_links_id
ON weave_agent_links (id);
