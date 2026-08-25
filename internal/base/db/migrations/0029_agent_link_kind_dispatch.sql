ALTER TABLE weave_agent_links
DROP CONSTRAINT weave_agent_links_kind_check;

ALTER TABLE weave_agent_links
ALTER COLUMN kind SET DEFAULT 'dispatch';

UPDATE weave_agent_links
SET kind = 'dispatch'
WHERE kind = 'handoff';

ALTER TABLE weave_agent_links
ADD CONSTRAINT weave_agent_links_kind_check
CHECK (kind IN ('handoff', 'consult', 'dispatch'));
