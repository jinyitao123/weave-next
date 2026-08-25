LOCK TABLE weave_team_run_snapshots IN ACCESS EXCLUSIVE MODE;

DROP TRIGGER weave_team_run_snapshots_immutable
  ON weave_team_run_snapshots;

ALTER TABLE weave_team_run_snapshots
  ADD COLUMN snapshot_schema_version INT,
  ADD COLUMN mode TEXT,
  ADD COLUMN artifact_workflow_id TEXT,
  ADD COLUMN artifact_workflow_version INT,
  ADD COLUMN trigger_source_v2 JSONB;

UPDATE weave_team_run_snapshots
SET snapshot_schema_version = 1;

ALTER TABLE weave_team_run_snapshots
  ALTER COLUMN snapshot_schema_version SET NOT NULL,
  ALTER COLUMN lead_avatar_id DROP NOT NULL,
  ALTER COLUMN lead_avatar_version DROP NOT NULL,
  ALTER COLUMN worker_versions DROP NOT NULL,
  ALTER COLUMN team_worker_snapshot DROP NOT NULL,
  DROP CONSTRAINT weave_team_run_snapshots_worker_versions_check,
  DROP CONSTRAINT weave_team_run_snapshots_team_workers_check;

ALTER TABLE weave_team_run_snapshots
  ADD CONSTRAINT weave_team_run_snapshots_workspace_run_key
    UNIQUE (workspace_id, run_id),
  ADD CONSTRAINT weave_team_run_snapshots_schema_mode_check CHECK (
    (
      snapshot_schema_version = 1
      AND mode IS NULL
      AND artifact_workflow_id IS NULL
      AND artifact_workflow_version IS NULL
      AND trigger_source_v2 IS NULL
    )
    OR (
      snapshot_schema_version = 2
      AND mode IS NOT NULL
      AND mode IN ('fixed_workflow', 'free_collab')
    )
  ),
  ADD CONSTRAINT weave_team_run_snapshots_worker_versions_check CHECK (
    worker_versions IS NULL
    OR jsonb_typeof(worker_versions) IS NOT DISTINCT FROM 'object'
  ),
  ADD CONSTRAINT weave_team_run_snapshots_team_workers_check CHECK (
    team_worker_snapshot IS NULL
    OR jsonb_typeof(team_worker_snapshot) IS NOT DISTINCT FROM 'array'
  ),
  ADD CONSTRAINT weave_team_run_snapshots_fixed_mode_check CHECK (
    snapshot_schema_version <> 2
    OR mode <> 'fixed_workflow'
    OR (
      workflow_id IS NOT NULL
      AND workflow_id <> ''
      AND workflow_version IS NOT NULL
      AND workflow_version > 0
      AND artifact_workflow_id IS NOT NULL
      AND artifact_workflow_id <> ''
      AND artifact_workflow_version IS NOT NULL
      AND artifact_workflow_version > 0
      AND workflow_id = artifact_workflow_id
      AND workflow_version = artifact_workflow_version
      AND lead_avatar_id IS NULL
      AND lead_avatar_version IS NULL
      AND worker_versions IS NULL
      AND team_worker_snapshot IS NULL
      AND inline_dependencies IS NULL
      AND artifact_ref IS NULL
    )
  ),
  ADD CONSTRAINT weave_team_run_snapshots_free_mode_check CHECK (
    snapshot_schema_version <> 2
    OR mode <> 'free_collab'
    OR (
      workflow_id IS NULL
      AND workflow_version IS NULL
      AND artifact_workflow_id IS NULL
      AND artifact_workflow_version IS NULL
      AND lead_avatar_id IS NOT NULL
      AND lead_avatar_id <> ''
      AND lead_avatar_version IS NOT NULL
      AND lead_avatar_version > 0
      AND worker_versions IS NOT NULL
      AND team_worker_snapshot IS NOT NULL
      AND inline_dependencies IS NOT NULL
      AND artifact_ref IS NULL
    )
  ),
  ADD CONSTRAINT weave_team_run_snapshots_artifact_workflow_fk
    FOREIGN KEY (
      workspace_id,
      artifact_workflow_id,
      artifact_workflow_version
    )
    REFERENCES weave_published_artifact_contents (
      workspace_id,
      workflow_id,
      workflow_version
    ) ON DELETE RESTRICT;

CREATE INDEX idx_weave_team_run_snapshots_artifact_workflow
  ON weave_team_run_snapshots (
    workspace_id,
    artifact_workflow_id,
    artifact_workflow_version
  );

CREATE FUNCTION weave_team_run_snapshots_is_rfc3339(value TEXT)
RETURNS BOOLEAN
LANGUAGE plpgsql
IMMUTABLE
STRICT
AS $$
DECLARE
  parts TEXT[];
  year_value INT;
  month_value INT;
  day_value INT;
  hour_value INT;
  minute_value INT;
  second_value INT;
  zone_hour_value INT;
  zone_minute_value INT;
BEGIN
  parts := regexp_match(
    value,
    '^([0-9]{4})-([0-9]{2})-([0-9]{2})T([0-9]{2}):([0-9]{2}):([0-9]{2})(\.[0-9]+)?(Z|[+-]([0-9]{2}):([0-9]{2}))$'
  );
  IF parts IS NULL THEN
    RETURN false;
  END IF;

  year_value := parts[1]::INT;
  month_value := parts[2]::INT;
  day_value := parts[3]::INT;
  hour_value := parts[4]::INT;
  minute_value := parts[5]::INT;
  second_value := parts[6]::INT;
  zone_hour_value := COALESCE(parts[9]::INT, 0);
  zone_minute_value := COALESCE(parts[10]::INT, 0);
  IF year_value < 1
     OR hour_value > 23
     OR minute_value > 59
     OR second_value > 59
     OR zone_hour_value > 23
     OR zone_minute_value > 59 THEN
    RETURN false;
  END IF;

  PERFORM make_date(year_value, month_value, day_value);
  RETURN true;
EXCEPTION
  WHEN others THEN
    RETURN false;
END;
$$;

ALTER TABLE weave_team_run_snapshots
  ADD CONSTRAINT weave_team_run_snapshots_admission_v2_check CHECK (
    snapshot_schema_version <> 2
    OR mode IS NULL
    OR mode NOT IN ('fixed_workflow', 'free_collab')
    OR (
      jsonb_typeof(admission_decision) IS NOT DISTINCT FROM 'object'
      AND admission_decision ?& ARRAY[
        'schema_version',
        'team_active',
        'workflow_active',
        'workers_enabled',
        'version_blocked',
        'decided_at'
      ]
      AND admission_decision - ARRAY[
        'schema_version',
        'team_active',
        'workflow_active',
        'workers_enabled',
        'version_blocked',
        'decided_at'
      ] = '{}'::jsonb
      AND jsonb_typeof(admission_decision->'schema_version')
        IS NOT DISTINCT FROM 'number'
      AND admission_decision->'schema_version' = '1'::jsonb
      AND admission_decision->'team_active' = 'true'::jsonb
      AND admission_decision->'workers_enabled' = 'true'::jsonb
      AND jsonb_typeof(admission_decision->'decided_at')
        IS NOT DISTINCT FROM 'string'
      AND weave_team_run_snapshots_is_rfc3339(
        admission_decision->>'decided_at'
      )
      AND (
        (
          mode = 'fixed_workflow'
          AND admission_decision->'workflow_active' = 'true'::jsonb
          AND admission_decision->'version_blocked' = 'false'::jsonb
        )
        OR (
          mode = 'free_collab'
          AND admission_decision->'workflow_active' = 'null'::jsonb
          AND admission_decision->'version_blocked' = 'null'::jsonb
        )
      )
    )
  ),
  ADD CONSTRAINT weave_team_run_snapshots_run_associations_v2_check CHECK (
    snapshot_schema_version <> 2
    OR (
      jsonb_typeof(run_associations) IS NOT DISTINCT FROM 'object'
      AND run_associations ?& ARRAY[
        'schema_version',
        'parent_run_id',
        'source_snapshot_id',
        'task_group_id'
      ]
      AND run_associations - ARRAY[
        'schema_version',
        'parent_run_id',
        'source_snapshot_id',
        'task_group_id'
      ] = '{}'::jsonb
      AND jsonb_typeof(run_associations->'schema_version')
        IS NOT DISTINCT FROM 'number'
      AND run_associations->'schema_version' = '1'::jsonb
      AND (
        run_associations->'parent_run_id' = 'null'::jsonb
        OR (
          jsonb_typeof(run_associations->'parent_run_id')
            IS NOT DISTINCT FROM 'string'
          AND run_associations->>'parent_run_id' <> ''
        )
      )
      AND (
        run_associations->'source_snapshot_id' = 'null'::jsonb
        OR (
          jsonb_typeof(run_associations->'source_snapshot_id')
            IS NOT DISTINCT FROM 'string'
          AND run_associations->>'source_snapshot_id' <> ''
        )
      )
      AND (
        run_associations->'task_group_id' = 'null'::jsonb
        OR (
          jsonb_typeof(run_associations->'task_group_id')
            IS NOT DISTINCT FROM 'string'
          AND run_associations->>'task_group_id' <> ''
        )
      )
    )
  ),
  ADD CONSTRAINT weave_team_run_snapshots_trigger_source_v2_check CHECK (
    snapshot_schema_version <> 2
    OR (
      jsonb_typeof(trigger_source_v2) IS NOT DISTINCT FROM 'object'
      AND trigger_source_v2 ?& ARRAY[
        'schema_version',
        'type',
        'source_ref'
      ]
      AND trigger_source_v2 - ARRAY[
        'schema_version',
        'type',
        'source_ref',
        'occurrence_key'
      ] = '{}'::jsonb
      AND jsonb_typeof(trigger_source_v2->'schema_version')
        IS NOT DISTINCT FROM 'number'
      AND trigger_source_v2->'schema_version' = '1'::jsonb
      AND jsonb_typeof(trigger_source_v2->'type')
        IS NOT DISTINCT FROM 'string'
      AND trigger_source_v2->>'type' IN (
        'conversation_explicit',
        'conversation_auto',
        'schedule',
        'api',
        'event',
        'fanout_synthesis'
      )
      AND trigger_source = trigger_source_v2->>'type'
      AND jsonb_typeof(trigger_source_v2->'source_ref')
        IS NOT DISTINCT FROM 'string'
      AND trigger_source_v2->>'source_ref' <> ''
      AND (
        (
          trigger_source_v2->>'type' = 'schedule'
          AND jsonb_typeof(trigger_source_v2->'occurrence_key')
            IS NOT DISTINCT FROM 'string'
          AND trigger_source_v2->>'occurrence_key' ~ '^[0-9a-f]{64}$'
        )
        OR (
          trigger_source_v2->>'type' <> 'schedule'
          AND (
            NOT (trigger_source_v2 ? 'occurrence_key')
            OR trigger_source_v2->'occurrence_key' = 'null'::jsonb
          )
        )
      )
    )
  );

CREATE FUNCTION weave_team_run_snapshots_require_v2_insert()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.snapshot_schema_version IS DISTINCT FROM 2 THEN
    RAISE EXCEPTION 'new team run snapshots must use schema version 2'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER weave_team_run_snapshots_require_v2_insert
BEFORE INSERT ON weave_team_run_snapshots
FOR EACH ROW
EXECUTE FUNCTION weave_team_run_snapshots_require_v2_insert();

CREATE TRIGGER weave_team_run_snapshots_immutable
BEFORE UPDATE OR DELETE ON weave_team_run_snapshots
FOR EACH ROW
EXECUTE FUNCTION weave_team_run_snapshots_reject_mutation();
