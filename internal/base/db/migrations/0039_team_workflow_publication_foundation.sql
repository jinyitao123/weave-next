CREATE TABLE weave_team_workflows (
  workspace_id TEXT NOT NULL,
  id TEXT NOT NULL,
  team_id TEXT NOT NULL,
  name TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'active',
  published_version INT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, id),
  CONSTRAINT weave_team_workflows_status_check
    CHECK (status IN ('active', 'archived')),
  CONSTRAINT weave_team_workflows_published_version_check
    CHECK (published_version IS NULL OR published_version > 0),
  CONSTRAINT weave_team_workflows_team_fk
    FOREIGN KEY (workspace_id, team_id)
    REFERENCES weave_teams (workspace_id, id) ON DELETE RESTRICT
);

CREATE TABLE weave_team_workflow_versions (
  workspace_id TEXT NOT NULL,
  workflow_id TEXT NOT NULL,
  version INT NOT NULL,
  status TEXT NOT NULL DEFAULT 'draft',
  trigger_config JSONB NOT NULL,
  graph_definition JSONB NOT NULL,
  created_by TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  published_at TIMESTAMPTZ,
  PRIMARY KEY (workspace_id, workflow_id, version),
  CONSTRAINT weave_team_workflow_versions_workflow_fk
    FOREIGN KEY (workspace_id, workflow_id)
    REFERENCES weave_team_workflows (workspace_id, id) ON DELETE RESTRICT,
  CONSTRAINT weave_team_workflow_versions_version_check
    CHECK (version > 0),
  CONSTRAINT weave_team_workflow_versions_status_check
    CHECK (status IN ('draft', 'published')),
  CONSTRAINT weave_team_workflow_versions_published_at_check CHECK (
    (status = 'draft' AND published_at IS NULL)
    OR (status = 'published' AND published_at IS NOT NULL)
  ),
  CONSTRAINT weave_team_workflow_versions_trigger_config_check CHECK (
    jsonb_typeof(trigger_config) IS NOT DISTINCT FROM 'object'
    AND jsonb_typeof(trigger_config->'schema_version') IS NOT DISTINCT FROM 'number'
    AND trigger_config->'schema_version' = '1'::jsonb
  ),
  CONSTRAINT weave_team_workflow_versions_graph_definition_check CHECK (
    jsonb_typeof(graph_definition) IS NOT DISTINCT FROM 'object'
    AND jsonb_typeof(graph_definition->'schema_version') IS NOT DISTINCT FROM 'number'
    AND graph_definition->'schema_version' = '1'::jsonb
  )
);

CREATE UNIQUE INDEX uniq_weave_team_workflow_versions_draft
  ON weave_team_workflow_versions (workspace_id, workflow_id)
  WHERE status = 'draft';

ALTER TABLE weave_team_run_snapshots
  ADD CONSTRAINT weave_team_run_snapshots_workflow_version_fk
    FOREIGN KEY (workspace_id, workflow_id, workflow_version)
    REFERENCES weave_team_workflow_versions (
      workspace_id,
      workflow_id,
      version
    ) ON DELETE RESTRICT
    NOT VALID;

CREATE INDEX idx_weave_team_run_snapshots_workflow_version
  ON weave_team_run_snapshots (
    workspace_id,
    workflow_id,
    workflow_version
  );

ALTER TABLE weave_team_workflows
  ADD CONSTRAINT weave_team_workflows_published_version_fk
    FOREIGN KEY (workspace_id, id, published_version)
    REFERENCES weave_team_workflow_versions (
      workspace_id,
      workflow_id,
      version
    ) ON DELETE RESTRICT;

CREATE FUNCTION weave_team_workflows_require_published_pointer()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.published_version IS NULL THEN
    RETURN NEW;
  END IF;

  PERFORM 1
  FROM weave_team_workflow_versions
  WHERE workspace_id = NEW.workspace_id
    AND workflow_id = NEW.id
    AND version = NEW.published_version
    AND status = 'published';

  IF NOT FOUND THEN
    RAISE EXCEPTION 'workflow published_version must reference its own published version'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER weave_team_workflows_published_pointer_guard
BEFORE INSERT OR UPDATE OF workspace_id, id, published_version
ON weave_team_workflows
FOR EACH ROW
EXECUTE FUNCTION weave_team_workflows_require_published_pointer();

CREATE FUNCTION weave_team_workflow_versions_guard_publication()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'draft' THEN
      RAISE EXCEPTION 'workflow versions must be inserted as draft'
        USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
  END IF;

  IF TG_OP = 'DELETE' THEN
    IF OLD.status = 'published' THEN
      RAISE EXCEPTION 'published workflow versions are immutable'
        USING ERRCODE = '23514';
    END IF;
    RETURN OLD;
  END IF;

  IF OLD.status = 'published' THEN
    RAISE EXCEPTION 'published workflow versions are immutable'
      USING ERRCODE = '23514';
  END IF;

  IF OLD.workspace_id IS DISTINCT FROM NEW.workspace_id
     OR OLD.workflow_id IS DISTINCT FROM NEW.workflow_id
     OR OLD.version IS DISTINCT FROM NEW.version
     OR OLD.created_by IS DISTINCT FROM NEW.created_by
     OR OLD.created_at IS DISTINCT FROM NEW.created_at THEN
    RAISE EXCEPTION 'workflow version identity and creation metadata are immutable'
      USING ERRCODE = '23514';
  END IF;

  IF NEW.status = 'published' THEN
    IF OLD.status <> 'draft'
       OR OLD.trigger_config IS DISTINCT FROM NEW.trigger_config
       OR OLD.graph_definition IS DISTINCT FROM NEW.graph_definition
       OR OLD.workspace_id IS DISTINCT FROM NEW.workspace_id
       OR OLD.workflow_id IS DISTINCT FROM NEW.workflow_id
       OR OLD.version IS DISTINCT FROM NEW.version
       OR OLD.created_by IS DISTINCT FROM NEW.created_by
       OR OLD.created_at IS DISTINCT FROM NEW.created_at THEN
      RAISE EXCEPTION 'publishing may only transition unchanged draft content'
        USING ERRCODE = '23514';
    END IF;
  END IF;

  RETURN NEW;
END;
$$;

CREATE TRIGGER weave_team_workflow_versions_publication_guard
BEFORE INSERT OR UPDATE OR DELETE ON weave_team_workflow_versions
FOR EACH ROW
EXECUTE FUNCTION weave_team_workflow_versions_guard_publication();

CREATE TABLE weave_team_workflow_dependencies (
  workspace_id TEXT NOT NULL,
  workflow_id TEXT NOT NULL,
  workflow_version INT NOT NULL,
  owner_type TEXT NOT NULL,
  owner_id TEXT NOT NULL,
  owner_agent_version BIGINT,
  dependency_type TEXT NOT NULL,
  dependency_key TEXT NOT NULL,
  dependency_version BIGINT,
  content_hash TEXT NOT NULL,
  PRIMARY KEY (
    workspace_id,
    workflow_id,
    workflow_version,
    owner_type,
    owner_id,
    dependency_type,
    dependency_key
  ),
  CONSTRAINT weave_team_workflow_dependencies_version_fk
    FOREIGN KEY (workspace_id, workflow_id, workflow_version)
    REFERENCES weave_team_workflow_versions (
      workspace_id,
      workflow_id,
      version
    ) ON DELETE RESTRICT,
  CONSTRAINT weave_team_workflow_dependencies_owner_check CHECK (
    (
      owner_type = 'workflow'
      AND owner_id = workflow_id
      AND owner_agent_version IS NULL
    )
    OR (
      owner_type = 'agent'
      AND owner_agent_version IS NOT NULL
      AND owner_agent_version > 0
    )
  ),
  CONSTRAINT weave_team_workflow_dependencies_type_check CHECK (
    dependency_type IN (
      'agent',
      'skill',
      'mcp_binding',
      'model_binding',
      'credential_reference',
      'delivery_target',
      'runtime_binding',
      'factory'
    )
  ),
  CONSTRAINT weave_team_workflow_dependencies_version_value_check
    CHECK (dependency_version IS NULL OR dependency_version > 0),
  CONSTRAINT weave_team_workflow_dependencies_content_hash_check
    CHECK (content_hash ~ '^[0-9a-f]{64}$')
);

CREATE FUNCTION weave_team_workflow_dependencies_guard_publication()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
  parent_status TEXT;
BEGIN
  IF TG_OP = 'INSERT' THEN
    SELECT status INTO parent_status
    FROM weave_team_workflow_versions
    WHERE workspace_id = NEW.workspace_id
      AND workflow_id = NEW.workflow_id
      AND version = NEW.workflow_version
    FOR UPDATE;

    IF parent_status IS DISTINCT FROM 'draft' THEN
      RAISE EXCEPTION 'dependencies may only be added before publication'
        USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
  END IF;

  IF TG_OP = 'UPDATE'
     AND (
       OLD.workspace_id IS DISTINCT FROM NEW.workspace_id
       OR OLD.workflow_id IS DISTINCT FROM NEW.workflow_id
       OR OLD.workflow_version IS DISTINCT FROM NEW.workflow_version
       OR OLD.owner_type IS DISTINCT FROM NEW.owner_type
       OR OLD.owner_id IS DISTINCT FROM NEW.owner_id
       OR OLD.owner_agent_version IS DISTINCT FROM NEW.owner_agent_version
       OR OLD.dependency_type IS DISTINCT FROM NEW.dependency_type
       OR OLD.dependency_key IS DISTINCT FROM NEW.dependency_key
     ) THEN
    RAISE EXCEPTION 'workflow dependency identity is immutable'
      USING ERRCODE = '23514';
  END IF;

  SELECT status INTO parent_status
  FROM weave_team_workflow_versions
  WHERE workspace_id = OLD.workspace_id
    AND workflow_id = OLD.workflow_id
    AND version = OLD.workflow_version
  FOR UPDATE;

  IF parent_status IS DISTINCT FROM 'draft' THEN
    RAISE EXCEPTION 'published workflow dependencies are immutable'
      USING ERRCODE = '23514';
  END IF;

  IF TG_OP = 'DELETE' THEN
    RETURN OLD;
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER weave_team_workflow_dependencies_publication_guard
BEFORE INSERT OR UPDATE OR DELETE ON weave_team_workflow_dependencies
FOR EACH ROW
EXECUTE FUNCTION weave_team_workflow_dependencies_guard_publication();

CREATE TABLE weave_published_artifact_contents (
  workspace_id TEXT NOT NULL,
  workflow_id TEXT NOT NULL,
  workflow_version INT NOT NULL,
  artifact_schema_version INT NOT NULL,
  canonicalization_algorithm TEXT NOT NULL,
  canonicalization_version INT NOT NULL,
  hash_algorithm TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  payload JSONB NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, workflow_id, workflow_version),
  CONSTRAINT weave_published_artifact_contents_version_fk
    FOREIGN KEY (workspace_id, workflow_id, workflow_version)
    REFERENCES weave_team_workflow_versions (
      workspace_id,
      workflow_id,
      version
    ) ON DELETE RESTRICT,
  CONSTRAINT weave_published_artifact_contents_schema_check
    CHECK (artifact_schema_version = 1),
  CONSTRAINT weave_published_artifact_contents_canonicalization_check CHECK (
    canonicalization_algorithm = 'rfc8785+jcs-preorder'
    AND canonicalization_version = 1
  ),
  CONSTRAINT weave_published_artifact_contents_hash_check CHECK (
    hash_algorithm = 'sha256'
    AND content_hash ~ '^[0-9a-f]{64}$'
  ),
  CONSTRAINT weave_published_artifact_contents_payload_check
    CHECK (jsonb_typeof(payload) IS NOT DISTINCT FROM 'object')
);

CREATE FUNCTION weave_published_artifact_contents_guard()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
  parent_status TEXT;
BEGIN
  IF TG_OP = 'INSERT' THEN
    SELECT status INTO parent_status
    FROM weave_team_workflow_versions
    WHERE workspace_id = NEW.workspace_id
      AND workflow_id = NEW.workflow_id
      AND version = NEW.workflow_version;

    IF parent_status IS DISTINCT FROM 'published' THEN
      RAISE EXCEPTION 'published artifact content requires a published workflow version'
        USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
  END IF;

  RAISE EXCEPTION 'published artifact content is immutable'
    USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER weave_published_artifact_contents_guard
BEFORE INSERT OR UPDATE OR DELETE ON weave_published_artifact_contents
FOR EACH ROW
EXECUTE FUNCTION weave_published_artifact_contents_guard();

CREATE TABLE weave_workflow_version_admission_statuses (
  workspace_id TEXT NOT NULL,
  workflow_id TEXT NOT NULL,
  workflow_version INT NOT NULL,
  blocked BOOLEAN NOT NULL DEFAULT false,
  PRIMARY KEY (workspace_id, workflow_id, workflow_version),
  CONSTRAINT weave_workflow_version_admission_statuses_version_fk
    FOREIGN KEY (workspace_id, workflow_id, workflow_version)
    REFERENCES weave_team_workflow_versions (
      workspace_id,
      workflow_id,
      version
    ) ON DELETE RESTRICT
);

CREATE FUNCTION weave_workflow_version_admission_statuses_guard()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
  parent_status TEXT;
BEGIN
  IF TG_OP = 'INSERT' THEN
    SELECT status INTO parent_status
    FROM weave_team_workflow_versions
    WHERE workspace_id = NEW.workspace_id
      AND workflow_id = NEW.workflow_id
      AND version = NEW.workflow_version;

    IF parent_status IS DISTINCT FROM 'published' THEN
      RAISE EXCEPTION 'admission status requires a published workflow version'
        USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
  END IF;

  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'workflow version admission status is historical'
      USING ERRCODE = '23514';
  END IF;

  IF OLD.workspace_id IS DISTINCT FROM NEW.workspace_id
     OR OLD.workflow_id IS DISTINCT FROM NEW.workflow_id
     OR OLD.workflow_version IS DISTINCT FROM NEW.workflow_version THEN
    RAISE EXCEPTION 'workflow version admission identity is immutable'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER weave_workflow_version_admission_statuses_guard
BEFORE INSERT OR UPDATE OR DELETE
ON weave_workflow_version_admission_statuses
FOR EACH ROW
EXECUTE FUNCTION weave_workflow_version_admission_statuses_guard();

CREATE TABLE weave_workflow_version_admission_receipts (
  workspace_id TEXT NOT NULL,
  workflow_id TEXT NOT NULL,
  workflow_version INT NOT NULL,
  idempotency_key TEXT NOT NULL,
  request_hash TEXT NOT NULL,
  response JSONB NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (
    workspace_id,
    workflow_id,
    workflow_version,
    idempotency_key
  ),
  CONSTRAINT weave_workflow_version_admission_receipts_status_fk
    FOREIGN KEY (workspace_id, workflow_id, workflow_version)
    REFERENCES weave_workflow_version_admission_statuses (
      workspace_id,
      workflow_id,
      workflow_version
    ) ON DELETE RESTRICT,
  CONSTRAINT weave_workflow_version_admission_receipts_request_hash_check
    CHECK (request_hash ~ '^[0-9a-f]{64}$'),
  CONSTRAINT weave_workflow_version_admission_receipts_response_check CHECK (
    jsonb_typeof(response) IS NOT DISTINCT FROM 'object'
    AND response ?& ARRAY[
      'schema_version',
      'old_blocked',
      'new_blocked',
      'changed',
      'audit_id'
    ]
    AND response - ARRAY[
      'schema_version',
      'old_blocked',
      'new_blocked',
      'changed',
      'audit_id'
    ] = '{}'::jsonb
    AND jsonb_typeof(response->'schema_version') IS NOT DISTINCT FROM 'number'
    AND (response->'schema_version')::text = '1'
    AND jsonb_typeof(response->'old_blocked') IS NOT DISTINCT FROM 'boolean'
    AND jsonb_typeof(response->'new_blocked') IS NOT DISTINCT FROM 'boolean'
    AND jsonb_typeof(response->'changed') IS NOT DISTINCT FROM 'boolean'
    AND response->'changed' = to_jsonb(
      response->'old_blocked' IS DISTINCT FROM response->'new_blocked'
    )
    AND (
      (
        response->'changed' = 'true'::jsonb
        AND jsonb_typeof(response->'audit_id') IS NOT DISTINCT FROM 'string'
        AND response->>'audit_id' <> ''
      )
      OR (
        response->'changed' = 'false'::jsonb
        AND response->'audit_id' = 'null'::jsonb
      )
    )
  )
);

CREATE FUNCTION weave_workflow_version_admission_receipts_reject_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'workflow version admission receipt is append-only'
    USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER weave_workflow_version_admission_receipts_append_only
BEFORE UPDATE OR DELETE ON weave_workflow_version_admission_receipts
FOR EACH ROW
EXECUTE FUNCTION weave_workflow_version_admission_receipts_reject_mutation();

CREATE TABLE weave_workflow_version_admission_audits (
  workspace_id TEXT NOT NULL,
  workflow_id TEXT NOT NULL,
  workflow_version INT NOT NULL,
  audit_id TEXT NOT NULL,
  idempotency_key TEXT NOT NULL,
  old_blocked BOOLEAN NOT NULL,
  new_blocked BOOLEAN NOT NULL,
  operator_id TEXT NOT NULL,
  reason TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (
    workspace_id,
    workflow_id,
    workflow_version,
    audit_id
  ),
  CONSTRAINT weave_workflow_version_admission_audits_status_fk
    FOREIGN KEY (workspace_id, workflow_id, workflow_version)
    REFERENCES weave_workflow_version_admission_statuses (
      workspace_id,
      workflow_id,
      workflow_version
    ) ON DELETE RESTRICT,
  CONSTRAINT weave_workflow_version_admission_audits_transition_check
    CHECK (old_blocked <> new_blocked),
  CONSTRAINT weave_workflow_version_admission_audits_receipt_fk
    FOREIGN KEY (
      workspace_id,
      workflow_id,
      workflow_version,
      idempotency_key
    )
    REFERENCES weave_workflow_version_admission_receipts (
      workspace_id,
      workflow_id,
      workflow_version,
      idempotency_key
    ) ON DELETE RESTRICT
);

CREATE FUNCTION weave_workflow_version_admission_audits_validate_receipt()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
  frozen_response JSONB;
BEGIN
  SELECT response INTO frozen_response
  FROM weave_workflow_version_admission_receipts
  WHERE workspace_id = NEW.workspace_id
    AND workflow_id = NEW.workflow_id
    AND workflow_version = NEW.workflow_version
    AND idempotency_key = NEW.idempotency_key;

  IF NOT FOUND THEN
    RETURN NEW;
  END IF;

  IF frozen_response->'changed' IS DISTINCT FROM 'true'::jsonb
     OR frozen_response->'audit_id' IS DISTINCT FROM to_jsonb(NEW.audit_id)
     OR frozen_response->'old_blocked' IS DISTINCT FROM to_jsonb(NEW.old_blocked)
     OR frozen_response->'new_blocked' IS DISTINCT FROM to_jsonb(NEW.new_blocked) THEN
    RAISE EXCEPTION 'admission audit must match frozen receipt'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER weave_workflow_version_admission_audits_validate_receipt
BEFORE INSERT ON weave_workflow_version_admission_audits
FOR EACH ROW
EXECUTE FUNCTION weave_workflow_version_admission_audits_validate_receipt();

CREATE FUNCTION weave_workflow_version_admission_audits_reject_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'workflow version admission audit is append-only'
    USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER weave_workflow_version_admission_audits_append_only
BEFORE UPDATE OR DELETE ON weave_workflow_version_admission_audits
FOR EACH ROW
EXECUTE FUNCTION weave_workflow_version_admission_audits_reject_mutation();
