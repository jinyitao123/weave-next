-- Team-run terminal events are platform integration work, not employee tasks.
-- This outbox keeps delivery retryable while Forge remains the owner of the
-- native inbox rows that employees see and acknowledge.
CREATE TABLE weave_employee_run_event_outbox (
  event_id UUID NOT NULL,
  workspace_id TEXT NOT NULL,
  run_id TEXT NOT NULL,
  input_revision_id TEXT NOT NULL,
  payload JSONB NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
  delivery_state TEXT NOT NULL DEFAULT 'pending'
    CHECK (delivery_state IN ('pending', 'delivering', 'delivered', 'permanent_failure')),
  delivery_attempts INTEGER NOT NULL DEFAULT 0 CHECK (delivery_attempts >= 0),
  next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
  claimed_at TIMESTAMPTZ,
  last_error TEXT,
  forge_notification_id TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
  delivered_at TIMESTAMPTZ,
  PRIMARY KEY (event_id),
  UNIQUE (workspace_id, run_id),
  FOREIGN KEY (workspace_id, run_id) REFERENCES weave_team_runs(workspace_id, run_id),
  CHECK (workspace_id <> '' AND run_id <> '' AND input_revision_id <> ''),
  CHECK ((delivery_state = 'delivering') = (claimed_at IS NOT NULL)),
  CHECK ((delivery_state = 'delivered') = (delivered_at IS NOT NULL))
);

CREATE INDEX weave_employee_run_event_outbox_pending_idx
  ON weave_employee_run_event_outbox(next_attempt_at, created_at, event_id)
  WHERE delivery_state IN ('pending', 'delivering');

CREATE FUNCTION weave_employee_run_event_outbox_guard()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'employee run event outbox rows are retained for audit' USING ERRCODE = '55000';
  END IF;
  IF ROW(OLD.event_id, OLD.workspace_id, OLD.run_id, OLD.input_revision_id, OLD.payload, OLD.created_at)
    IS DISTINCT FROM
    ROW(NEW.event_id, NEW.workspace_id, NEW.run_id, NEW.input_revision_id, NEW.payload, NEW.created_at) THEN
    RAISE EXCEPTION 'employee run event facts are immutable' USING ERRCODE = '55000';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER weave_employee_run_event_outbox_guard
  BEFORE UPDATE OR DELETE ON weave_employee_run_event_outbox
  FOR EACH ROW EXECUTE FUNCTION weave_employee_run_event_outbox_guard();
