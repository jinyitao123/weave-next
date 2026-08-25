CREATE FUNCTION weave_team_roster_kinds_valid(value JSONB)
RETURNS BOOLEAN
LANGUAGE sql
IMMUTABLE
AS $$
  SELECT value IN (
    '["consult"]'::jsonb,
    '["dispatch"]'::jsonb,
    '["handoff"]'::jsonb,
    '["consult", "dispatch"]'::jsonb,
    '["consult", "handoff"]'::jsonb,
    '["dispatch", "handoff"]'::jsonb,
    '["consult", "dispatch", "handoff"]'::jsonb
  );
$$;

CREATE FUNCTION weave_team_roster_authorization_projection_valid(value JSONB)
RETURNS BOOLEAN
LANGUAGE sql
IMMUTABLE
AS $$
  SELECT
    jsonb_typeof(value) IS NOT DISTINCT FROM 'array'
    AND NOT EXISTS (
      SELECT 1
      FROM jsonb_array_elements(value) WITH ORDINALITY AS worker(item, ordinal)
      WHERE jsonb_typeof(item) IS DISTINCT FROM 'object'
         OR NOT item ?& ARRAY['worker_agent_id', 'allowed_kinds', 'default_kind', 'enabled']
         OR item - ARRAY['worker_agent_id', 'allowed_kinds', 'default_kind', 'enabled'] <> '{}'::jsonb
         OR jsonb_typeof(item->'worker_agent_id') IS DISTINCT FROM 'string'
         OR item->>'worker_agent_id' = ''
         OR NOT weave_team_roster_kinds_valid(item->'allowed_kinds')
         OR jsonb_typeof(item->'default_kind') IS DISTINCT FROM 'string'
         OR NOT (item->'allowed_kinds' @> jsonb_build_array(item->>'default_kind'))
         OR jsonb_typeof(item->'enabled') IS DISTINCT FROM 'boolean'
    )
    AND NOT EXISTS (
      SELECT 1
      FROM (
        SELECT
          item->>'worker_agent_id' AS worker_id,
          lag(item->>'worker_agent_id') OVER (ORDER BY ordinal) AS previous_worker_id
        FROM jsonb_array_elements(value) WITH ORDINALITY AS worker(item, ordinal)
      ) ordered_workers
      WHERE previous_worker_id IS NOT NULL
        AND previous_worker_id COLLATE "C" >= worker_id COLLATE "C"
    );
$$;

CREATE FUNCTION weave_team_roster_workers_valid(value JSONB)
RETURNS BOOLEAN
LANGUAGE sql
IMMUTABLE
AS $$
  SELECT
    jsonb_typeof(value) IS NOT DISTINCT FROM 'array'
    AND cardinality(ARRAY(SELECT 1 FROM jsonb_array_elements(value))) > 0
    AND NOT EXISTS (
      SELECT 1
      FROM jsonb_array_elements(value) WITH ORDINALITY AS worker(item, ordinal)
      WHERE jsonb_typeof(item) IS DISTINCT FROM 'object'
         OR NOT item ?& ARRAY[
           'worker_agent_id', 'duty', 'when_to_use', 'context_instruction',
           'allowed_kinds', 'default_kind', 'result_requirement', 'enabled'
         ]
         OR item - ARRAY[
           'worker_agent_id', 'duty', 'when_to_use', 'context_instruction',
           'allowed_kinds', 'default_kind', 'result_requirement', 'enabled'
         ] <> '{}'::jsonb
         OR jsonb_typeof(item->'worker_agent_id') IS DISTINCT FROM 'string'
         OR item->>'worker_agent_id' = ''
         OR jsonb_typeof(item->'duty') IS DISTINCT FROM 'string'
         OR jsonb_typeof(item->'when_to_use') IS DISTINCT FROM 'string'
         OR jsonb_typeof(item->'context_instruction') IS DISTINCT FROM 'string'
         OR NOT weave_team_roster_kinds_valid(item->'allowed_kinds')
         OR jsonb_typeof(item->'default_kind') IS DISTINCT FROM 'string'
         OR NOT (item->'allowed_kinds' @> jsonb_build_array(item->>'default_kind'))
         OR jsonb_typeof(item->'result_requirement') IS DISTINCT FROM 'string'
         OR jsonb_typeof(item->'enabled') IS DISTINCT FROM 'boolean'
    )
    AND NOT EXISTS (
      SELECT 1
      FROM (
        SELECT
          item->>'worker_agent_id' AS worker_id,
          lag(item->>'worker_agent_id') OVER (ORDER BY ordinal) AS previous_worker_id
        FROM jsonb_array_elements(value) WITH ORDINALITY AS worker(item, ordinal)
      ) ordered_workers
      WHERE previous_worker_id IS NOT NULL
        AND previous_worker_id COLLATE "C" >= worker_id COLLATE "C"
    );
$$;

CREATE FUNCTION weave_team_roster_affected_workers_valid(value JSONB)
RETURNS BOOLEAN
LANGUAGE sql
IMMUTABLE
AS $$
  SELECT
    jsonb_typeof(value) IS NOT DISTINCT FROM 'array'
    AND NOT EXISTS (
      SELECT 1
      FROM jsonb_array_elements(value) WITH ORDINALITY AS affected(item, ordinal)
      WHERE jsonb_typeof(item) IS DISTINCT FROM 'object'
         OR NOT item ?& ARRAY[
           'worker_agent_id', 'affected_published_version_count', 'revocation_impact_url'
         ]
         OR item - ARRAY[
           'worker_agent_id', 'affected_published_version_count', 'revocation_impact_url'
         ] <> '{}'::jsonb
         OR jsonb_typeof(item->'worker_agent_id') IS DISTINCT FROM 'string'
         OR item->>'worker_agent_id' = ''
         OR jsonb_typeof(item->'affected_published_version_count') IS DISTINCT FROM 'number'
         OR (item->>'affected_published_version_count') !~ '^(0|[1-9][0-9]*)$'
         OR jsonb_typeof(item->'revocation_impact_url') IS DISTINCT FROM 'string'
         OR item->>'revocation_impact_url' = ''
    )
    AND NOT EXISTS (
      SELECT 1
      FROM (
        SELECT
          item->>'worker_agent_id' AS worker_id,
          lag(item->>'worker_agent_id') OVER (ORDER BY ordinal) AS previous_worker_id
        FROM jsonb_array_elements(value) WITH ORDINALITY AS affected(item, ordinal)
      ) ordered_workers
      WHERE previous_worker_id IS NOT NULL
        AND previous_worker_id COLLATE "C" >= worker_id COLLATE "C"
    );
$$;

CREATE TABLE weave_team_roster_receipts (
  workspace_id TEXT NOT NULL,
  team_id TEXT NOT NULL,
  idempotency_key TEXT NOT NULL,
  request_hash TEXT NOT NULL,
  response JSONB NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, team_id, idempotency_key),
  CONSTRAINT weave_team_roster_receipts_team_fk
    FOREIGN KEY (workspace_id, team_id)
    REFERENCES weave_teams (workspace_id, id) ON DELETE RESTRICT,
  CONSTRAINT weave_team_roster_receipts_idempotency_key_check
    CHECK (idempotency_key <> '' AND idempotency_key = btrim(idempotency_key)),
  CONSTRAINT weave_team_roster_receipts_request_hash_check
    CHECK (request_hash ~ '^[0-9a-f]{64}$'),
  CONSTRAINT weave_team_roster_receipts_response_check CHECK (
    jsonb_typeof(response) IS NOT DISTINCT FROM 'object'
    AND response ?& ARRAY[
      'schema_version', 'team_id', 'team_status', 'lead_agent_id',
      'updated_at', 'workers', 'changed', 'audit_id', 'affected_workers'
    ]
    AND response - ARRAY[
      'schema_version', 'team_id', 'team_status', 'lead_agent_id',
      'updated_at', 'workers', 'changed', 'audit_id', 'affected_workers'
    ] = '{}'::jsonb
    AND jsonb_typeof(response->'schema_version') IS NOT DISTINCT FROM 'number'
    AND (response->'schema_version')::text = '1'
    AND jsonb_typeof(response->'team_id') IS NOT DISTINCT FROM 'string'
    AND response->>'team_id' = team_id
    AND response->>'team_status' IN ('active', 'archived')
    AND jsonb_typeof(response->'lead_agent_id') IS NOT DISTINCT FROM 'string'
    AND response->>'lead_agent_id' <> ''
    AND jsonb_typeof(response->'updated_at') IS NOT DISTINCT FROM 'string'
    AND response->>'updated_at' ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{6}Z$'
    AND weave_team_roster_workers_valid(response->'workers')
    AND jsonb_typeof(response->'changed') IS NOT DISTINCT FROM 'boolean'
    AND weave_team_roster_affected_workers_valid(response->'affected_workers')
    AND (
      (
        response->'changed' = 'true'::jsonb
        AND jsonb_typeof(response->'audit_id') IS NOT DISTINCT FROM 'string'
        AND response->>'audit_id' <> ''
      ) OR (
        response->'changed' = 'false'::jsonb
        AND response->'audit_id' = 'null'::jsonb
        AND response->'affected_workers' = '[]'::jsonb
      )
    )
  )
);

CREATE FUNCTION weave_team_roster_receipts_reject_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'team roster receipt is append-only'
    USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER weave_team_roster_receipts_append_only
BEFORE UPDATE OR DELETE ON weave_team_roster_receipts
FOR EACH ROW
EXECUTE FUNCTION weave_team_roster_receipts_reject_mutation();

CREATE TABLE weave_team_roster_audits (
  workspace_id TEXT NOT NULL,
  team_id TEXT NOT NULL,
  audit_id TEXT NOT NULL,
  idempotency_key TEXT NOT NULL,
  old_team_status TEXT NOT NULL,
  new_team_status TEXT NOT NULL,
  old_lead_agent_id TEXT,
  new_lead_agent_id TEXT NOT NULL,
  old_workers JSONB NOT NULL,
  new_workers JSONB NOT NULL,
  operator_id TEXT NOT NULL,
  reason TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, team_id, audit_id),
  UNIQUE (workspace_id, team_id, idempotency_key),
  CONSTRAINT weave_team_roster_audits_receipt_fk
    FOREIGN KEY (workspace_id, team_id, idempotency_key)
    REFERENCES weave_team_roster_receipts (
      workspace_id,
      team_id,
      idempotency_key
    ) ON DELETE RESTRICT,
  CONSTRAINT weave_team_roster_audits_status_check CHECK (
    old_team_status IN ('active', 'archived', 'needs_repair')
    AND new_team_status IN ('active', 'archived')
  ),
  CONSTRAINT weave_team_roster_audits_lead_check CHECK (
    (old_lead_agent_id IS NULL OR old_lead_agent_id <> '')
    AND new_lead_agent_id <> ''
  ),
  CONSTRAINT weave_team_roster_audits_workers_check CHECK (
    weave_team_roster_authorization_projection_valid(old_workers)
    AND weave_team_roster_authorization_projection_valid(new_workers)
    AND jsonb_array_length(new_workers) > 0
  ),
  CONSTRAINT weave_team_roster_audits_operator_check
    CHECK (operator_id <> '' AND operator_id = btrim(operator_id)),
  CONSTRAINT weave_team_roster_audits_reason_check
    CHECK (reason <> '' AND reason = btrim(reason))
);

CREATE INDEX weave_team_roster_audits_list_idx
  ON weave_team_roster_audits (
    workspace_id,
    team_id,
    created_at,
    audit_id COLLATE "C"
  );

CREATE FUNCTION weave_team_roster_audits_validate_receipt()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
  frozen_response JSONB;
  frozen_projection JSONB;
BEGIN
  SELECT response INTO frozen_response
  FROM weave_team_roster_receipts
  WHERE workspace_id = NEW.workspace_id
    AND team_id = NEW.team_id
    AND idempotency_key = NEW.idempotency_key;

  IF NOT FOUND THEN
    RETURN NEW;
  END IF;

  SELECT COALESCE(
    jsonb_agg(
      jsonb_build_object(
        'worker_agent_id', item->'worker_agent_id',
        'allowed_kinds', item->'allowed_kinds',
        'default_kind', item->'default_kind',
        'enabled', item->'enabled'
      ) ORDER BY ordinal
    ),
    '[]'::jsonb
  ) INTO frozen_projection
  FROM jsonb_array_elements(frozen_response->'workers')
    WITH ORDINALITY AS worker(item, ordinal);

  IF frozen_response->'changed' IS DISTINCT FROM 'true'::jsonb
     OR frozen_response->>'audit_id' IS DISTINCT FROM NEW.audit_id
     OR frozen_response->>'team_status' IS DISTINCT FROM NEW.new_team_status
     OR frozen_response->>'lead_agent_id' IS DISTINCT FROM NEW.new_lead_agent_id
     OR frozen_projection IS DISTINCT FROM NEW.new_workers THEN
    RAISE EXCEPTION 'team roster audit must match frozen receipt'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER weave_team_roster_audits_validate_receipt
BEFORE INSERT ON weave_team_roster_audits
FOR EACH ROW
EXECUTE FUNCTION weave_team_roster_audits_validate_receipt();

CREATE FUNCTION weave_team_roster_audits_reject_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'team roster audit is append-only'
    USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER weave_team_roster_audits_append_only
BEFORE UPDATE OR DELETE ON weave_team_roster_audits
FOR EACH ROW
EXECUTE FUNCTION weave_team_roster_audits_reject_mutation();

CREATE FUNCTION weave_teams_advance_updated_at()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  NEW.updated_at := GREATEST(
    statement_timestamp(),
    OLD.updated_at + interval '1 microsecond'
  );
  RETURN NEW;
END;
$$;

CREATE TRIGGER weave_teams_advance_updated_at
BEFORE UPDATE ON weave_teams
FOR EACH ROW
EXECUTE FUNCTION weave_teams_advance_updated_at();
