CREATE TABLE weave_chat_requests (
  workspace_id TEXT NOT NULL REFERENCES weave_workspaces(id) ON DELETE CASCADE,
  user_id TEXT NOT NULL,
  client_request_id UUID NOT NULL,
  request_fingerprint TEXT NOT NULL,
  project_id TEXT NOT NULL,
  agent_id TEXT NOT NULL,
  session_id TEXT,
  conversation_id TEXT,
  user_message_id TEXT,
  task_id TEXT,
  run_id TEXT,
  status TEXT NOT NULL DEFAULT 'admitting'
    CHECK (status IN ('admitting','admitted','queued','running','yielded','completed','failed')),
  response JSONB,
  error_code TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, user_id, client_request_id),
  CONSTRAINT weave_chat_requests_project_fk
    FOREIGN KEY (workspace_id, project_id)
    REFERENCES weave_projects (workspace_id, id) ON DELETE RESTRICT,
  CONSTRAINT weave_chat_requests_conversation_fk
    FOREIGN KEY (conversation_id)
    REFERENCES weave_conversations (id) ON DELETE RESTRICT,
  CONSTRAINT weave_chat_requests_user_message_fk
    FOREIGN KEY (user_message_id)
    REFERENCES weave_messages (id) ON DELETE RESTRICT,
  CONSTRAINT weave_chat_requests_task_fk
    FOREIGN KEY (task_id)
    REFERENCES weave_task_queue (id) ON DELETE RESTRICT,
  CONSTRAINT weave_chat_requests_identity_check
    CHECK (btrim(user_id) <> '' AND btrim(request_fingerprint) <> '' AND btrim(agent_id) <> ''),
  CONSTRAINT weave_chat_requests_admission_shape_check
    CHECK (
      (session_id IS NULL AND conversation_id IS NULL AND user_message_id IS NULL)
      OR
      (session_id IS NOT NULL AND conversation_id IS NOT NULL AND user_message_id IS NOT NULL)
    )
);

CREATE INDEX weave_chat_requests_project_idx
  ON weave_chat_requests (workspace_id, project_id, updated_at DESC);

CREATE INDEX weave_chat_requests_task_idx
  ON weave_chat_requests (workspace_id, task_id)
  WHERE task_id IS NOT NULL;
