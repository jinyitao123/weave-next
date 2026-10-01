-- Decision 002 (weave-workbench): a Workbench run can notify its employee more
-- than once — at each human wait (human_review) and at its terminal state. The
-- outbox keeps one event per (run, scope): scope 'terminal' or the human
-- interaction id.
ALTER TABLE weave_employee_run_event_outbox
  ADD COLUMN event_scope TEXT NOT NULL DEFAULT 'terminal' CHECK (event_scope <> '');

ALTER TABLE weave_employee_run_event_outbox
  DROP CONSTRAINT weave_employee_run_event_outbox_workspace_id_run_id_key,
  ADD CONSTRAINT weave_employee_run_event_outbox_scope_key UNIQUE (workspace_id, run_id, event_scope);

CREATE OR REPLACE FUNCTION weave_employee_run_event_outbox_guard()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'employee run event outbox rows are retained for audit' USING ERRCODE = '55000';
  END IF;
  IF ROW(OLD.event_id, OLD.workspace_id, OLD.run_id, OLD.input_revision_id, OLD.payload, OLD.created_at, OLD.event_scope)
    IS DISTINCT FROM
    ROW(NEW.event_id, NEW.workspace_id, NEW.run_id, NEW.input_revision_id, NEW.payload, NEW.created_at, NEW.event_scope) THEN
    RAISE EXCEPTION 'employee run event facts are immutable' USING ERRCODE = '55000';
  END IF;
  RETURN NEW;
END;
$$;
