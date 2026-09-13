-- Cancellation intent must reach descendants even if the parent process is
-- disconnected and cannot acknowledge its own exit. Active descendants keep
-- their worker/lease until their own stop receipts; queued descendants close.
CREATE OR REPLACE FUNCTION weave_task_queue_stop_descendants() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.status='cancel_requested' THEN
    IF OLD.status='cancel_requested' THEN RETURN NEW; END IF;
  ELSIF NEW.status NOT IN ('completed','failed','cancelled','superseded','cut','timed_out')
      OR NEW.worker_id IS NOT NULL
      OR (OLD.status IN ('completed','failed','cancelled','superseded','cut','timed_out')
          AND OLD.worker_id IS NULL) THEN
    RETURN NEW;
  END IF;
  WITH RECURSIVE descendants AS (
    SELECT child.workspace_id, child.id
      FROM weave_task_queue child
      WHERE child.workspace_id=NEW.workspace_id AND child.parent_task_id=NEW.id
    UNION
    SELECT child.workspace_id, child.id
      FROM weave_task_queue child
      JOIN descendants parent
        ON parent.workspace_id=child.workspace_id AND parent.id=child.parent_task_id
  )
  UPDATE weave_task_queue child
    SET status=CASE WHEN child.worker_id IS NULL THEN 'cancelled' ELSE 'cancel_requested' END,
        completed_at=CASE WHEN child.worker_id IS NULL THEN now() ELSE NULL END,
        updated_at=now()
    FROM descendants
    WHERE child.workspace_id=descendants.workspace_id AND child.id=descendants.id
      AND child.status IN ('queued','dispatched','running','cancel_requested');
  RETURN NEW;
END; $$;
