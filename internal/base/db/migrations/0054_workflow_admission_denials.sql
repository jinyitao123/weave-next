CREATE TABLE weave_workflow_admission_denials (
  workspace_id TEXT NOT NULL,
  workflow_id TEXT NOT NULL,
  workflow_version INT NOT NULL,
  trigger_type TEXT NOT NULL,
  admission_attempt_key TEXT NOT NULL,
  reason_code TEXT NOT NULL,
  decided_at TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (
    workspace_id,
    workflow_id,
    workflow_version,
    trigger_type,
    admission_attempt_key
  ),
  CONSTRAINT weave_workflow_admission_denials_status_fk
    FOREIGN KEY (workspace_id, workflow_id, workflow_version)
    REFERENCES weave_workflow_version_admission_statuses (
      workspace_id,
      workflow_id,
      workflow_version
    ) ON DELETE RESTRICT,
  CONSTRAINT weave_workflow_admission_denials_trigger_type_check
    CHECK (trigger_type IN ('schedule', 'api', 'event', 'session')),
  CONSTRAINT weave_workflow_admission_denials_attempt_key_check
    CHECK (admission_attempt_key <> '' AND admission_attempt_key = btrim(admission_attempt_key)),
  CONSTRAINT weave_workflow_admission_denials_reason_code_check
    CHECK (reason_code IN ('team_worker_disabled', 'workflow_version_blocked'))
);

CREATE INDEX weave_workflow_admission_denials_list_idx
  ON weave_workflow_admission_denials (
    workspace_id,
    workflow_id,
    workflow_version,
    decided_at,
    trigger_type COLLATE "C",
    admission_attempt_key COLLATE "C"
  );

CREATE FUNCTION weave_workflow_admission_denials_reject_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'workflow admission denial audit is append-only'
    USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER weave_workflow_admission_denials_append_only
BEFORE UPDATE OR DELETE ON weave_workflow_admission_denials
FOR EACH ROW
EXECUTE FUNCTION weave_workflow_admission_denials_reject_mutation();
