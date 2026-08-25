ALTER TABLE weave_conversations
  ADD COLUMN IF NOT EXISTS parent_message_id TEXT REFERENCES weave_messages(id) ON DELETE CASCADE;

ALTER TABLE weave_conversations
  ADD COLUMN IF NOT EXISTS thread_title TEXT NOT NULL DEFAULT '';

ALTER TABLE weave_conversations
  DROP CONSTRAINT IF EXISTS weave_conversations_workspace_id_agent_id_user_id_channel_key;

CREATE UNIQUE INDEX IF NOT EXISTS idx_conversations_root_unique
  ON weave_conversations (workspace_id, agent_id, user_id, channel)
  WHERE parent_message_id IS NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_conversations_thread_parent_unique
  ON weave_conversations (workspace_id, parent_message_id)
  WHERE parent_message_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_conversations_thread_parent
  ON weave_conversations (workspace_id, parent_message_id);

CREATE TABLE IF NOT EXISTS weave_message_flags (
  id           TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
  message_id   TEXT NOT NULL REFERENCES weave_messages(id) ON DELETE CASCADE,
  user_id      TEXT NOT NULL,
  workspace_id TEXT NOT NULL REFERENCES weave_workspaces(id) ON DELETE CASCADE,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (message_id, user_id)
);

CREATE INDEX IF NOT EXISTS idx_message_flags_ws_user_created
  ON weave_message_flags (workspace_id, user_id, created_at DESC);
