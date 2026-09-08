-- Frozen delivery requirements accompany the existing dispatch input. The API
-- extracts explicit requirements from the immutable user-sourced task text.
ALTER TABLE weave_dispatch_input_revisions
  ADD COLUMN delivery_contract JSONB NOT NULL DEFAULT '{}'::jsonb
  CHECK (jsonb_typeof(delivery_contract) = 'object');

CREATE FUNCTION weave_dispatch_input_facts_immutable()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF (to_jsonb(NEW) - ARRAY['is_current','consumed_run_id','consumed_task_id','consumed_at','closed_at'])
      IS DISTINCT FROM
      (to_jsonb(OLD) - ARRAY['is_current','consumed_run_id','consumed_task_id','consumed_at','closed_at'])
    OR (OLD.consumed_run_id IS NOT NULL AND
      ROW(OLD.consumed_run_id,OLD.consumed_task_id,OLD.consumed_at)
      IS DISTINCT FROM ROW(NEW.consumed_run_id,NEW.consumed_task_id,NEW.consumed_at))
    OR (OLD.closed_at IS NOT NULL AND OLD.closed_at IS DISTINCT FROM NEW.closed_at)
    OR (NOT OLD.is_current AND NEW.is_current) THEN
    RAISE EXCEPTION 'dispatch input facts are immutable' USING ERRCODE = '55000';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER weave_dispatch_input_facts_immutable
  BEFORE UPDATE ON weave_dispatch_input_revisions
  FOR EACH ROW EXECUTE FUNCTION weave_dispatch_input_facts_immutable();

-- Dispatch creates a snapshot before the queue consumer creates TeamRun.
-- Consequently the frozen binding is keyed by snapshot, not by a future run FK.
CREATE TABLE weave_run_delivery_state (
  workspace_id TEXT NOT NULL,
  run_snapshot_id TEXT NOT NULL REFERENCES weave_team_run_snapshots(run_id),
  run_id TEXT,
  input_revision_id TEXT NOT NULL DEFAULT '',
  workflow_id TEXT NOT NULL,
  workflow_version INTEGER NOT NULL CHECK (workflow_version > 0),
  published_digest TEXT NOT NULL,
  contract JSONB NOT NULL,
  contract_digest TEXT NOT NULL CHECK (contract_digest ~ '^[0-9a-f]{64}$'),
  current_revision_id TEXT,
  current_verification_id TEXT,
  selection_sequence BIGINT NOT NULL DEFAULT 0 CHECK (selection_sequence >= 0),
  created_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
  PRIMARY KEY (workspace_id, run_snapshot_id),
  UNIQUE (workspace_id, run_id),
  UNIQUE (workspace_id, run_snapshot_id, contract_digest),
  CHECK (workspace_id <> '' AND run_snapshot_id <> '' AND workflow_id <> ''),
  CHECK (run_id IS NULL OR run_id <> ''),
  CHECK (jsonb_typeof(contract) IN ('object', 'null')),
  CHECK ((current_revision_id IS NULL AND current_verification_id IS NULL AND selection_sequence = 0)
    OR (run_id IS NOT NULL AND current_revision_id IS NOT NULL AND current_revision_id <> ''
      AND current_verification_id IS NOT NULL AND current_verification_id <> '' AND selection_sequence > 0))
);

CREATE TABLE weave_run_delivery_verifications (
  workspace_id TEXT NOT NULL,
  run_id TEXT NOT NULL,
  run_snapshot_id TEXT NOT NULL,
  verification_id TEXT NOT NULL,
  revision_id TEXT NOT NULL,
  contract_digest TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('passed', 'failed', 'unknown')),
  report JSONB NOT NULL CHECK (jsonb_typeof(report) = 'object'),
  created_at TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (workspace_id, verification_id),
  UNIQUE (workspace_id, run_id, run_snapshot_id, revision_id, verification_id),
  FOREIGN KEY (workspace_id, run_id) REFERENCES weave_team_runs(workspace_id, run_id),
  FOREIGN KEY (workspace_id, run_snapshot_id, contract_digest)
    REFERENCES weave_run_delivery_state(workspace_id, run_snapshot_id, contract_digest),
  CHECK (verification_id <> '' AND revision_id <> ''),
  CHECK (report->>'id' IS NOT DISTINCT FROM verification_id
    AND report->>'revision_id' IS NOT DISTINCT FROM revision_id
    AND report->>'contract_digest' IS NOT DISTINCT FROM contract_digest
    AND report->>'status' IS NOT DISTINCT FROM status
    AND report->'candidate'->>'workspace_id' IS NOT DISTINCT FROM workspace_id
    AND report->'candidate'->>'run_id' IS NOT DISTINCT FROM run_id
    AND report->'candidate'->>'run_snapshot_id' IS NOT DISTINCT FROM run_snapshot_id)
);

ALTER TABLE weave_run_delivery_state ADD CONSTRAINT weave_run_delivery_current_report_fk
  FOREIGN KEY (workspace_id, run_id, run_snapshot_id, current_revision_id, current_verification_id)
  REFERENCES weave_run_delivery_verifications(workspace_id, run_id, run_snapshot_id, revision_id, verification_id);

CREATE FUNCTION weave_delivery_verification_immutable()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'delivery verification reports are immutable' USING ERRCODE = '55000';
END;
$$;

CREATE TRIGGER weave_delivery_verification_immutable
  BEFORE UPDATE OR DELETE ON weave_run_delivery_verifications
  FOR EACH ROW EXECUTE FUNCTION weave_delivery_verification_immutable();

CREATE FUNCTION weave_delivery_binding_immutable()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'delivery contract bindings are immutable' USING ERRCODE = '55000';
  END IF;
  IF ROW(OLD.workspace_id, OLD.run_snapshot_id, OLD.input_revision_id, OLD.workflow_id,
      OLD.workflow_version, OLD.published_digest, OLD.contract, OLD.contract_digest, OLD.created_at)
    IS DISTINCT FROM
    ROW(NEW.workspace_id, NEW.run_snapshot_id, NEW.input_revision_id, NEW.workflow_id,
      NEW.workflow_version, NEW.published_digest, NEW.contract, NEW.contract_digest, NEW.created_at)
    OR (OLD.run_id IS NOT NULL AND OLD.run_id IS DISTINCT FROM NEW.run_id)
    OR NEW.selection_sequence < OLD.selection_sequence THEN
    RAISE EXCEPTION 'delivery contract bindings are immutable' USING ERRCODE = '55000';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER weave_delivery_binding_immutable
  BEFORE UPDATE OR DELETE ON weave_run_delivery_state
  FOR EACH ROW EXECUTE FUNCTION weave_delivery_binding_immutable();
