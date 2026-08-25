ALTER TABLE weave_messages
  ADD COLUMN event_id TEXT,
  ADD COLUMN lease_epoch BIGINT;

ALTER TABLE weave_messages
  ADD CONSTRAINT weave_messages_lease_epoch_check CHECK (
    lease_epoch IS NULL OR lease_epoch > 0
  );

CREATE UNIQUE INDEX weave_messages_conversation_event_key
  ON weave_messages (workspace_id, conversation_id, event_id)
  WHERE event_id IS NOT NULL;
