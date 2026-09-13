ALTER TABLE weave_capability_invocation_tasks
  DROP CONSTRAINT weave_capability_invocation_tasks_status_check;

ALTER TABLE weave_capability_invocation_tasks
  ADD CONSTRAINT weave_capability_invocation_tasks_status_check CHECK (
    status IN ('queued','running','waiting','reconciling','completed','failed','cancelled')
  );

