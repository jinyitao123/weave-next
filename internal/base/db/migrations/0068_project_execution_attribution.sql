ALTER TABLE weave_task_queue ADD COLUMN project_id TEXT;
ALTER TABLE weave_task_group ADD COLUMN project_id TEXT;
ALTER TABLE weave_session_execution_leases ADD COLUMN project_id TEXT;
ALTER TABLE weave_session_outbox ADD COLUMN project_id TEXT;
ALTER TABLE weave_session_execution_audit ADD COLUMN project_id TEXT;
ALTER TABLE weave_team_run_snapshots ADD COLUMN project_id TEXT;
ALTER TABLE weave_team_runs ADD COLUMN project_id TEXT;
ALTER TABLE weave_run_terminal_markers ADD COLUMN project_id TEXT;

UPDATE weave_task_group AS target
SET project_id = conversation.project_id
FROM weave_conversations AS conversation
WHERE target.workspace_id = conversation.workspace_id
  AND target.conversation_id = conversation.id
  AND target.conversation_id <> '';

UPDATE weave_task_queue AS target
SET project_id = task_group.project_id
FROM weave_task_group AS task_group
WHERE target.project_id IS NULL
  AND target.task_group_id IS NOT NULL
  AND task_group.workspace_id = target.workspace_id
  AND task_group.id = target.task_group_id
  AND task_group.project_id IS NOT NULL;

UPDATE weave_task_queue AS target
SET project_id = conversation.project_id
FROM weave_conversations AS conversation
WHERE target.project_id IS NULL
  AND NULLIF(target.payload->>'conversation_id', '') IS NOT NULL
  AND conversation.workspace_id = target.workspace_id
  AND conversation.id = target.payload->>'conversation_id';

UPDATE weave_task_queue AS target
SET project_id = NULLIF(target.payload->>'project_id', '')
WHERE target.project_id IS NULL
  AND NULLIF(target.payload->>'project_id', '') IS NOT NULL;

UPDATE weave_session_execution_leases AS target
SET project_id = conversation.project_id
FROM weave_conversations AS conversation
WHERE target.workspace_id = conversation.workspace_id
  AND target.user_id = conversation.user_id
  AND target.lead_avatar_id = conversation.agent_id
  AND conversation.session_key = target.workspace_id || ':' || target.user_id || ':' || target.lead_avatar_id || ':' || target.session_id;

UPDATE weave_team_run_snapshots AS target
SET project_id = lease.project_id
FROM weave_session_execution_leases AS lease
WHERE target.workspace_id = lease.workspace_id
  AND target.run_id = lease.run_snapshot_id
  AND lease.project_id IS NOT NULL;

UPDATE weave_team_runs AS target
SET project_id = snapshot.project_id
FROM weave_team_run_snapshots AS snapshot
WHERE target.workspace_id = snapshot.workspace_id
  AND target.run_snapshot_id = snapshot.run_id
  AND snapshot.project_id IS NOT NULL;

UPDATE weave_session_outbox AS target
SET project_id = lease.project_id
FROM weave_session_execution_leases AS lease
WHERE target.workspace_id = lease.workspace_id
  AND target.user_id = lease.user_id
  AND target.lead_avatar_id = lease.lead_avatar_id
  AND target.session_id = lease.session_id
  AND lease.project_id IS NOT NULL;

UPDATE weave_session_execution_audit AS target
SET project_id = lease.project_id
FROM weave_session_execution_leases AS lease
WHERE target.workspace_id = lease.workspace_id
  AND target.user_id = lease.user_id
  AND target.lead_avatar_id = lease.lead_avatar_id
  AND target.session_id = lease.session_id
  AND lease.project_id IS NOT NULL;

UPDATE weave_run_terminal_markers AS target
SET project_id = snapshot.project_id
FROM weave_team_run_snapshots AS snapshot
WHERE target.workspace_id = snapshot.workspace_id
  AND target.run_snapshot_id = snapshot.run_id
  AND snapshot.project_id IS NOT NULL;

ALTER TABLE weave_task_queue ADD CONSTRAINT weave_task_queue_project_fk
  FOREIGN KEY (workspace_id, project_id) REFERENCES weave_projects (workspace_id, id) ON DELETE RESTRICT;
ALTER TABLE weave_task_group ADD CONSTRAINT weave_task_group_project_fk
  FOREIGN KEY (workspace_id, project_id) REFERENCES weave_projects (workspace_id, id) ON DELETE RESTRICT;
ALTER TABLE weave_session_execution_leases ADD CONSTRAINT weave_session_execution_leases_project_fk
  FOREIGN KEY (workspace_id, project_id) REFERENCES weave_projects (workspace_id, id) ON DELETE RESTRICT;
ALTER TABLE weave_session_outbox ADD CONSTRAINT weave_session_outbox_project_fk
  FOREIGN KEY (workspace_id, project_id) REFERENCES weave_projects (workspace_id, id) ON DELETE RESTRICT;
ALTER TABLE weave_session_execution_audit ADD CONSTRAINT weave_session_execution_audit_project_fk
  FOREIGN KEY (workspace_id, project_id) REFERENCES weave_projects (workspace_id, id) ON DELETE RESTRICT;
ALTER TABLE weave_team_run_snapshots ADD CONSTRAINT weave_team_run_snapshots_project_fk
  FOREIGN KEY (workspace_id, project_id) REFERENCES weave_projects (workspace_id, id) ON DELETE RESTRICT;
ALTER TABLE weave_team_runs ADD CONSTRAINT weave_team_runs_project_fk
  FOREIGN KEY (workspace_id, project_id) REFERENCES weave_projects (workspace_id, id) ON DELETE RESTRICT;
ALTER TABLE weave_run_terminal_markers ADD CONSTRAINT weave_run_terminal_markers_project_fk
  FOREIGN KEY (workspace_id, project_id) REFERENCES weave_projects (workspace_id, id) ON DELETE RESTRICT;

CREATE INDEX weave_task_queue_project_idx ON weave_task_queue (workspace_id, project_id, created_at DESC)
  WHERE project_id IS NOT NULL;
CREATE INDEX weave_task_group_project_idx ON weave_task_group (workspace_id, project_id, updated_at DESC)
  WHERE project_id IS NOT NULL;
CREATE INDEX weave_team_runs_project_idx ON weave_team_runs (workspace_id, project_id, updated_at DESC)
  WHERE project_id IS NOT NULL;

CREATE FUNCTION weave_terminal_marker_project_derive()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.project_id IS NULL AND NEW.run_snapshot_id IS NOT NULL THEN
    SELECT snapshot.project_id
    INTO NEW.project_id
    FROM weave_team_run_snapshots AS snapshot
    WHERE snapshot.workspace_id = NEW.workspace_id
      AND snapshot.run_id = NEW.run_snapshot_id;
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER weave_run_terminal_marker_project_derive
  BEFORE INSERT ON weave_run_terminal_markers
  FOR EACH ROW EXECUTE FUNCTION weave_terminal_marker_project_derive();

CREATE FUNCTION weave_execution_project_immutable()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF OLD.project_id IS NOT NULL AND NEW.project_id IS DISTINCT FROM OLD.project_id THEN
    RAISE EXCEPTION 'execution project attribution is immutable'
      USING ERRCODE = '55000';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER weave_task_queue_project_immutable BEFORE UPDATE OF project_id ON weave_task_queue
  FOR EACH ROW EXECUTE FUNCTION weave_execution_project_immutable();
CREATE TRIGGER weave_task_group_project_immutable BEFORE UPDATE OF project_id ON weave_task_group
  FOR EACH ROW EXECUTE FUNCTION weave_execution_project_immutable();
CREATE TRIGGER weave_session_execution_leases_project_immutable BEFORE UPDATE OF project_id ON weave_session_execution_leases
  FOR EACH ROW EXECUTE FUNCTION weave_execution_project_immutable();
CREATE TRIGGER weave_session_outbox_project_immutable BEFORE UPDATE OF project_id ON weave_session_outbox
  FOR EACH ROW EXECUTE FUNCTION weave_execution_project_immutable();
CREATE TRIGGER weave_session_execution_audit_project_immutable BEFORE UPDATE OF project_id ON weave_session_execution_audit
  FOR EACH ROW EXECUTE FUNCTION weave_execution_project_immutable();
CREATE TRIGGER weave_team_run_snapshots_project_immutable BEFORE UPDATE OF project_id ON weave_team_run_snapshots
  FOR EACH ROW EXECUTE FUNCTION weave_execution_project_immutable();
CREATE TRIGGER weave_team_runs_project_immutable BEFORE UPDATE OF project_id ON weave_team_runs
  FOR EACH ROW EXECUTE FUNCTION weave_execution_project_immutable();
CREATE TRIGGER weave_run_terminal_markers_project_immutable BEFORE UPDATE OF project_id ON weave_run_terminal_markers
  FOR EACH ROW EXECUTE FUNCTION weave_execution_project_immutable();
