ALTER TABLE weave_task_queue DROP CONSTRAINT IF EXISTS weave_task_queue_status_check;
ALTER TABLE weave_task_queue ADD CONSTRAINT weave_task_queue_status_check
  CHECK (status IN ('queued','dispatched','running','cancel_requested','completed','failed',
                    'cancelled','superseded','cut','timed_out'));
