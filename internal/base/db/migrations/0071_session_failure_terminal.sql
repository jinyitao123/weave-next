ALTER TABLE weave_session_execution_leases
  DROP CONSTRAINT weave_session_execution_leases_close_reason_check;

ALTER TABLE weave_session_execution_leases
  ADD CONSTRAINT weave_session_execution_leases_close_reason_check CHECK (
    close_reason IS NULL OR close_reason IN ('final_committed', 'expired', 'failed')
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
BEGIN
  IF outbox.role <> 'assistant' OR btrim(outbox.content) = '' THEN
    RETURN;
  END IF;

  deliverable_id := 'deliverable_' || md5(
    outbox.workspace_id || chr(31) || outbox.user_id || chr(31) ||
    outbox.lead_avatar_id || chr(31) || outbox.session_id || chr(31) || outbox.event_id
  );
  deliverable_title := COALESCE(
    NULLIF(outbox.metadata->'assistant_metadata'->>'title', ''),
    'Final deliverable'
  );

  INSERT INTO weave_final_deliverables (
    id, workspace_id, project_id, conversation_id, user_id, lead_avatar_id,
    session_id, event_id, run_id, run_snapshot_id, title, content,
    content_type, metadata, created_at
  ) VALUES (
    deliverable_id, outbox.workspace_id, outbox.project_id,
    NULLIF(outbox.metadata->>'conversation_id', ''), outbox.user_id,
    outbox.lead_avatar_id, outbox.session_id, outbox.event_id,
    outbox.active_run_id, outbox.run_snapshot_id, deliverable_title,
    outbox.content, 'text/markdown', outbox.metadata, outbox.created_at
  )
  ON CONFLICT (workspace_id, user_id, lead_avatar_id, session_id, event_id)
  DO NOTHING;
END;
$$;
