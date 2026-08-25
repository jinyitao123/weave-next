DO $$
DECLARE
  legacy_timezone TEXT := NULLIF(
    current_setting('weave.legacy_schedule_timezone', true),
    ''
  );
BEGIN
  IF EXISTS (SELECT 1 FROM weave_schedule WHERE kind = 'daily') THEN
    IF legacy_timezone IS NULL
       OR legacy_timezone = 'Local'
       OR NOT EXISTS (
         SELECT 1 FROM pg_timezone_names WHERE name = legacy_timezone
       ) THEN
      RAISE EXCEPTION
        'WEAVE_LEGACY_SCHEDULE_TIMEZONE must be a valid IANA timezone for legacy daily schedules'
        USING ERRCODE = '22023';
    END IF;
  END IF;
END;
$$;

ALTER TABLE weave_schedule
  ADD COLUMN target_kind TEXT,
  ADD COLUMN target_workflow_id TEXT,
  ADD COLUMN timezone TEXT;

ALTER TABLE weave_schedule
  ALTER COLUMN agent DROP NOT NULL,
  ALTER COLUMN message DROP NOT NULL;

UPDATE weave_schedule
SET target_kind = 'agent',
    timezone = CASE
      WHEN kind = 'daily'
        THEN current_setting('weave.legacy_schedule_timezone', true)
      ELSE 'UTC'
    END;

-- The legacy executor ignored run_at on daily rows and time_of_day on once
-- rows. Normalize only those redundant fields before enforcing the new shape.
UPDATE weave_schedule
SET run_at = NULL
WHERE kind = 'daily';

UPDATE weave_schedule
SET time_of_day = ''
WHERE kind = 'once';

ALTER TABLE weave_schedule
  ALTER COLUMN target_kind SET NOT NULL,
  ALTER COLUMN timezone SET NOT NULL,
  ADD CONSTRAINT weave_schedule_workspace_id_key
    UNIQUE (workspace_id, id),
  ADD CONSTRAINT weave_schedule_target_kind_check
    CHECK (target_kind IN ('agent', 'team_workflow')),
  ADD CONSTRAINT weave_schedule_kind_shape_check CHECK (
    (
      kind = 'daily'
      AND time_of_day ~ '^([01][0-9]|2[0-3]):[0-5][0-9]$'
      AND run_at IS NULL
    )
    OR (
      kind = 'once'
      AND time_of_day = ''
      AND run_at IS NOT NULL
    )
  ),
  ADD CONSTRAINT weave_schedule_target_shape_check CHECK (
    (
      target_kind = 'agent'
      AND agent IS NOT NULL
      AND agent <> ''
      AND target_workflow_id IS NULL
    )
    OR (
      target_kind = 'team_workflow'
      AND agent IS NULL
      AND message IS NULL
      AND target_workflow_id IS NOT NULL
      AND target_workflow_id <> ''
    )
  ),
  ADD CONSTRAINT weave_schedule_target_workflow_fk
    FOREIGN KEY (workspace_id, target_workflow_id)
    REFERENCES weave_team_workflows (workspace_id, id) ON DELETE RESTRICT;

CREATE FUNCTION weave_schedule_validate_timezone()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.timezone = 'Local'
     OR NOT EXISTS (
       SELECT 1 FROM pg_timezone_names WHERE name = NEW.timezone
     ) THEN
    RAISE EXCEPTION 'schedule timezone must be a valid IANA timezone'
      USING ERRCODE = '22023';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER weave_schedule_timezone_guard
BEFORE INSERT OR UPDATE OF timezone
ON weave_schedule
FOR EACH ROW
EXECUTE FUNCTION weave_schedule_validate_timezone();

CREATE TABLE weave_schedule_occurrences (
  workspace_id TEXT NOT NULL,
  occurrence_key TEXT NOT NULL,
  schedule_id TEXT NOT NULL,
  target_workflow_id TEXT NOT NULL,
  scheduled_for TIMESTAMPTZ NOT NULL,
  status TEXT NOT NULL,
  workflow_version INT,
  run_snapshot_id TEXT,
  task_id TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, occurrence_key),
  CONSTRAINT weave_schedule_occurrences_key_check
    CHECK (occurrence_key ~ '^[0-9a-f]{64}$'),
  CONSTRAINT weave_schedule_occurrences_status_check
    CHECK (status IN ('pending', 'committed')),
  CONSTRAINT weave_schedule_occurrences_state_check CHECK (
    (
      status = 'pending'
      AND workflow_version IS NULL
      AND run_snapshot_id IS NULL
      AND task_id IS NULL
    )
    OR (
      status = 'committed'
      AND workflow_version IS NOT NULL
      AND workflow_version > 0
      AND run_snapshot_id IS NOT NULL
      AND run_snapshot_id <> ''
      AND task_id IS NOT NULL
      AND task_id <> ''
    )
  ),
  CONSTRAINT weave_schedule_occurrences_schedule_fk
    FOREIGN KEY (workspace_id, schedule_id)
    REFERENCES weave_schedule (workspace_id, id) ON DELETE RESTRICT,
  CONSTRAINT weave_schedule_occurrences_workflow_fk
    FOREIGN KEY (workspace_id, target_workflow_id)
    REFERENCES weave_team_workflows (workspace_id, id) ON DELETE RESTRICT,
  CONSTRAINT weave_schedule_occurrences_workflow_version_fk
    FOREIGN KEY (workspace_id, target_workflow_id, workflow_version)
    REFERENCES weave_team_workflow_versions (
      workspace_id, workflow_id, version
    ) ON DELETE RESTRICT,
  CONSTRAINT weave_schedule_occurrences_snapshot_fk
    FOREIGN KEY (workspace_id, run_snapshot_id)
    REFERENCES weave_team_run_snapshots (
      workspace_id, run_id
    ) ON DELETE RESTRICT,
  CONSTRAINT weave_schedule_occurrences_task_fk
    FOREIGN KEY (workspace_id, task_id)
    REFERENCES weave_task_queue (workspace_id, id) ON DELETE RESTRICT
);

CREATE FUNCTION weave_schedule_occurrences_guard()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
  expected_occurrence_key TEXT;
BEGIN
  IF TG_OP <> 'DELETE' THEN
    expected_occurrence_key := encode(
      sha256(
        convert_to(NEW.workspace_id, 'UTF8')
        || decode('00', 'hex')
        || convert_to(NEW.schedule_id, 'UTF8')
        || decode('00', 'hex')
        || convert_to(
          to_char(
            NEW.scheduled_for AT TIME ZONE 'UTC',
            'YYYY-MM-DD"T"HH24:MI:SS.US'
          ) || '000Z',
          'UTF8'
        )
      ),
      'hex'
    );
    IF NEW.occurrence_key <> expected_occurrence_key THEN
      RAISE EXCEPTION 'occurrence_key does not match canonical schedule occurrence'
        USING ERRCODE = '23514';
    END IF;
  END IF;
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'pending'
       OR NEW.workflow_version IS NOT NULL
       OR NEW.run_snapshot_id IS NOT NULL
       OR NEW.task_id IS NOT NULL THEN
      RAISE EXCEPTION 'schedule occurrences must be inserted pending'
        USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
  END IF;
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'schedule occurrences cannot be deleted'
      USING ERRCODE = '23514';
  END IF;
  IF OLD.status = 'committed' THEN
    RAISE EXCEPTION 'committed schedule occurrences are immutable'
      USING ERRCODE = '23514';
  END IF;
  IF NEW.status <> 'committed'
     OR OLD.workspace_id IS DISTINCT FROM NEW.workspace_id
     OR OLD.occurrence_key IS DISTINCT FROM NEW.occurrence_key
     OR OLD.schedule_id IS DISTINCT FROM NEW.schedule_id
     OR OLD.target_workflow_id IS DISTINCT FROM NEW.target_workflow_id
     OR OLD.scheduled_for IS DISTINCT FROM NEW.scheduled_for
     OR OLD.created_at IS DISTINCT FROM NEW.created_at THEN
    RAISE EXCEPTION 'schedule occurrence may only transition pending to committed'
      USING ERRCODE = '23514';
  END IF;
  PERFORM 1
  FROM weave_schedule
  WHERE workspace_id = NEW.workspace_id
    AND id = NEW.schedule_id
    AND target_kind = 'team_workflow'
    AND target_workflow_id = NEW.target_workflow_id;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'occurrence schedule workflow stamp does not match'
      USING ERRCODE = '23514';
  END IF;
  PERFORM 1
  FROM weave_team_run_snapshots
  WHERE workspace_id = NEW.workspace_id
    AND run_id = NEW.run_snapshot_id
    AND snapshot_schema_version = 2
    AND mode = 'fixed_workflow'
    AND workflow_id = NEW.target_workflow_id
    AND workflow_version = NEW.workflow_version
    AND artifact_workflow_id = NEW.target_workflow_id
    AND artifact_workflow_version = NEW.workflow_version;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'occurrence snapshot stamp does not match'
      USING ERRCODE = '23514';
  END IF;
  PERFORM 1
  FROM weave_task_queue
  WHERE workspace_id = NEW.workspace_id
    AND id = NEW.task_id
    AND identity_kind = 'team_workflow'
    AND identity_schema_version = 2
    AND kind = 'team_workflow'
    AND workflow_id = NEW.target_workflow_id
    AND workflow_version = NEW.workflow_version
    AND run_snapshot_id = NEW.run_snapshot_id;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'occurrence task stamp does not match'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER weave_schedule_occurrences_immutable
BEFORE INSERT OR UPDATE OR DELETE
ON weave_schedule_occurrences
FOR EACH ROW
EXECUTE FUNCTION weave_schedule_occurrences_guard();

CREATE FUNCTION weave_schedule_occurrences_require_committed()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF EXISTS (
    SELECT 1
    FROM weave_schedule_occurrences
    WHERE workspace_id = NEW.workspace_id
      AND occurrence_key = NEW.occurrence_key
      AND status = 'pending'
  ) THEN
    RAISE EXCEPTION 'pending schedule occurrence cannot survive transaction commit'
      USING ERRCODE = '23514';
  END IF;
  RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER weave_schedule_occurrences_committed_gate
AFTER INSERT OR UPDATE
ON weave_schedule_occurrences
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW
EXECUTE FUNCTION weave_schedule_occurrences_require_committed();
