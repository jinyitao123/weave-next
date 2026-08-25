ALTER TABLE weave_conversations
  ADD COLUMN root_reuse_key TEXT;

UPDATE weave_conversations
SET root_reuse_key = 'singleton'
WHERE parent_message_id IS NULL;

DROP INDEX idx_conversations_root_project_unique;

CREATE UNIQUE INDEX idx_conversations_root_project_reuse_unique
  ON weave_conversations (
    workspace_id, project_id, agent_id, user_id, channel, root_reuse_key
  )
  WHERE parent_message_id IS NULL AND root_reuse_key IS NOT NULL;
