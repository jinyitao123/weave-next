CREATE TABLE weave_team_template_requests (
  workspace_id TEXT NOT NULL,
  idempotency_key UUID NOT NULL,
  request_fingerprint TEXT NOT NULL,
  build_run_id TEXT NOT NULL,
  created_by TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL,

  CONSTRAINT weave_team_template_requests_pkey PRIMARY KEY (
    workspace_id, idempotency_key
  ),
  CONSTRAINT weave_team_template_requests_identity_check CHECK (
    workspace_id <> '' AND build_run_id <> '' AND created_by <> ''
  ),
  CONSTRAINT weave_team_template_requests_fingerprint_check CHECK (
    request_fingerprint ~ '^[0-9a-f]{64}$'
  ),
  CONSTRAINT weave_team_template_requests_build_run_unique UNIQUE (
    workspace_id, build_run_id
  )
);
