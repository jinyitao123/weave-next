CREATE TABLE weave_final_deliverables (
  id TEXT NOT NULL,
  workspace_id TEXT NOT NULL,
  project_id TEXT,
  conversation_id TEXT,
  user_id TEXT NOT NULL,
  lead_avatar_id TEXT NOT NULL,
  session_id TEXT NOT NULL,
  event_id TEXT NOT NULL,
  run_id TEXT NOT NULL,
  run_snapshot_id TEXT NOT NULL,
  title TEXT NOT NULL DEFAULT 'Final deliverable',
  content TEXT NOT NULL,
  content_type TEXT NOT NULL DEFAULT 'text/markdown',
  metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, id),
  UNIQUE (workspace_id, user_id, lead_avatar_id, session_id, event_id),
  FOREIGN KEY (workspace_id, project_id)
    REFERENCES weave_projects (workspace_id, id) ON DELETE RESTRICT
);

CREATE INDEX weave_final_deliverables_project_idx
  ON weave_final_deliverables (workspace_id, project_id, created_at DESC)
  WHERE project_id IS NOT NULL;

CREATE INDEX weave_final_deliverables_conversation_idx
  ON weave_final_deliverables (workspace_id, conversation_id, created_at DESC)
  WHERE conversation_id IS NOT NULL;

CREATE FUNCTION weave_project_final_deliverable(
  outbox weave_session_outbox
)
RETURNS void
LANGUAGE plpgsql
AS $$
DECLARE
  deliverable_id TEXT;
  deliverable_title TEXT;
BEGIN
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

DO $$
DECLARE
  outbox weave_session_outbox%ROWTYPE;
BEGIN
  FOR outbox IN
    SELECT * FROM weave_session_outbox
  LOOP
    PERFORM weave_project_final_deliverable(outbox);
  END LOOP;
END;
$$;

CREATE FUNCTION weave_final_deliverable_from_outbox()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  PERFORM weave_project_final_deliverable(NEW);
  RETURN NEW;
END;
$$;

CREATE TRIGGER weave_session_outbox_final_deliverable
  AFTER INSERT ON weave_session_outbox
  FOR EACH ROW EXECUTE FUNCTION weave_final_deliverable_from_outbox();

CREATE TRIGGER weave_final_deliverable_project_immutable
  BEFORE UPDATE OF project_id ON weave_final_deliverables
  FOR EACH ROW EXECUTE FUNCTION weave_execution_project_immutable();

CREATE FUNCTION weave_final_deliverable_immutable()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'final deliverables are immutable'
    USING ERRCODE = '55000';
END;
$$;

CREATE TRIGGER weave_final_deliverable_immutable
  BEFORE UPDATE OR DELETE ON weave_final_deliverables
  FOR EACH ROW EXECUTE FUNCTION weave_final_deliverable_immutable();
