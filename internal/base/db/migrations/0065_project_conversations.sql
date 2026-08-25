LOCK TABLE weave_projects, weave_conversations, weave_messages, weave_agents
  IN ACCESS EXCLUSIVE MODE;

ALTER TABLE weave_projects
  ADD COLUMN system_kind TEXT,
  ADD CONSTRAINT weave_projects_system_kind_check
    CHECK (system_kind IS NULL OR system_kind = 'unclassified');

CREATE UNIQUE INDEX weave_projects_system_kind_key
  ON weave_projects (workspace_id, avatar_id, system_kind)
  WHERE system_kind IS NOT NULL;

DO $$
BEGIN
  IF EXISTS (
    SELECT 1
    FROM weave_conversations AS conversation
    LEFT JOIN weave_agents AS agent
      ON agent.workspace_id = conversation.workspace_id
     AND agent.id = conversation.agent_id
     AND agent.role = 'avatar'
     AND agent.deleted = false
    WHERE conversation.parent_message_id IS NULL
      AND agent.id IS NULL
  ) THEN
    RAISE EXCEPTION 'historical root conversation cannot be attributed to an active avatar'
      USING ERRCODE = '23514';
  END IF;
END;
$$;

WITH owners AS (
  SELECT DISTINCT conversation.workspace_id, conversation.agent_id AS avatar_id
  FROM weave_conversations AS conversation
  WHERE conversation.parent_message_id IS NULL
), generated AS (
  SELECT owner.workspace_id, owner.avatar_id, gen_random_uuid()::text AS id
  FROM owners AS owner
  WHERE NOT EXISTS (
    SELECT 1
    FROM weave_projects AS project
    WHERE project.workspace_id = owner.workspace_id
      AND project.avatar_id = owner.avatar_id
      AND project.system_kind = 'unclassified'
  )
)
INSERT INTO weave_projects (
  id, workspace_id, avatar_id, name, description, system_kind
)
SELECT
  generated.id,
  generated.workspace_id,
  generated.avatar_id,
  CASE WHEN EXISTS (
    SELECT 1
    FROM weave_projects AS project
    WHERE project.workspace_id = generated.workspace_id
      AND project.avatar_id = generated.avatar_id
      AND project.archived_at IS NULL
      AND lower(project.name) = lower('未分类')
  ) THEN '未分类 ' || left(generated.id, 8) ELSE '未分类' END,
  '',
  'unclassified'
FROM generated;

ALTER TABLE weave_conversations
  ADD COLUMN project_id TEXT;

UPDATE weave_conversations AS conversation
SET project_id = project.id
FROM weave_projects AS project
WHERE conversation.parent_message_id IS NULL
  AND project.workspace_id = conversation.workspace_id
  AND project.avatar_id = conversation.agent_id
  AND project.system_kind = 'unclassified';

UPDATE weave_conversations AS thread
SET project_id = root.project_id
FROM weave_messages AS parent_message,
     weave_conversations AS root
WHERE thread.parent_message_id IS NOT NULL
  AND parent_message.workspace_id = thread.workspace_id
  AND parent_message.id = thread.parent_message_id
  AND root.workspace_id = thread.workspace_id
  AND root.id = parent_message.conversation_id;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM weave_conversations WHERE project_id IS NULL) THEN
    RAISE EXCEPTION 'conversation project attribution is incomplete'
      USING ERRCODE = '23514';
  END IF;
END;
$$;

ALTER TABLE weave_conversations
  ALTER COLUMN project_id SET NOT NULL,
  ADD CONSTRAINT weave_conversations_project_fk
    FOREIGN KEY (workspace_id, project_id)
    REFERENCES weave_projects (workspace_id, id) ON DELETE RESTRICT;

DROP INDEX idx_conversations_root_unique;

CREATE UNIQUE INDEX idx_conversations_root_project_unique
  ON weave_conversations (workspace_id, project_id, agent_id, user_id, channel)
  WHERE parent_message_id IS NULL;

CREATE INDEX idx_conversations_project_updated
  ON weave_conversations (workspace_id, project_id, user_id, updated_at DESC, id)
  WHERE parent_message_id IS NULL;
