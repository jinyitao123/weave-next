ALTER TABLE weave_task_queue ADD COLUMN fairness_key TEXT;
ALTER TABLE weave_task_queue SET (fillfactor=80);
ALTER TABLE weave_run_attempt_leases SET (fillfactor=80);
CREATE INDEX weave_task_queue_workflow_ready ON weave_task_queue(workspace_id, priority DESC, created_at, id)
  WHERE status='queued' AND kind='team_workflow';

-- Transactional hints: a rolled-back enqueue/cancellation never wakes a worker.
CREATE FUNCTION weave_notify_workflow_task() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.kind='team_workflow' THEN
    IF NEW.status='queued' AND (TG_OP='INSERT' OR OLD.status IS DISTINCT FROM NEW.status) THEN
      PERFORM pg_notify('weave_task_ready', NEW.workspace_id);
    ELSIF NEW.status='cancel_requested' AND (TG_OP='INSERT' OR OLD.status IS DISTINCT FROM NEW.status) THEN
      PERFORM pg_notify('weave_task_cancel', COALESCE(NEW.run_snapshot_id, ''));
    END IF;
  END IF;
  RETURN NEW;
END;
$$;
CREATE TRIGGER weave_workflow_task_notification AFTER INSERT OR UPDATE OF status ON weave_task_queue
  FOR EACH ROW EXECUTE FUNCTION weave_notify_workflow_task();
