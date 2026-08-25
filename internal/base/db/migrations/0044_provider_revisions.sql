LOCK TABLE weave_provider_credentials IN ACCESS EXCLUSIVE MODE;

ALTER TABLE weave_provider_credentials
  ADD COLUMN latest_revision BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN source_kind TEXT NOT NULL DEFAULT 'workspace',
  ADD COLUMN source_provider_id TEXT,
  ADD COLUMN enabled BOOLEAN NOT NULL DEFAULT true,
  ADD COLUMN revoked_at TIMESTAMPTZ,
  ADD COLUMN deleted_at TIMESTAMPTZ,
  ADD CONSTRAINT weave_provider_credentials_latest_revision_check
    CHECK (latest_revision BETWEEN 0 AND 9007199254740991),
  ADD CONSTRAINT weave_provider_credentials_source_check CHECK (
    (source_kind = 'workspace' AND source_provider_id IS NULL)
    OR
    (source_kind = 'system_mirror'
      AND source_provider_id IS NOT NULL
      AND source_provider_id <> '')
  ),
  ADD CONSTRAINT weave_provider_credentials_status_check CHECK (
    revoked_at IS NULL OR deleted_at IS NULL OR revoked_at <= deleted_at
  );

-- Legacy credential rows become live workspace heads at revision zero. The
-- migration deliberately does not invent canonical functional history.
CREATE TABLE weave_provider_revisions (
  workspace_id TEXT NOT NULL,
  provider_id TEXT NOT NULL,
  revision BIGINT NOT NULL,
  name TEXT NOT NULL,
  base_url TEXT NOT NULL,
  models JSONB NOT NULL,
  json_object_mode BOOLEAN NOT NULL,
  content_hash TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, provider_id, revision),
  CONSTRAINT weave_provider_revisions_provider_fk
    FOREIGN KEY (workspace_id, provider_id)
    REFERENCES weave_provider_credentials (workspace_id, id) ON DELETE RESTRICT,
  CONSTRAINT weave_provider_revisions_version_check
    CHECK (revision BETWEEN 1 AND 9007199254740991),
  CONSTRAINT weave_provider_revisions_models_check
    CHECK (jsonb_typeof(models) = 'array'),
  CONSTRAINT weave_provider_revisions_content_hash_check
    CHECK (content_hash ~ '^[0-9a-f]{64}$')
);

CREATE FUNCTION weave_provider_revisions_reject_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'provider revision records are immutable'
    USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER weave_provider_revisions_immutable
BEFORE UPDATE OR DELETE ON weave_provider_revisions
FOR EACH ROW
EXECUTE FUNCTION weave_provider_revisions_reject_mutation();

CREATE TABLE weave_system_provider_mirror_audits (
  workspace_id TEXT NOT NULL,
  audit_id TEXT NOT NULL,
  provider_id TEXT NOT NULL,
  system_provider_id TEXT NOT NULL,
  operator_id TEXT NOT NULL,
  outcome TEXT NOT NULL,
  before_revision BIGINT NOT NULL,
  after_revision BIGINT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, audit_id),
  CONSTRAINT weave_system_provider_mirror_audits_provider_fk
    FOREIGN KEY (workspace_id, provider_id)
    REFERENCES weave_provider_credentials (workspace_id, id) ON DELETE RESTRICT,
  CONSTRAINT weave_system_provider_mirror_audits_identity_check CHECK (
    audit_id <> ''
    AND system_provider_id <> ''
    AND operator_id <> ''
    AND outcome <> ''
  ),
  CONSTRAINT weave_system_provider_mirror_audits_before_revision_check
    CHECK (before_revision BETWEEN 0 AND 9007199254740991),
  CONSTRAINT weave_system_provider_mirror_audits_after_revision_check
    CHECK (after_revision BETWEEN 0 AND 9007199254740991)
);

CREATE FUNCTION weave_system_provider_mirror_audits_reject_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'system provider mirror audit is append-only'
    USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER weave_system_provider_mirror_audits_append_only
BEFORE UPDATE OR DELETE ON weave_system_provider_mirror_audits
FOR EACH ROW
EXECUTE FUNCTION weave_system_provider_mirror_audits_reject_mutation();
