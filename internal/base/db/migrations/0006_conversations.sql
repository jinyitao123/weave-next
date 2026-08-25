CREATE TABLE IF NOT EXISTS weave_conversations (
  id              TEXT PRIMARY KEY,
  workspace_id    TEXT NOT NULL REFERENCES weave_workspaces(id) ON DELETE CASCADE,
  agent_id        TEXT NOT NULL REFERENCES weave_agents(id) ON DELETE CASCADE,
  user_id         TEXT NOT NULL,
  title           TEXT NOT NULL DEFAULT '',
  channel         TEXT NOT NULL DEFAULT 'default',
  session_key     TEXT,
  -- In-place event-message updates increment this cache invalidation counter.
  content_version INT NOT NULL DEFAULT 0,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (workspace_id, agent_id, user_id, channel),
  UNIQUE (workspace_id, session_key)
);

CREATE TABLE IF NOT EXISTS weave_messages (
  id                TEXT PRIMARY KEY,
  -- Strict insertion order, independent of clock resolution: two messages in
  -- the same conversation can share a created_at (same request/instant), so
  -- ordering must key on a monotonic sequence, not the wall clock.
  seq               BIGINT GENERATED ALWAYS AS IDENTITY,
  conversation_id   TEXT NOT NULL REFERENCES weave_conversations(id) ON DELETE CASCADE,
  workspace_id      TEXT NOT NULL REFERENCES weave_workspaces(id) ON DELETE CASCADE,
  role              TEXT NOT NULL CHECK (role IN ('user','assistant','event')),
  content           TEXT NOT NULL DEFAULT '',
  parent_message_id TEXT,
  metadata          JSONB,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_messages_conv
  ON weave_messages (conversation_id, seq);

CREATE TABLE IF NOT EXISTS weave_conversation_read_state (
  conversation_id      TEXT NOT NULL REFERENCES weave_conversations(id) ON DELETE CASCADE,
  user_id              TEXT NOT NULL,
  last_read_message_id TEXT,
  last_read_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (conversation_id, user_id)
);

CREATE TABLE IF NOT EXISTS weave_inbox_unread (
  workspace_id    TEXT NOT NULL REFERENCES weave_workspaces(id) ON DELETE CASCADE,
  user_id         TEXT NOT NULL,
  conversation_id TEXT NOT NULL REFERENCES weave_conversations(id) ON DELETE CASCADE,
  unread_count    INT NOT NULL DEFAULT 0,
  last_message_id TEXT,
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, user_id, conversation_id)
);

CREATE INDEX IF NOT EXISTS idx_conversations_user_updated
  ON weave_conversations (workspace_id, user_id, updated_at DESC);

CREATE INDEX IF NOT EXISTS idx_inbox_unread_user
  ON weave_inbox_unread (workspace_id, user_id, updated_at DESC);
