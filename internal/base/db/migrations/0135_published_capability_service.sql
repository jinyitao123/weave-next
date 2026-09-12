ALTER TABLE weave_users
  ADD CONSTRAINT weave_users_tenant_id_id_key UNIQUE (tenant_id, id);

CREATE TABLE weave_service_apps (
  workspace_id              TEXT        NOT NULL,
  id                        TEXT        NOT NULL,
  name                      TEXT        NOT NULL CHECK (name <> ''),
  description               TEXT        NOT NULL DEFAULT '',
  enabled                   BOOLEAN     NOT NULL DEFAULT true,
  max_concurrent_invocations INTEGER    NOT NULL CHECK (max_concurrent_invocations BETWEEN 1 AND 1024),
  created_by                TEXT        NOT NULL,
  created_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, id),
  UNIQUE (workspace_id, name),
  FOREIGN KEY (workspace_id) REFERENCES weave_workspaces(id) ON DELETE RESTRICT,
  FOREIGN KEY (workspace_id, created_by)
    REFERENCES weave_users(tenant_id, id) ON DELETE RESTRICT
);

CREATE TABLE weave_service_app_credentials (
  workspace_id TEXT        NOT NULL,
  id           TEXT        NOT NULL,
  app_id       TEXT        NOT NULL,
  name         TEXT        NOT NULL CHECK (name <> ''),
  key_hash     TEXT        NOT NULL UNIQUE CHECK (key_hash ~ '^[0-9a-f]{64}$'),
  scopes       TEXT[]      NOT NULL CHECK (
    cardinality(scopes) BETWEEN 1 AND 3
    AND scopes <@ ARRAY['invoke','read','cancel']::TEXT[]
  ),
  expires_at   TIMESTAMPTZ,
  revoked_at   TIMESTAMPTZ,
  last_used_at TIMESTAMPTZ,
  created_by   TEXT        NOT NULL,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, id),
  UNIQUE (workspace_id, id, app_id),
  UNIQUE (workspace_id, app_id, name),
  FOREIGN KEY (workspace_id, app_id)
    REFERENCES weave_service_apps(workspace_id, id) ON DELETE RESTRICT,
  FOREIGN KEY (workspace_id, created_by)
    REFERENCES weave_users(tenant_id, id) ON DELETE RESTRICT
);

CREATE TABLE weave_capabilities (
  workspace_id TEXT        NOT NULL,
  id           TEXT        NOT NULL,
  key          TEXT        NOT NULL CHECK (key ~ '^[a-z][a-z0-9_]{0,62}$'),
  name         TEXT        NOT NULL CHECK (name <> ''),
  description  TEXT        NOT NULL DEFAULT '',
  enabled      BOOLEAN     NOT NULL DEFAULT true,
  created_by   TEXT        NOT NULL,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, id),
  UNIQUE (workspace_id, key),
  FOREIGN KEY (workspace_id) REFERENCES weave_workspaces(id) ON DELETE RESTRICT,
  FOREIGN KEY (workspace_id, created_by)
    REFERENCES weave_users(tenant_id, id) ON DELETE RESTRICT
);

ALTER TABLE weave_published_artifact_contents
  ADD CONSTRAINT weave_published_artifact_contents_hash_identity_key
  UNIQUE (workspace_id, workflow_id, workflow_version, content_hash);

CREATE TABLE weave_capability_releases (
  workspace_id          TEXT        NOT NULL,
  capability_id         TEXT        NOT NULL,
  version               INTEGER     NOT NULL CHECK (version > 0),
  id                    TEXT        NOT NULL,
  workflow_id           TEXT        NOT NULL,
  workflow_version      INTEGER     NOT NULL CHECK (workflow_version > 0),
  artifact_content_hash TEXT        NOT NULL CHECK (artifact_content_hash ~ '^[0-9a-f]{64}$'),
  contract_dialect      TEXT        NOT NULL CHECK (contract_dialect = 'weave.fixed-json-schema-v1'),
  normalization         TEXT        NOT NULL CHECK (normalization = 'rfc8785-jcs-v1'),
  input_contract        JSONB       NOT NULL,
  output_contract       JSONB       NOT NULL,
  input_mapping         JSONB       NOT NULL CHECK (
    input_mapping = '{"schema_version":1,"mode":"direct_run_input"}'::JSONB
  ),
  execution_limits      JSONB       NOT NULL,
  result_policy         JSONB       NOT NULL,
  created_by            TEXT        NOT NULL,
  created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, capability_id, version),
  UNIQUE (workspace_id, id),
  UNIQUE (workspace_id, id, capability_id, version, artifact_content_hash),
  FOREIGN KEY (workspace_id, capability_id)
    REFERENCES weave_capabilities(workspace_id, id) ON DELETE RESTRICT,
  FOREIGN KEY (workspace_id, workflow_id, workflow_version, artifact_content_hash)
    REFERENCES weave_published_artifact_contents(
      workspace_id, workflow_id, workflow_version, content_hash
    ) ON DELETE RESTRICT,
  FOREIGN KEY (workspace_id, created_by)
    REFERENCES weave_users(tenant_id, id) ON DELETE RESTRICT
);

CREATE TABLE weave_capability_release_controls (
  workspace_id  TEXT        NOT NULL,
  capability_id TEXT        NOT NULL,
  version       INTEGER     NOT NULL,
  enabled       BOOLEAN     NOT NULL DEFAULT true,
  updated_by    TEXT        NOT NULL,
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, capability_id, version),
  FOREIGN KEY (workspace_id, capability_id, version)
    REFERENCES weave_capability_releases(workspace_id, capability_id, version) ON DELETE RESTRICT,
  FOREIGN KEY (workspace_id, updated_by)
    REFERENCES weave_users(tenant_id, id) ON DELETE RESTRICT
);

CREATE TABLE weave_service_app_release_grants (
  workspace_id       TEXT        NOT NULL,
  id                 TEXT        NOT NULL,
  app_id             TEXT        NOT NULL,
  capability_id      TEXT        NOT NULL,
  release_version    INTEGER     NOT NULL,
  max_concurrent     INTEGER     NOT NULL CHECK (max_concurrent BETWEEN 1 AND 1024),
  granted_by         TEXT        NOT NULL,
  granted_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
  revoked_by         TEXT,
  revoked_at         TIMESTAMPTZ,
  PRIMARY KEY (workspace_id, id),
  UNIQUE (workspace_id, id, app_id, capability_id, release_version),
  FOREIGN KEY (workspace_id, app_id)
    REFERENCES weave_service_apps(workspace_id, id) ON DELETE RESTRICT,
  FOREIGN KEY (workspace_id, capability_id, release_version)
    REFERENCES weave_capability_releases(workspace_id, capability_id, version) ON DELETE RESTRICT,
  FOREIGN KEY (workspace_id, granted_by)
    REFERENCES weave_users(tenant_id, id) ON DELETE RESTRICT,
  FOREIGN KEY (workspace_id, revoked_by)
    REFERENCES weave_users(tenant_id, id) ON DELETE RESTRICT,
  CHECK ((revoked_at IS NULL) = (revoked_by IS NULL))
);

CREATE UNIQUE INDEX weave_service_app_release_grants_active_key
  ON weave_service_app_release_grants(workspace_id, app_id, capability_id, release_version)
  WHERE revoked_at IS NULL;

CREATE TABLE weave_capability_invocations (
  workspace_id          TEXT        NOT NULL,
  app_id                TEXT        NOT NULL,
  invocation_id         TEXT        NOT NULL,
  request_id            TEXT        NOT NULL CHECK (
    request_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$'
  ),
  capability_id         TEXT        NOT NULL,
  release_version       INTEGER     NOT NULL CHECK (release_version > 0),
  release_id            TEXT        NOT NULL,
  grant_id              TEXT        NOT NULL,
  credential_id         TEXT        NOT NULL,
  artifact_content_hash TEXT        NOT NULL CHECK (artifact_content_hash ~ '^[0-9a-f]{64}$'),
  normalization         TEXT        NOT NULL CHECK (normalization = 'rfc8785-jcs-v1'),
  input_json            JSONB       NOT NULL,
  input_canonical       TEXT        NOT NULL CHECK (input_json = input_canonical::JSONB),
  input_hash            TEXT        NOT NULL CHECK (input_hash ~ '^[0-9a-f]{64}$'),
  request_fingerprint   TEXT        NOT NULL CHECK (request_fingerprint ~ '^[0-9a-f]{64}$'),
  concurrency_key       TEXT CHECK (
    concurrency_key IS NULL OR concurrency_key ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$'
  ),
  run_id                TEXT        NOT NULL,
  task_id               TEXT        NOT NULL,
  accepted_at           TIMESTAMPTZ NOT NULL,
  deadline_at           TIMESTAMPTZ NOT NULL CHECK (deadline_at > accepted_at),
  PRIMARY KEY (workspace_id, invocation_id),
  UNIQUE (workspace_id, app_id, invocation_id),
  UNIQUE (workspace_id, app_id, request_id),
  UNIQUE (workspace_id, run_id),
  UNIQUE (workspace_id, task_id),
  FOREIGN KEY (workspace_id, app_id)
    REFERENCES weave_service_apps(workspace_id, id) ON DELETE RESTRICT,
  FOREIGN KEY (
    workspace_id, release_id, capability_id, release_version, artifact_content_hash
  ) REFERENCES weave_capability_releases(
    workspace_id, id, capability_id, version, artifact_content_hash
  ) ON DELETE RESTRICT,
  FOREIGN KEY (workspace_id, grant_id, app_id, capability_id, release_version)
    REFERENCES weave_service_app_release_grants(
      workspace_id, id, app_id, capability_id, release_version
    ) ON DELETE RESTRICT,
  FOREIGN KEY (workspace_id, credential_id, app_id)
    REFERENCES weave_service_app_credentials(workspace_id, id, app_id) ON DELETE RESTRICT,
  FOREIGN KEY (workspace_id, run_id)
    REFERENCES weave_team_run_snapshots(workspace_id, run_id) ON DELETE RESTRICT,
  FOREIGN KEY (workspace_id, task_id)
    REFERENCES weave_task_queue(workspace_id, id) ON DELETE RESTRICT
);

CREATE INDEX weave_capability_invocations_app_accepted
  ON weave_capability_invocations(workspace_id, app_id, accepted_at DESC);

CREATE INDEX weave_capability_invocations_release
  ON weave_capability_invocations(workspace_id, capability_id, release_version, accepted_at DESC);

CREATE TABLE weave_capability_invocation_cancellations (
  workspace_id           TEXT        NOT NULL,
  app_id                 TEXT        NOT NULL,
  request_id             TEXT        NOT NULL CHECK (
    request_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$'
  ),
  invocation_id          TEXT,
  requested_credential_id TEXT       NOT NULL,
  reason                 TEXT        NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 256),
  requested_at           TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (workspace_id, app_id, request_id),
  FOREIGN KEY (workspace_id, app_id)
    REFERENCES weave_service_apps(workspace_id, id) ON DELETE RESTRICT,
  FOREIGN KEY (workspace_id, app_id, invocation_id)
    REFERENCES weave_capability_invocations(workspace_id, app_id, invocation_id) ON DELETE RESTRICT,
  FOREIGN KEY (workspace_id, requested_credential_id, app_id)
    REFERENCES weave_service_app_credentials(workspace_id, id, app_id) ON DELETE RESTRICT
);

CREATE FUNCTION weave_capability_release_reject_rewrite()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'capability release is immutable';
END;
$$;

CREATE TRIGGER weave_capability_release_immutable
BEFORE UPDATE OR DELETE ON weave_capability_releases
FOR EACH ROW EXECUTE FUNCTION weave_capability_release_reject_rewrite();

CREATE FUNCTION weave_capability_invocation_reject_rewrite()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'capability invocation is immutable';
END;
$$;

CREATE TRIGGER weave_capability_invocation_immutable
BEFORE UPDATE OR DELETE ON weave_capability_invocations
FOR EACH ROW EXECUTE FUNCTION weave_capability_invocation_reject_rewrite();

CREATE FUNCTION weave_capability_cancellation_reject_rewrite()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'capability cancellation is immutable';
END;
$$;

CREATE TRIGGER weave_capability_cancellation_immutable
BEFORE UPDATE OR DELETE ON weave_capability_invocation_cancellations
FOR EACH ROW EXECUTE FUNCTION weave_capability_cancellation_reject_rewrite();
