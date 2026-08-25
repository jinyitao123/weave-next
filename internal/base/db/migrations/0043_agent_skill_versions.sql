LOCK TABLE
  weave_agent_versions,
  weave_task_queue,
  weave_team_run_snapshots
IN ACCESS EXCLUSIVE MODE;

DROP TRIGGER weave_agent_versions_immutable
  ON weave_agent_versions;

ALTER TABLE weave_agent_versions
  ADD COLUMN workspace_id TEXT;

UPDATE weave_agent_versions AS version
SET workspace_id = agent.workspace_id
FROM weave_agents AS agent
WHERE agent.id = version.agent_id;

DO $$
BEGIN
  IF EXISTS (
    SELECT 1
    FROM weave_agent_versions
    WHERE workspace_id IS NULL
  ) THEN
    RAISE EXCEPTION
      'agent version workspace backfill found orphan or null workspace'
      USING ERRCODE = '23514';
  END IF;

  IF EXISTS (
    SELECT 1
    FROM weave_task_queue AS task
    JOIN weave_agent_versions AS version
      ON version.agent_id = task.agent_id
     AND version.version = task.agent_version
    WHERE task.agent_id IS NOT NULL
      AND task.agent_version IS NOT NULL
      AND task.workspace_id IS DISTINCT FROM version.workspace_id
  ) THEN
    RAISE EXCEPTION
      'task agent version workspace does not match backfilled version workspace'
      USING ERRCODE = '23514';
  END IF;

  IF EXISTS (
    SELECT 1
    FROM weave_team_run_snapshots AS snapshot
    JOIN weave_agent_versions AS version
      ON version.agent_id = snapshot.lead_avatar_id
     AND version.version = snapshot.lead_avatar_version
    WHERE snapshot.lead_avatar_id IS NOT NULL
      AND snapshot.lead_avatar_version IS NOT NULL
      AND snapshot.workspace_id IS DISTINCT FROM version.workspace_id
  ) THEN
    RAISE EXCEPTION
      'snapshot lead version workspace does not match backfilled version workspace'
      USING ERRCODE = '23514';
  END IF;
END;
$$;

DO $$
DECLARE
  existing_constraint RECORD;
BEGIN
  FOR existing_constraint IN
    SELECT
      constraint_row.conname,
      constraint_row.conrelid::regclass::text AS relation_name,
      CASE
        WHEN constraint_row.contype = 'f'
          AND constraint_row.confrelid = 'weave_agent_versions'::regclass
          THEN 1
        WHEN constraint_row.contype = 'f'
          AND constraint_row.conrelid = 'weave_agent_versions'::regclass
          THEN 2
        ELSE 3
      END AS drop_order
    FROM pg_constraint AS constraint_row
    WHERE (
      constraint_row.contype = 'f'
      AND constraint_row.confrelid = 'weave_agent_versions'::regclass
      AND constraint_row.conrelid IN (
        'weave_task_queue'::regclass,
        'weave_team_run_snapshots'::regclass
      )
    ) OR (
      constraint_row.contype = 'f'
      AND constraint_row.conrelid = 'weave_agent_versions'::regclass
      AND constraint_row.confrelid = 'weave_agents'::regclass
    ) OR (
      constraint_row.contype = 'p'
      AND constraint_row.conrelid = 'weave_agent_versions'::regclass
    )
    ORDER BY drop_order, constraint_row.conname
  LOOP
    EXECUTE format(
      'ALTER TABLE %s DROP CONSTRAINT %I',
      existing_constraint.relation_name,
      existing_constraint.conname
    );
  END LOOP;
END;
$$;

ALTER TABLE weave_agent_versions
  ALTER COLUMN workspace_id SET NOT NULL,
  ADD CONSTRAINT weave_agent_versions_pkey
    PRIMARY KEY (workspace_id, agent_id, version),
  ADD CONSTRAINT weave_agent_versions_agent_fk
    FOREIGN KEY (workspace_id, agent_id)
    REFERENCES weave_agents (workspace_id, id) ON DELETE RESTRICT,
  ADD CONSTRAINT weave_agent_versions_version_check
    CHECK (version > 0);

ALTER TABLE weave_task_queue
  ADD CONSTRAINT weave_task_queue_agent_version_fk
    FOREIGN KEY (workspace_id, agent_id, agent_version)
    REFERENCES weave_agent_versions (
      workspace_id,
      agent_id,
      version
    ) ON DELETE RESTRICT;

ALTER TABLE weave_team_run_snapshots
  ADD CONSTRAINT weave_team_run_snapshots_lead_version_fk
    FOREIGN KEY (workspace_id, lead_avatar_id, lead_avatar_version)
    REFERENCES weave_agent_versions (
      workspace_id,
      agent_id,
      version
    ) ON DELETE RESTRICT;

CREATE INDEX idx_weave_task_queue_agent_version_fk
  ON weave_task_queue (workspace_id, agent_id, agent_version)
  WHERE agent_id IS NOT NULL AND agent_version IS NOT NULL;

CREATE INDEX idx_weave_team_run_snapshots_lead_version_fk
  ON weave_team_run_snapshots (
    workspace_id,
    lead_avatar_id,
    lead_avatar_version
  )
  WHERE lead_avatar_id IS NOT NULL AND lead_avatar_version IS NOT NULL;

CREATE OR REPLACE FUNCTION weave_agent_versions_reject_update()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'agent version records are immutable'
    USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER weave_agent_versions_immutable
BEFORE UPDATE OR DELETE ON weave_agent_versions
FOR EACH ROW
EXECUTE FUNCTION weave_agent_versions_reject_update();

-- Task 4C appends immutable SkillVersion storage here. This AgentVersion
-- migration intentionally does not inspect, import, or mutate Loom KV Skills.

CREATE TABLE weave_skills (
  workspace_id TEXT NOT NULL,
  id TEXT NOT NULL,
  name TEXT NOT NULL,
  latest_version BIGINT NOT NULL DEFAULT 0,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, id),
  UNIQUE (workspace_id, name),
  CONSTRAINT weave_skills_workspace_fk
    FOREIGN KEY (workspace_id)
    REFERENCES weave_workspaces (id) ON DELETE RESTRICT,
  CONSTRAINT weave_skills_latest_version_check
    CHECK (latest_version BETWEEN 0 AND 9007199254740991)
);

CREATE TABLE weave_skill_versions (
  workspace_id TEXT NOT NULL,
  skill_id TEXT NOT NULL,
  version BIGINT NOT NULL,
  name TEXT NOT NULL,
  description TEXT NOT NULL,
  body TEXT NOT NULL,
  always_active BOOLEAN NOT NULL,
  resources JSONB NOT NULL,
  content_hash TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, skill_id, version),
  CONSTRAINT weave_skill_versions_skill_fk
    FOREIGN KEY (workspace_id, skill_id)
    REFERENCES weave_skills (workspace_id, id) ON DELETE RESTRICT,
  CONSTRAINT weave_skill_versions_version_check
    CHECK (version BETWEEN 1 AND 9007199254740991),
  CONSTRAINT weave_skill_versions_resources_check
    CHECK (jsonb_typeof(resources) = 'array'),
  CONSTRAINT weave_skill_versions_content_hash_check
    CHECK (content_hash ~ '^[0-9a-f]{64}$')
);

CREATE FUNCTION weave_skill_versions_reject_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'skill version records are immutable'
    USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER weave_skill_versions_immutable
BEFORE UPDATE OR DELETE ON weave_skill_versions
FOR EACH ROW
EXECUTE FUNCTION weave_skill_versions_reject_mutation();

CREATE TABLE weave_skill_import_receipts (
  workspace_id TEXT NOT NULL,
  idempotency_key TEXT NOT NULL,
  request_hash TEXT NOT NULL,
  response JSONB NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, idempotency_key),
  CONSTRAINT weave_skill_import_receipts_workspace_fk
    FOREIGN KEY (workspace_id)
    REFERENCES weave_workspaces (id) ON DELETE RESTRICT,
  CONSTRAINT weave_skill_import_receipts_request_hash_check
    CHECK (request_hash ~ '^[0-9a-f]{64}$'),
  CONSTRAINT weave_skill_import_receipts_response_check CHECK (
    jsonb_typeof(response) IS NOT DISTINCT FROM 'object'
    AND response ?& ARRAY[
      'schema_version', 'skill_id', 'version', 'changed',
      'source_hash', 'content_hash'
    ]
    AND response - ARRAY[
      'schema_version', 'skill_id', 'version', 'changed',
      'source_hash', 'content_hash'
    ] = '{}'::jsonb
    AND (response->'schema_version')::text = '1'
    AND jsonb_typeof(response->'skill_id') IS NOT DISTINCT FROM 'string'
    AND response->>'skill_id' <> ''
    AND jsonb_typeof(response->'version') IS NOT DISTINCT FROM 'number'
    AND (response->'version')::text ~ '^[1-9][0-9]*$'
    AND (response->>'version')::bigint BETWEEN 1 AND 9007199254740991
    AND jsonb_typeof(response->'changed') IS NOT DISTINCT FROM 'boolean'
    AND jsonb_typeof(response->'source_hash') IS NOT DISTINCT FROM 'string'
    AND response->>'source_hash' ~ '^[0-9a-f]{64}$'
    AND jsonb_typeof(response->'content_hash') IS NOT DISTINCT FROM 'string'
    AND response->>'content_hash' ~ '^[0-9a-f]{64}$'
  )
);

CREATE FUNCTION weave_skill_import_receipts_reject_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'skill import receipt is append-only'
    USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER weave_skill_import_receipts_append_only
BEFORE UPDATE OR DELETE ON weave_skill_import_receipts
FOR EACH ROW
EXECUTE FUNCTION weave_skill_import_receipts_reject_mutation();

CREATE TABLE weave_skill_import_audits (
  workspace_id TEXT NOT NULL,
  audit_id TEXT NOT NULL,
  idempotency_key TEXT NOT NULL,
  skill_id TEXT NOT NULL,
  operator_id TEXT NOT NULL,
  reason TEXT NOT NULL,
  request_hash TEXT NOT NULL,
  source_hash TEXT NOT NULL,
  version BIGINT NOT NULL,
  changed BOOLEAN NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, audit_id),
  CONSTRAINT weave_skill_import_audits_receipt_fk
    FOREIGN KEY (workspace_id, idempotency_key)
    REFERENCES weave_skill_import_receipts (workspace_id, idempotency_key)
    ON DELETE RESTRICT,
  CONSTRAINT weave_skill_import_audits_skill_fk
    FOREIGN KEY (workspace_id, skill_id)
    REFERENCES weave_skills (workspace_id, id) ON DELETE RESTRICT,
  CONSTRAINT weave_skill_import_audits_request_hash_check
    CHECK (request_hash ~ '^[0-9a-f]{64}$'),
  CONSTRAINT weave_skill_import_audits_source_hash_check
    CHECK (source_hash ~ '^[0-9a-f]{64}$'),
  CONSTRAINT weave_skill_import_audits_version_check
    CHECK (version BETWEEN 1 AND 9007199254740991)
);

CREATE FUNCTION weave_skill_import_audits_reject_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'skill import audit is append-only'
    USING ERRCODE = '23514';
END;
$$;

CREATE FUNCTION weave_skill_import_audits_validate_receipt()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
  frozen_hash TEXT;
  frozen_response JSONB;
BEGIN
  SELECT request_hash, response INTO frozen_hash, frozen_response
  FROM weave_skill_import_receipts
  WHERE workspace_id = NEW.workspace_id
    AND idempotency_key = NEW.idempotency_key;
  IF NOT FOUND THEN
    RETURN NEW;
  END IF;
  IF frozen_hash IS DISTINCT FROM NEW.request_hash
     OR frozen_response->>'skill_id' IS DISTINCT FROM NEW.skill_id
     OR (frozen_response->>'version')::bigint IS DISTINCT FROM NEW.version
     OR (frozen_response->>'changed')::boolean IS DISTINCT FROM NEW.changed
     OR frozen_response->>'source_hash' IS DISTINCT FROM NEW.source_hash THEN
    RAISE EXCEPTION 'skill import audit must match frozen receipt'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER weave_skill_import_audits_validate_receipt
BEFORE INSERT ON weave_skill_import_audits
FOR EACH ROW
EXECUTE FUNCTION weave_skill_import_audits_validate_receipt();

CREATE TRIGGER weave_skill_import_audits_append_only
BEFORE UPDATE OR DELETE ON weave_skill_import_audits
FOR EACH ROW
EXECUTE FUNCTION weave_skill_import_audits_reject_mutation();
