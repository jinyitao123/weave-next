-- Capability admission (quota and lane checks) counts in-flight invocations.
-- These partial indexes hold only live rows, so that work follows in-flight
-- load instead of the workspace's whole invocation and task history.
CREATE INDEX weave_capability_workflow_invocations_task_idx
  ON weave_capability_workflow_invocations (workspace_id, task_id);
CREATE INDEX weave_workflow_admission_requests_unreceipted_idx
  ON weave_workflow_admission_requests (workspace_id, request_id)
  WHERE receipt IS NULL;
CREATE INDEX weave_capability_invocations_waiting_idx
  ON weave_capability_invocations (workspace_id)
  WHERE status = 'waiting';
CREATE INDEX weave_team_runs_live_idx
  ON weave_team_runs (workspace_id, run_id)
  WHERE status IN ('queued', 'running', 'parked', 'cancel_requested');
CREATE INDEX weave_task_queue_unacknowledged_failure_idx
  ON weave_task_queue (workspace_id, id)
  WHERE status = 'failed' AND worker_id IS NOT NULL;

-- Every workflow invocation is inserted after its admission request in the
-- same transaction and neither is ever deleted. The live-candidate count
-- relies on that, so the database now enforces it.
ALTER TABLE weave_capability_workflow_invocations
  ADD CONSTRAINT weave_capability_workflow_invocations_admission_fk
  FOREIGN KEY (workspace_id, admission_request_id)
  REFERENCES weave_workflow_admission_requests (workspace_id, request_id)
  ON DELETE RESTRICT NOT VALID;
ALTER TABLE weave_capability_workflow_invocations
  VALIDATE CONSTRAINT weave_capability_workflow_invocations_admission_fk;
