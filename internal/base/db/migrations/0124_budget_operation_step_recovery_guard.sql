CREATE OR REPLACE FUNCTION weave_team_build_operation_steps_guard()
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
       AND NEW.lease_owner IS NULL
       AND NEW.lease_until IS NULL
       AND NEW.lease_epoch = OLD.lease_epoch
       AND NEW.attempt = OLD.attempt
       AND NEW.error_class IS NULL
       AND NEW.error_code IS NULL
       AND NEW.evidence_json IS NULL
       AND NEW.output_hash IS NULL
       AND NEW.started_at IS NOT DISTINCT FROM OLD.started_at
       AND NEW.completed_at IS NULL
       AND NEW.updated_at >= OLD.updated_at THEN
      RETURN NEW;
    END IF;
    RAISE EXCEPTION 'terminal team build operation step is immutable'
      USING ERRCODE = '23514';
  END IF;

  IF (OLD.status = 'pending' AND NEW.status NOT IN ('pending', 'running'))
     OR (OLD.status = 'running' AND NEW.status NOT IN ('pending', 'running', 'succeeded', 'skipped', 'failed')) THEN
    RAISE EXCEPTION 'invalid team build operation step transition % -> %',
      OLD.status, NEW.status
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;
