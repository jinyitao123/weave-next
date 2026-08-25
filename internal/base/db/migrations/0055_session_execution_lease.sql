CREATE TABLE weave_session_execution_leases (
  workspace_id TEXT NOT NULL,
  user_id TEXT NOT NULL,
  lead_avatar_id TEXT NOT NULL,
  session_id TEXT NOT NULL,
  lease_epoch BIGINT NOT NULL,
  state TEXT NOT NULL,
  active_run_id TEXT NOT NULL,
  run_snapshot_id TEXT NOT NULL,
  controller_kind TEXT NOT NULL,
  controller_id TEXT NOT NULL,
  yield_kind TEXT NOT NULL,
  resume_token_hash BYTEA,
  yield_generation BIGINT,
  input_schema JSONB,
  acquire_event_id TEXT NOT NULL,
  expires_at TIMESTAMPTZ NOT NULL,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  closed_at TIMESTAMPTZ,
  close_reason TEXT,

  CONSTRAINT weave_session_execution_leases_pkey PRIMARY KEY (
    workspace_id, user_id, lead_avatar_id, session_id
  ),
  CONSTRAINT weave_session_execution_leases_acquire_event_key UNIQUE (
    workspace_id, user_id, lead_avatar_id, session_id, acquire_event_id
  ),
  CONSTRAINT weave_session_execution_leases_identity_nonempty_check CHECK (
    workspace_id <> '' AND user_id <> '' AND lead_avatar_id <> '' AND session_id <> ''
  ),
  CONSTRAINT weave_session_execution_leases_epoch_check CHECK (lease_epoch > 0),
  CONSTRAINT weave_session_execution_leases_state_check CHECK (
    state IN ('active', 'parked', 'closed')
  ),
  CONSTRAINT weave_session_execution_leases_run_nonempty_check CHECK (
    active_run_id <> '' AND run_snapshot_id <> ''
  ),
  CONSTRAINT weave_session_execution_leases_controller_kind_check CHECK (
    controller_kind IN ('lead_avatar', 'worker')
  ),
  CONSTRAINT weave_session_execution_leases_controller_nonempty_check CHECK (
    controller_id <> ''
  ),
  CONSTRAINT weave_session_execution_leases_yield_kind_check CHECK (
    yield_kind IN ('none', 'await_input')
  ),
  CONSTRAINT weave_session_execution_leases_acquire_event_nonempty_check CHECK (
    acquire_event_id <> ''
  ),
  CONSTRAINT weave_session_execution_leases_parked_shape_check CHECK (
    (state = 'parked') = (
      yield_kind <> 'none'
      AND resume_token_hash IS NOT NULL
      AND yield_generation IS NOT NULL
      AND input_schema IS NOT NULL
    )
  ),
  CONSTRAINT weave_session_execution_leases_closed_shape_check CHECK (
    (state = 'closed') = (closed_at IS NOT NULL AND close_reason IS NOT NULL)
  ),
  CONSTRAINT weave_session_execution_leases_close_reason_check CHECK (
    close_reason IS NULL OR close_reason IN ('final_committed', 'expired')
  )
);

CREATE INDEX weave_session_execution_leases_expired_idx
  ON weave_session_execution_leases (expires_at)
  WHERE state IN ('active', 'parked');

CREATE TABLE weave_session_outbox (
  workspace_id TEXT NOT NULL,
  user_id TEXT NOT NULL,
  lead_avatar_id TEXT NOT NULL,
  session_id TEXT NOT NULL,
  event_id TEXT NOT NULL,
  lease_epoch BIGINT NOT NULL,
  active_run_id TEXT NOT NULL,
  run_snapshot_id TEXT NOT NULL,
  role TEXT NOT NULL,
  content TEXT NOT NULL,
  metadata JSONB NOT NULL,
  delivery_state TEXT NOT NULL,
  delivery_attempts INTEGER NOT NULL DEFAULT 0,
  claim_owner TEXT,
  claim_expires_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL,
  delivered_at TIMESTAMPTZ,
  last_error TEXT,

  CONSTRAINT weave_session_outbox_pkey PRIMARY KEY (
    workspace_id, user_id, lead_avatar_id, session_id, event_id
  ),
  CONSTRAINT weave_session_outbox_single_final_key UNIQUE (
    workspace_id, user_id, lead_avatar_id, session_id, lease_epoch, active_run_id
  ),
  CONSTRAINT weave_session_outbox_lease_fkey FOREIGN KEY (
    workspace_id, user_id, lead_avatar_id, session_id
  ) REFERENCES weave_session_execution_leases (
    workspace_id, user_id, lead_avatar_id, session_id
  ),
  CONSTRAINT weave_session_outbox_epoch_check CHECK (lease_epoch > 0),
  CONSTRAINT weave_session_outbox_role_check CHECK (role IN ('assistant', 'event')),
  CONSTRAINT weave_session_outbox_delivery_state_check CHECK (
    delivery_state IN ('pending', 'delivering', 'delivered')
  ),
  CONSTRAINT weave_session_outbox_delivery_attempts_check CHECK (delivery_attempts >= 0)
);

CREATE TABLE weave_session_execution_audit (
  id TEXT NOT NULL,
  workspace_id TEXT NOT NULL,
  user_id TEXT NOT NULL,
  lead_avatar_id TEXT NOT NULL,
  session_id TEXT NOT NULL,
  observed_epoch BIGINT NOT NULL,
  current_epoch BIGINT NOT NULL,
  active_run_id TEXT NOT NULL,
  event_kind TEXT NOT NULL,
  isolation_reason TEXT,
  payload_digest BYTEA,
  detail JSONB NOT NULL,
  created_at TIMESTAMPTZ NOT NULL,

  CONSTRAINT weave_session_execution_audit_pkey PRIMARY KEY (
    workspace_id, user_id, lead_avatar_id, session_id, id
  )
);
