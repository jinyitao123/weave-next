CREATE TABLE weave_capability_workflow_invocations (
  workspace_id TEXT NOT NULL,
  application_id TEXT NOT NULL,
  request_id TEXT NOT NULL,
  invocation_id TEXT NOT NULL,
  credential_id TEXT NOT NULL,
  capability_id TEXT NOT NULL,
  revision BIGINT NOT NULL CHECK (revision > 0),
  definition_hash TEXT NOT NULL CHECK (length(definition_hash) = 64),
  workflow_id TEXT NOT NULL,
  workflow_version INT NOT NULL CHECK (workflow_version > 0),
  input JSONB NOT NULL CHECK (jsonb_typeof(input) = 'object'),
  input_hash TEXT NOT NULL CHECK (length(input_hash) = 64),
  lane TEXT,
  admission_request_id TEXT NOT NULL,
  run_id TEXT NOT NULL,
  task_id TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, application_id, invocation_id),
  UNIQUE (workspace_id, application_id, request_id),
  UNIQUE (workspace_id, admission_request_id),
  UNIQUE (workspace_id, run_id),
  FOREIGN KEY (workspace_id, application_id)
    REFERENCES weave_capability_apps(workspace_id, id) ON DELETE RESTRICT,
  FOREIGN KEY (workspace_id, credential_id)
    REFERENCES weave_capability_credentials(workspace_id, id) ON DELETE RESTRICT,
  FOREIGN KEY (workspace_id, capability_id, revision)
    REFERENCES weave_capability_revisions(workspace_id, capability_id, revision) ON DELETE RESTRICT,
  CHECK (workspace_id <> '' AND application_id <> '' AND request_id <> '' AND invocation_id <> ''
    AND credential_id <> '' AND capability_id <> '' AND workflow_id <> ''
    AND admission_request_id <> '' AND run_id <> '' AND task_id <> ''),
  CHECK (lane IS NULL OR (lane <> '' AND octet_length(lane) <= 256))
);

CREATE INDEX weave_capability_workflow_invocations_lane_idx
  ON weave_capability_workflow_invocations (workspace_id, application_id, lane)
  WHERE lane IS NOT NULL;

CREATE TABLE weave_capability_workflow_cancel_intents (
  workspace_id TEXT NOT NULL,
  application_id TEXT NOT NULL,
  invocation_id TEXT NOT NULL,
  requested_by_credential_id TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, application_id, invocation_id),
  FOREIGN KEY (workspace_id, application_id)
    REFERENCES weave_capability_apps(workspace_id, id) ON DELETE RESTRICT,
  FOREIGN KEY (workspace_id, requested_by_credential_id)
    REFERENCES weave_capability_credentials(workspace_id, id) ON DELETE RESTRICT,
  CHECK (workspace_id <> '' AND application_id <> '' AND invocation_id <> '' AND requested_by_credential_id <> '')
);

CREATE FUNCTION weave_capability_workflow_invocations_reject_mutation()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'capability workflow invocation facts are immutable'
    USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER weave_capability_workflow_invocations_immutable
BEFORE UPDATE OR DELETE ON weave_capability_workflow_invocations
FOR EACH ROW EXECUTE FUNCTION weave_capability_workflow_invocations_reject_mutation();

CREATE FUNCTION weave_capability_workflow_cancel_intents_reject_mutation()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'capability workflow cancellation intents are immutable'
    USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER weave_capability_workflow_cancel_intents_immutable
BEFORE UPDATE OR DELETE ON weave_capability_workflow_cancel_intents
FOR EACH ROW EXECUTE FUNCTION weave_capability_workflow_cancel_intents_reject_mutation();
