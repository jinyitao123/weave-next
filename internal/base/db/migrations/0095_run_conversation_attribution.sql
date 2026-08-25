ALTER TABLE weave_run_terminal_markers
  ADD COLUMN conversation_id TEXT;

CREATE INDEX weave_run_terminal_markers_conversation_idx
  ON weave_run_terminal_markers (
    workspace_id,
    conversation_id,
    terminal_at DESC
  )
  WHERE conversation_id IS NOT NULL;

CREATE FUNCTION weave_terminal_marker_conversation_derive()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.conversation_id IS NULL AND NEW.run_snapshot_id IS NOT NULL THEN
    SELECT snapshot.trigger_source_v2->>'source_ref'
    INTO NEW.conversation_id
    FROM weave_team_run_snapshots AS snapshot
    WHERE snapshot.workspace_id = NEW.workspace_id
      AND snapshot.run_id = NEW.run_snapshot_id
      AND snapshot.trigger_source_v2->>'type' = 'conversation_explicit'
      AND snapshot.trigger_source_v2->>'source_ref' <> '';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER weave_run_terminal_marker_conversation_derive
  BEFORE INSERT ON weave_run_terminal_markers
  FOR EACH ROW EXECUTE FUNCTION weave_terminal_marker_conversation_derive();

CREATE FUNCTION weave_execution_conversation_immutable()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF OLD.conversation_id IS NOT NULL AND NEW.conversation_id IS DISTINCT FROM OLD.conversation_id THEN
    RAISE EXCEPTION 'execution conversation attribution is immutable'
      USING ERRCODE = '55000';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER weave_run_terminal_markers_conversation_immutable
  BEFORE UPDATE OF conversation_id ON weave_run_terminal_markers
  FOR EACH ROW EXECUTE FUNCTION weave_execution_conversation_immutable();
