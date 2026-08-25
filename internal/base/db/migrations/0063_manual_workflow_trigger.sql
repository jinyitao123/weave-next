ALTER TABLE weave_team_run_snapshots
  DROP CONSTRAINT weave_team_run_snapshots_trigger_source_v2_check,
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
        'manual',
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
          trigger_source_v2->>'type' = 'manual'
          AND NOT (trigger_source_v2 ? 'occurrence_key')
        )
        OR (
          trigger_source_v2->>'type' NOT IN ('schedule', 'manual')
          AND (
            NOT (trigger_source_v2 ? 'occurrence_key')
            OR trigger_source_v2->'occurrence_key' = 'null'::jsonb
          )
        )
      )
    )
  );

ALTER TABLE weave_workflow_admission_denials
  DROP CONSTRAINT weave_workflow_admission_denials_trigger_type_check,
  ADD CONSTRAINT weave_workflow_admission_denials_trigger_type_check
    CHECK (trigger_type IN ('schedule', 'manual', 'api', 'event', 'session'));

ALTER TABLE weave_team_runs
  DROP CONSTRAINT weave_team_runs_source_kind_check,
  ADD CONSTRAINT weave_team_runs_source_kind_check CHECK (
    source_kind IN ('session', 'schedule', 'manual', 'api', 'event')
  );
