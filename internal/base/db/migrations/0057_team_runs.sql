CREATE TABLE weave_team_runs (
  workspace_id TEXT NOT NULL,
  run_id TEXT NOT NULL,
  status TEXT NOT NULL,
  team_run_generation BIGINT NOT NULL DEFAULT 0,
  execution_lease_epoch BIGINT NOT NULL DEFAULT 0,
  resume_generation BIGINT NOT NULL DEFAULT 0,

  team_id TEXT NOT NULL,
  workflow_id TEXT NOT NULL,
  workflow_version INT NOT NULL,
  run_snapshot_id TEXT NOT NULL,
  source_kind TEXT NOT NULL,
  source_task_id TEXT NOT NULL,
  establish_idempotency_key TEXT NOT NULL,

  current_executor_id TEXT,
  wait_kind TEXT,
  wait_detail JSONB,
  resume_token_hash BYTEA,
  checkpoint_ref TEXT,

  cancel_actor TEXT,
  cancel_reason TEXT,
  cancel_idempotency_key TEXT,
  cancel_requested_at TIMESTAMPTZ,
  cancel_grace_deadline_at TIMESTAMPTZ,

  error_code TEXT,
  cause_summary TEXT,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  terminal_at TIMESTAMPTZ,

  CONSTRAINT weave_team_runs_pkey PRIMARY KEY (workspace_id, run_id),
  CONSTRAINT weave_team_runs_source_task_key UNIQUE (
    workspace_id, source_kind, source_task_id
  ),
  CONSTRAINT weave_team_runs_identity_nonempty_check CHECK (
    workspace_id <> ''
    AND run_id <> ''
    AND team_id <> ''
    AND workflow_id <> ''
    AND run_snapshot_id <> ''
    AND source_task_id <> ''
    AND establish_idempotency_key <> ''
  ),
  CONSTRAINT weave_team_runs_status_check CHECK (
    status IN (
      'queued', 'running', 'parked', 'cancel_requested',
      'succeeded', 'failed', 'cancelled', 'abandoned'
    )
  ),
  CONSTRAINT weave_team_runs_generation_check CHECK (
    team_run_generation >= 0
    AND execution_lease_epoch >= 0
    AND resume_generation >= 0
  ),
  CONSTRAINT weave_team_runs_workflow_version_check
    CHECK (workflow_version > 0),
  CONSTRAINT weave_team_runs_source_kind_check CHECK (
    source_kind IN ('session', 'schedule', 'api', 'event')
  ),
  CONSTRAINT weave_team_runs_optional_text_check CHECK (
    (current_executor_id IS NULL OR current_executor_id <> '')
    AND (wait_kind IS NULL OR wait_kind <> '')
    AND (checkpoint_ref IS NULL OR checkpoint_ref <> '')
    AND (cancel_actor IS NULL OR cancel_actor <> '')
    AND (cancel_reason IS NULL OR cancel_reason <> '')
    AND (cancel_idempotency_key IS NULL OR cancel_idempotency_key <> '')
    AND (error_code IS NULL OR error_code <> '')
  ),
  CONSTRAINT weave_team_runs_wait_detail_check CHECK (
    wait_detail IS NULL
    OR jsonb_typeof(wait_detail) <> 'null'
  ),
  CONSTRAINT weave_team_runs_parked_shape_check CHECK (
    (status = 'parked') = (
      wait_kind IS NOT NULL
      AND wait_detail IS NOT NULL
      AND resume_token_hash IS NOT NULL
      AND octet_length(resume_token_hash) > 0
      AND checkpoint_ref IS NOT NULL
    )
  ),
  CONSTRAINT weave_team_runs_cancel_shape_check CHECK (
    (status = 'cancel_requested') = (
      cancel_actor IS NOT NULL
      AND cancel_reason IS NOT NULL
      AND cancel_idempotency_key IS NOT NULL
      AND cancel_requested_at IS NOT NULL
      AND cancel_grace_deadline_at IS NOT NULL
    )
  ),
  CONSTRAINT weave_team_runs_cancel_deadline_check CHECK (
    cancel_requested_at IS NULL
    OR cancel_grace_deadline_at >= cancel_requested_at
  ),
  CONSTRAINT weave_team_runs_running_shape_check CHECK (
    (status = 'running') = (current_executor_id IS NOT NULL)
  ),
  CONSTRAINT weave_team_runs_error_shape_check CHECK (
    (
      status IN ('failed', 'abandoned')
      AND error_code IS NOT NULL
    )
    OR (
      status = 'cancelled'
      AND error_code = 'team_run_cancelled'
      AND cause_summary IS NULL
    )
    OR (
      status NOT IN ('failed', 'abandoned', 'cancelled')
      AND error_code IS NULL
      AND cause_summary IS NULL
    )
  ),
  CONSTRAINT weave_team_runs_cause_summary_check CHECK (
    cause_summary IS NULL
    OR (
      error_code IS NOT NULL
      AND cause_summary <> ''
      AND octet_length(cause_summary) <= 1024
    )
  ),
  CONSTRAINT weave_team_runs_time_order_check CHECK (
    created_at <= updated_at
    AND (
      (
        status IN ('succeeded', 'failed', 'cancelled', 'abandoned')
        AND terminal_at IS NOT NULL
        AND updated_at <= terminal_at
      )
      OR (
        status NOT IN ('succeeded', 'failed', 'cancelled', 'abandoned')
        AND terminal_at IS NULL
      )
    )
  ),
  CONSTRAINT weave_team_runs_snapshot_fkey FOREIGN KEY (
    workspace_id, run_snapshot_id
  ) REFERENCES weave_team_run_snapshots (
    workspace_id, run_id
  ) ON DELETE RESTRICT
);

CREATE INDEX weave_team_runs_status_updated_idx
  ON weave_team_runs (status, updated_at, workspace_id, run_id);

CREATE INDEX weave_team_runs_cancel_deadline_idx
  ON weave_team_runs (
    cancel_grace_deadline_at, workspace_id, run_id
  )
  WHERE status = 'cancel_requested';

CREATE TABLE weave_team_run_transitions (
  workspace_id TEXT NOT NULL,
  run_id TEXT NOT NULL,
  seq BIGINT NOT NULL,
  from_status TEXT,
  to_status TEXT NOT NULL,
  team_run_generation BIGINT NOT NULL,
  execution_lease_epoch BIGINT NOT NULL,
  resume_generation BIGINT NOT NULL,
  actor TEXT NOT NULL,
  source TEXT NOT NULL,
  idempotency_key TEXT NOT NULL,
  error_code TEXT,
  cause_summary TEXT,
  occurred_at TIMESTAMPTZ NOT NULL,
  orphaned BOOLEAN NOT NULL DEFAULT FALSE,

  CONSTRAINT weave_team_run_transitions_pkey PRIMARY KEY (
    workspace_id, run_id, seq
  ),
  CONSTRAINT weave_team_run_transitions_idempotency_key UNIQUE (
    workspace_id, run_id, idempotency_key
  ),
  CONSTRAINT weave_team_run_transitions_run_fkey FOREIGN KEY (
    workspace_id, run_id
  ) REFERENCES weave_team_runs (
    workspace_id, run_id
  ) ON DELETE RESTRICT,
  CONSTRAINT weave_team_run_transitions_seq_check CHECK (seq > 0),
  CONSTRAINT weave_team_run_transitions_status_check CHECK (
    (
      from_status IS NULL
      OR from_status IN (
        'queued', 'running', 'parked', 'cancel_requested',
        'succeeded', 'failed', 'cancelled', 'abandoned'
      )
    )
    AND to_status IN (
      'queued', 'running', 'parked', 'cancel_requested',
      'succeeded', 'failed', 'cancelled', 'abandoned'
    )
  ),
  CONSTRAINT weave_team_run_transitions_generation_check CHECK (
    team_run_generation >= 0
    AND execution_lease_epoch >= 0
    AND resume_generation >= 0
  ),
  CONSTRAINT weave_team_run_transitions_identity_check CHECK (
    actor <> '' AND source <> '' AND idempotency_key <> ''
  ),
  CONSTRAINT weave_team_run_transitions_error_shape_check CHECK (
    (
      to_status IN ('failed', 'abandoned')
      AND error_code IS NOT NULL
      AND error_code <> ''
    )
    OR (
      to_status = 'cancelled'
      AND error_code = 'team_run_cancelled'
      AND cause_summary IS NULL
    )
    OR (
      to_status NOT IN ('failed', 'abandoned', 'cancelled')
      AND error_code IS NULL
      AND cause_summary IS NULL
    )
  ),
  CONSTRAINT weave_team_run_transitions_cause_summary_check CHECK (
    cause_summary IS NULL
    OR (
      error_code IS NOT NULL
      AND cause_summary <> ''
      AND octet_length(cause_summary) <= 1024
    )
  ),
  CONSTRAINT weave_team_run_transitions_allowed_check CHECK (
    (
      orphaned
      AND from_status IS NOT NULL
      AND from_status = to_status
    )
    OR (
      NOT orphaned
      AND (
        (from_status IS NULL AND to_status = 'queued')
        OR (from_status = 'queued' AND to_status IN ('running', 'cancelled'))
        OR (
          from_status = 'running'
          AND to_status IN (
            'running', 'parked', 'succeeded', 'failed',
            'cancel_requested', 'abandoned'
          )
        )
        OR (
          from_status = 'parked'
          AND to_status IN ('running', 'failed', 'cancel_requested')
        )
        OR (
          from_status = 'cancel_requested'
          AND to_status IN ('cancelled', 'abandoned')
        )
      )
    )
  )
);

CREATE INDEX weave_team_run_transitions_occurred_idx
  ON weave_team_run_transitions (
    workspace_id, run_id, occurred_at, seq
  );

CREATE FUNCTION weave_team_run_transitions_reject_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'team run transition ledger is append-only'
    USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER weave_team_run_transitions_append_only
BEFORE UPDATE OR DELETE ON weave_team_run_transitions
FOR EACH ROW
EXECUTE FUNCTION weave_team_run_transitions_reject_mutation();
