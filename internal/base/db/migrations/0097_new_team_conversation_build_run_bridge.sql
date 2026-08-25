ALTER TABLE weave_conversations
  ADD COLUMN intent TEXT NOT NULL DEFAULT '';

ALTER TABLE weave_conversations
  ADD CONSTRAINT weave_conversations_intent_known_check CHECK (
    intent IN ('', 'create_team')
  );

DROP INDEX weave_team_build_runs_conversation_active_idx;

CREATE UNIQUE INDEX weave_team_build_runs_conversation_active_unique_idx
  ON weave_team_build_runs (workspace_id, conversation_id)
  WHERE conversation_id IS NOT NULL
    AND status IN ('planning', 'authorized', 'round_running', 'publishing');
