-- A task-scoped Forge credential is bound to the exact Workbench input. The
-- credential body is encrypted and never enters the task payload or model.
CREATE TABLE weave_task_business_delegations (
  workspace_id TEXT NOT NULL,
  user_id TEXT NOT NULL,
  input_revision_id TEXT NOT NULL,
  delegation_id UUID NOT NULL,
  credential_ref TEXT NOT NULL,
  issuer TEXT NOT NULL,
  external_subject TEXT NOT NULL,
  external_organization TEXT NOT NULL,
  credential_ciphertext TEXT NOT NULL,
  credential_sha256 TEXT NOT NULL CHECK (credential_sha256 ~ '^[0-9a-f]{64}$'),
  allowed_actions JSONB NOT NULL CHECK (jsonb_typeof(allowed_actions) = 'array'),
  resources JSONB NOT NULL CHECK (jsonb_typeof(resources) = 'array'),
  workflow_id TEXT NOT NULL,
  workflow_version INTEGER NOT NULL CHECK (workflow_version > 0),
  issued_at TIMESTAMPTZ NOT NULL,
  expires_at TIMESTAMPTZ NOT NULL,
  revoked_at TIMESTAMPTZ,
  refresh_generation BIGINT NOT NULL DEFAULT 1 CHECK (refresh_generation > 0),
  PRIMARY KEY (workspace_id, input_revision_id),
  UNIQUE (workspace_id, delegation_id),
  UNIQUE (workspace_id, credential_ref),
  FOREIGN KEY (workspace_id, user_id, input_revision_id)
    REFERENCES weave_dispatch_input_revisions(workspace_id, user_id, input_revision_id),
  CHECK (workspace_id <> '' AND user_id <> '' AND credential_ref <> '' AND issuer <> ''
    AND external_subject <> '' AND external_organization <> '' AND workflow_id <> ''
    AND expires_at > issued_at)
);

CREATE INDEX weave_task_business_delegations_expiry_idx
  ON weave_task_business_delegations(workspace_id, expires_at)
  WHERE revoked_at IS NULL;

CREATE FUNCTION weave_task_business_delegation_guard()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'task business delegations are retained for audit' USING ERRCODE = '55000';
  END IF;
  IF ROW(OLD.workspace_id, OLD.user_id, OLD.input_revision_id, OLD.delegation_id,
      OLD.credential_ref, OLD.issuer, OLD.external_subject, OLD.external_organization,
      OLD.allowed_actions, OLD.resources, OLD.workflow_id, OLD.workflow_version, OLD.issued_at)
    IS DISTINCT FROM
    ROW(NEW.workspace_id, NEW.user_id, NEW.input_revision_id, NEW.delegation_id,
      NEW.credential_ref, NEW.issuer, NEW.external_subject, NEW.external_organization,
      NEW.allowed_actions, NEW.resources, NEW.workflow_id, NEW.workflow_version, NEW.issued_at)
    OR NEW.refresh_generation < OLD.refresh_generation
    OR (OLD.revoked_at IS NOT NULL AND NEW.revoked_at IS DISTINCT FROM OLD.revoked_at) THEN
    RAISE EXCEPTION 'task business delegation facts are immutable' USING ERRCODE = '55000';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER weave_task_business_delegation_guard
  BEFORE UPDATE OR DELETE ON weave_task_business_delegations
  FOR EACH ROW EXECUTE FUNCTION weave_task_business_delegation_guard();
