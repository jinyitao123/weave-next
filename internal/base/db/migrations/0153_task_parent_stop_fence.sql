-- Parent task admission and cancellation are platform queue invariants. The
-- row lock serializes child admission with a concurrent parent stop, while the
-- stop trigger fences every descendant without claiming that active work has
-- physically exited.
CREATE FUNCTION weave_task_queue_fence_parent() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  parent_status TEXT;
  parent_subject JSONB;
  parent_deadline TIMESTAMPTZ;
BEGIN
  IF NEW.parent_task_id IS NULL THEN
    RETURN NEW;
  END IF;
  SELECT status, actor_subject, deadline_at
    INTO parent_status, parent_subject, parent_deadline
    FROM weave_task_queue
    WHERE workspace_id=NEW.workspace_id AND id=NEW.parent_task_id
    FOR SHARE;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'parent task does not exist' USING ERRCODE='23503';
  END IF;
  IF parent_subject IS DISTINCT FROM NEW.actor_subject THEN
    RAISE EXCEPTION 'parent task subject does not match' USING ERRCODE='23514';
  END IF;
  IF parent_status NOT IN ('queued','dispatched','running') THEN
    RAISE EXCEPTION 'parent task has stopped' USING ERRCODE='23514';
  END IF;
  IF parent_deadline IS NOT NULL AND
      (NEW.deadline_at IS NULL OR parent_deadline < NEW.deadline_at) THEN
    NEW.deadline_at := parent_deadline;
  END IF;
  RETURN NEW;
END; $$;

CREATE TRIGGER weave_task_queue_parent_admission_fence
  BEFORE INSERT ON weave_task_queue
  FOR EACH ROW EXECUTE FUNCTION weave_task_queue_fence_parent();

CREATE FUNCTION weave_task_queue_stop_descendants() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.status NOT IN ('completed','failed','cancelled','superseded','cut','timed_out')
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

CREATE TRIGGER weave_task_queue_parent_stop_cascade
  AFTER UPDATE OF status,worker_id ON weave_task_queue
  FOR EACH ROW EXECUTE FUNCTION weave_task_queue_stop_descendants();
