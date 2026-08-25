ALTER TABLE weave_task_queue
  ADD COLUMN runtime_assignment JSONB;

ALTER TABLE weave_team_run_snapshots
  ADD COLUMN runtime_assignment JSONB;

ALTER TABLE weave_task_queue
  ADD CONSTRAINT weave_task_queue_runtime_assignment_check CHECK (
    runtime_assignment IS NULL OR jsonb_typeof(runtime_assignment) = 'object'
  );

ALTER TABLE weave_team_run_snapshots
  ADD CONSTRAINT weave_team_run_snapshots_runtime_assignment_check CHECK (
    runtime_assignment IS NULL OR jsonb_typeof(runtime_assignment) = 'object'
  );
