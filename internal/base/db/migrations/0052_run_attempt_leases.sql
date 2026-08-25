CREATE TABLE weave_run_attempt_leases (
  workspace_id TEXT NOT NULL,
  run_id TEXT NOT NULL,
  attempt_generation BIGINT NOT NULL,
  attempt_id UUID NOT NULL,
  graph_name TEXT NOT NULL,
  run_started_at TEXT NOT NULL,
  attempt_started_at TIMESTAMPTZ NOT NULL,
  state TEXT NOT NULL,
  heartbeat_at TIMESTAMPTZ NOT NULL,
  lease_expires_at TIMESTAMPTZ NOT NULL,
  claim_id UUID,
  claim_expires_at TIMESTAMPTZ,
  last_error_code TEXT,
  retry_count BIGINT NOT NULL,

  CONSTRAINT weave_run_attempt_leases_pkey PRIMARY KEY (workspace_id, run_id),
  CONSTRAINT weave_run_attempt_leases_identity_nonempty_check CHECK (
    workspace_id <> '' AND run_id <> '' AND graph_name <> '' AND run_started_at <> ''
  ),
  CONSTRAINT weave_run_attempt_leases_generation_check CHECK (attempt_generation >= 1),
  CONSTRAINT weave_run_attempt_leases_started_at_shape_check CHECK (
    run_started_at ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?(Z|[+-][0-9]{2}:[0-9]{2})$'
  ),
  CONSTRAINT weave_run_attempt_leases_state_check CHECK (
    state IN ('active', 'yielded', 'closed', 'reconciling', 'reconciled')
  ),
  CONSTRAINT weave_run_attempt_leases_time_order_check CHECK (
    attempt_started_at <= heartbeat_at AND heartbeat_at <= lease_expires_at
  ),
  CONSTRAINT weave_run_attempt_leases_claim_shape_check CHECK (
    (state = 'reconciling' AND claim_id IS NOT NULL AND claim_expires_at IS NOT NULL)
    OR (state <> 'reconciling' AND claim_id IS NULL AND claim_expires_at IS NULL)
  ),
  CONSTRAINT weave_run_attempt_leases_last_error_check CHECK (last_error_code IS NULL OR last_error_code <> ''),
  CONSTRAINT weave_run_attempt_leases_retry_count_check CHECK (retry_count >= 0)
);

CREATE INDEX idx_weave_run_attempt_leases_claim
  ON weave_run_attempt_leases (lease_expires_at, workspace_id, run_id)
  WHERE state = 'active';

CREATE INDEX idx_weave_run_attempt_leases_reclaim
  ON weave_run_attempt_leases (claim_expires_at, workspace_id, run_id)
  WHERE state = 'reconciling';
