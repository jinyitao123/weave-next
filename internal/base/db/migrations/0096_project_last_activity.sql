ALTER TABLE weave_projects
  ADD COLUMN last_activity_at TIMESTAMPTZ;

WITH message_activity AS (
  SELECT workspace_id, conversation_id, max(created_at) AS last_message_at
  FROM weave_messages
  GROUP BY workspace_id, conversation_id
), conversation_activity AS (
  SELECT
    conversation.workspace_id,
    conversation.project_id,
    max(GREATEST(
      conversation.updated_at,
      COALESCE(message_activity.last_message_at, conversation.updated_at)
    )) AS last_activity_at
  FROM weave_conversations AS conversation
  LEFT JOIN message_activity
    ON message_activity.workspace_id = conversation.workspace_id
   AND message_activity.conversation_id = conversation.id
  GROUP BY conversation.workspace_id, conversation.project_id
)
UPDATE weave_projects AS project
SET last_activity_at = COALESCE(conversation_activity.last_activity_at, project.updated_at)
FROM conversation_activity
WHERE conversation_activity.workspace_id = project.workspace_id
  AND conversation_activity.project_id = project.id;

UPDATE weave_projects
SET last_activity_at = updated_at
WHERE last_activity_at IS NULL;

CREATE INDEX weave_projects_workspace_activity_idx
  ON weave_projects (
    workspace_id,
    archived_at,
    COALESCE(last_activity_at, updated_at) DESC,
    id
  );

CREATE OR REPLACE FUNCTION weave_project_final_deliverable(
  outbox weave_session_outbox
)
RETURNS void
LANGUAGE plpgsql
AS $$
DECLARE
  deliverable_id TEXT;
  deliverable_title TEXT;
  deliverable_content TEXT;
  deliverable_content_type TEXT;
BEGIN
  IF outbox.role <> 'assistant' OR btrim(outbox.content) = '' THEN
    RETURN;
  END IF;

  IF jsonb_typeof(outbox.metadata->'declared_deliverable') IS DISTINCT FROM 'object'
    OR btrim(COALESCE(outbox.metadata->'declared_deliverable'->>'content', '')) = '' THEN
    RETURN;
  END IF;

  deliverable_id := 'deliverable_' || md5(
    outbox.workspace_id || chr(31) || outbox.user_id || chr(31) ||
    outbox.lead_avatar_id || chr(31) || outbox.session_id || chr(31) || outbox.event_id
  );
  deliverable_title := COALESCE(
    NULLIF(outbox.metadata->'declared_deliverable'->>'title', ''),
    'Final deliverable'
  );
  deliverable_content := outbox.metadata->'declared_deliverable'->>'content';
  deliverable_content_type := CASE
    WHEN left(btrim(deliverable_content), 15) ILIKE '<!doctype html%'
      OR left(btrim(deliverable_content), 6) ILIKE '<html%'
    THEN 'text/html'
    ELSE 'text/markdown'
  END;

  WITH inserted AS (
    INSERT INTO weave_final_deliverables (
      id, workspace_id, project_id, conversation_id, user_id, lead_avatar_id,
      session_id, event_id, run_id, run_snapshot_id, title, content,
      content_type, metadata, created_at
    ) VALUES (
      deliverable_id, outbox.workspace_id, outbox.project_id,
      NULLIF(outbox.metadata->>'conversation_id', ''), outbox.user_id,
      outbox.lead_avatar_id, outbox.session_id, outbox.event_id,
      outbox.active_run_id, outbox.run_snapshot_id, deliverable_title,
      deliverable_content, deliverable_content_type, outbox.metadata, outbox.created_at
    )
    ON CONFLICT (workspace_id, user_id, lead_avatar_id, session_id, event_id)
    DO NOTHING
    RETURNING workspace_id, project_id, created_at
  )
  UPDATE weave_projects AS project
  SET last_activity_at = GREATEST(
    COALESCE(project.last_activity_at, project.updated_at),
    inserted.created_at
  )
  FROM inserted
  WHERE inserted.project_id IS NOT NULL
    AND project.workspace_id = inserted.workspace_id
    AND project.id = inserted.project_id;
END;
$$;
