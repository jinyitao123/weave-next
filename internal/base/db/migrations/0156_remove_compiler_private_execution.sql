DROP TABLE weave_team_build_operation_attempts;
DROP FUNCTION weave_team_build_operation_attempts_guard();

DROP TRIGGER weave_team_build_operation_steps_guard_trigger
  ON weave_team_build_operation_steps;
DROP FUNCTION weave_team_build_operation_steps_guard();

DROP INDEX weave_team_build_operation_steps_owner_idx;

ALTER TABLE weave_team_build_operation_steps
  DROP CONSTRAINT weave_team_build_operation_steps_status_check,
  DROP CONSTRAINT weave_team_build_operation_steps_attempt_check,
  DROP CONSTRAINT weave_team_build_operation_steps_lease_check,
  DROP CONSTRAINT weave_team_build_operation_steps_terminal_check,
  DROP CONSTRAINT weave_team_build_operation_steps_time_check,
  DROP COLUMN lease_owner,
  DROP COLUMN lease_epoch,
  DROP COLUMN lease_until,
  DROP COLUMN attempt;

ALTER TABLE weave_team_build_operation_steps
  ADD CONSTRAINT weave_team_build_operation_steps_status_check CHECK (
    status IN ('pending', 'succeeded', 'skipped', 'failed')
  ),
  ADD CONSTRAINT weave_team_build_operation_steps_terminal_check CHECK (
    (
      status IN ('succeeded', 'skipped')
      AND started_at IS NOT NULL
      AND completed_at IS NOT NULL
      AND evidence_json IS NOT NULL
      AND output_hash IS NOT NULL
      AND error_class IS NULL
      AND error_code IS NULL
    )
    OR (
      status = 'failed'
      AND started_at IS NOT NULL
      AND completed_at IS NOT NULL
      AND evidence_json IS NOT NULL
      AND error_class IS NOT NULL
      AND error_class <> ''
      AND error_code IS NOT NULL
      AND error_code <> ''
      AND output_hash IS NULL
    )
    OR (
      status = 'pending'
      AND started_at IS NULL
      AND completed_at IS NULL
      AND evidence_json IS NULL
      AND output_hash IS NULL
      AND error_class IS NULL
      AND error_code IS NULL
    )
  ),
  ADD CONSTRAINT weave_team_build_operation_steps_time_check CHECK (
    created_at <= updated_at
    AND (started_at IS NULL OR created_at <= started_at)
    AND (completed_at IS NULL OR started_at <= completed_at)
  );

DROP INDEX weave_team_build_operation_steps_ready_idx;
CREATE INDEX weave_team_build_operation_steps_ready_idx
  ON weave_team_build_operation_steps (
    workspace_id, build_run_id, revision_no, status, operation_index
  )
  WHERE status = 'pending';

CREATE FUNCTION weave_team_build_operation_steps_guard()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'team build operation steps cannot be deleted'
      USING ERRCODE = '23514';
  END IF;

  IF OLD.workspace_id <> NEW.workspace_id
     OR OLD.build_run_id <> NEW.build_run_id
     OR OLD.revision_no <> NEW.revision_no
     OR OLD.operation_id <> NEW.operation_id
     OR OLD.operation_index <> NEW.operation_index
     OR OLD.operation_type <> NEW.operation_type
     OR OLD.depends_on <> NEW.depends_on
     OR OLD.input_hash <> NEW.input_hash
     OR OLD.created_at <> NEW.created_at THEN
    RAISE EXCEPTION 'immutable team build operation step facts changed'
      USING ERRCODE = '23514';
  END IF;

  IF OLD.status IN ('succeeded', 'skipped', 'failed') THEN
    IF OLD.status = 'failed'
       AND OLD.error_class = 'budget_exhausted'
       AND NEW.status = 'pending'
       AND NEW.error_class IS NULL
       AND NEW.error_code IS NULL
       AND NEW.evidence_json IS NULL
       AND NEW.output_hash IS NULL
       AND NEW.started_at IS NULL
       AND NEW.completed_at IS NULL
       AND NEW.updated_at >= OLD.updated_at THEN
      RETURN NEW;
    END IF;
    RAISE EXCEPTION 'terminal team build operation step is immutable'
      USING ERRCODE = '23514';
  END IF;

  IF OLD.status <> 'pending'
     OR NEW.status NOT IN ('pending', 'succeeded', 'skipped', 'failed') THEN
    RAISE EXCEPTION 'invalid team build operation step transition % -> %',
      OLD.status, NEW.status
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER weave_team_build_operation_steps_guard_trigger
BEFORE UPDATE OR DELETE ON weave_team_build_operation_steps
FOR EACH ROW
EXECUTE FUNCTION weave_team_build_operation_steps_guard();
