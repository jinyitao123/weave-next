-- Classify standalone HTML-document deliverables as text/html so previews
-- render in a sandboxed frame and downloads carry a .html extension. Backfills
-- rows that were projected as text/markdown before classification existed.
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

  -- Explicit-declaration model: a deliverable is projected only when the
  -- agent declared one via the save_deliverable tool during the turn.
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
  DO NOTHING;
END;
$$;

ALTER TABLE weave_final_deliverables DISABLE TRIGGER weave_final_deliverable_immutable;

UPDATE weave_final_deliverables
SET content_type = 'text/html'
WHERE content_type = 'text/markdown'
  AND (left(btrim(content), 15) ILIKE '<!doctype html%'
    OR left(btrim(content), 6) ILIKE '<html%');

ALTER TABLE weave_final_deliverables ENABLE TRIGGER weave_final_deliverable_immutable;
