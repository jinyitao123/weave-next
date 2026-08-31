CREATE TABLE weave_team_run_activity_events (
  workspace_id TEXT NOT NULL,
  run_id TEXT NOT NULL,
  seq BIGSERIAL NOT NULL,
  event_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  node_id TEXT,
  member_id TEXT,
  member_version BIGINT,
  detail JSONB NOT NULL DEFAULT '{}'::jsonb,
  occurred_at TIMESTAMPTZ NOT NULL,

  CONSTRAINT weave_team_run_activity_events_pkey PRIMARY KEY (workspace_id, run_id, seq),
  CONSTRAINT weave_team_run_activity_events_event_key UNIQUE (workspace_id, run_id, event_id),
  CONSTRAINT weave_team_run_activity_events_run_fkey FOREIGN KEY (workspace_id, run_id)
    REFERENCES weave_team_runs (workspace_id, run_id) ON DELETE RESTRICT,
  CONSTRAINT weave_team_run_activity_events_identity_check CHECK (
    workspace_id <> '' AND run_id <> '' AND event_id <> '' AND kind <> ''
    AND (node_id IS NULL OR node_id <> '')
    AND (member_id IS NULL OR member_id <> '')
    AND (member_version IS NULL OR member_version > 0)
    AND jsonb_typeof(detail) = 'object'
  )
);

CREATE INDEX weave_team_run_activity_events_timeline_idx
  ON weave_team_run_activity_events (workspace_id, run_id, occurred_at, seq);

CREATE FUNCTION weave_team_run_activity_events_reject_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'team run activity ledger is append-only'
    USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER weave_team_run_activity_events_append_only
BEFORE UPDATE OR DELETE ON weave_team_run_activity_events
FOR EACH ROW
EXECUTE FUNCTION weave_team_run_activity_events_reject_mutation();

CREATE TABLE weave_team_run_corrections (
  workspace_id TEXT NOT NULL,
  run_id TEXT NOT NULL,
  correction_id TEXT NOT NULL,
  idempotency_key TEXT NOT NULL,
  target_kind TEXT NOT NULL,
  target_member_id TEXT,
  instruction TEXT NOT NULL,
  status TEXT NOT NULL,
  requested_generation BIGINT NOT NULL,
  requested_execution_lease_epoch BIGINT NOT NULL,
  safe_node_id TEXT,
  restart_node_id TEXT,
  affected_node_ids JSONB,
  preserved_node_ids JSONB,
  requested_by TEXT NOT NULL,
  requested_at TIMESTAMPTZ NOT NULL,
  ready_at TIMESTAMPTZ,
  confirmed_by TEXT,
  confirmed_at TIMESTAMPTZ,
  applied_at TIMESTAMPTZ,

  CONSTRAINT weave_team_run_corrections_pkey PRIMARY KEY (workspace_id, run_id, correction_id),
  CONSTRAINT weave_team_run_corrections_idempotency_key UNIQUE (workspace_id, run_id, idempotency_key),
  CONSTRAINT weave_team_run_corrections_run_fkey FOREIGN KEY (workspace_id, run_id)
    REFERENCES weave_team_runs (workspace_id, run_id) ON DELETE RESTRICT,
  CONSTRAINT weave_team_run_corrections_identity_check CHECK (
    workspace_id <> '' AND run_id <> '' AND correction_id <> '' AND idempotency_key <> ''
    AND instruction <> '' AND octet_length(instruction) <= 16384 AND requested_by <> ''
  ),
  CONSTRAINT weave_team_run_corrections_target_check CHECK (
    (target_kind = 'team' AND target_member_id IS NULL)
    OR (target_kind = 'member' AND target_member_id IS NOT NULL AND target_member_id <> '')
  ),
  CONSTRAINT weave_team_run_corrections_status_check CHECK (
    status IN ('requested', 'ready', 'confirmed', 'discarded', 'applied')
  ),
  CONSTRAINT weave_team_run_corrections_generation_check CHECK (
    requested_generation >= 0 AND requested_execution_lease_epoch >= 0
  ),
  CONSTRAINT weave_team_run_corrections_plan_check CHECK (
    (
      status = 'requested'
      AND safe_node_id IS NULL AND restart_node_id IS NULL
      AND affected_node_ids IS NULL AND preserved_node_ids IS NULL AND ready_at IS NULL
    )
    OR (
      status <> 'requested'
      AND safe_node_id IS NOT NULL AND safe_node_id <> ''
      AND restart_node_id IS NOT NULL AND restart_node_id <> ''
      AND jsonb_typeof(affected_node_ids) = 'array'
      AND jsonb_typeof(preserved_node_ids) = 'array'
      AND ready_at IS NOT NULL
    )
  ),
  CONSTRAINT weave_team_run_corrections_confirmation_check CHECK (
    (status IN ('confirmed', 'applied')) = (confirmed_by IS NOT NULL AND confirmed_at IS NOT NULL)
    AND (confirmed_by IS NULL OR confirmed_by <> '')
    AND (status = 'applied') = (applied_at IS NOT NULL)
  ),
  CONSTRAINT weave_team_run_corrections_time_check CHECK (
    (ready_at IS NULL OR ready_at >= requested_at)
    AND (confirmed_at IS NULL OR confirmed_at >= ready_at)
    AND (applied_at IS NULL OR applied_at >= confirmed_at)
  )
);

CREATE UNIQUE INDEX weave_team_run_corrections_one_active_idx
  ON weave_team_run_corrections (workspace_id, run_id)
  WHERE status IN ('requested', 'ready', 'confirmed');

CREATE INDEX weave_team_run_corrections_timeline_idx
  ON weave_team_run_corrections (workspace_id, run_id, requested_at DESC, correction_id DESC);

CREATE TABLE weave_team_run_correction_events (
  workspace_id TEXT NOT NULL,
  run_id TEXT NOT NULL,
  correction_id TEXT NOT NULL,
  seq BIGSERIAL NOT NULL,
  event_kind TEXT NOT NULL,
  actor TEXT NOT NULL,
  detail JSONB NOT NULL DEFAULT '{}'::jsonb,
  occurred_at TIMESTAMPTZ NOT NULL,

  CONSTRAINT weave_team_run_correction_events_pkey PRIMARY KEY (workspace_id, run_id, correction_id, seq),
  CONSTRAINT weave_team_run_correction_events_correction_fkey FOREIGN KEY (workspace_id, run_id, correction_id)
    REFERENCES weave_team_run_corrections (workspace_id, run_id, correction_id) ON DELETE RESTRICT,
  CONSTRAINT weave_team_run_correction_events_identity_check CHECK (
    event_kind <> '' AND actor <> '' AND jsonb_typeof(detail) = 'object'
  )
);

CREATE FUNCTION weave_team_run_correction_events_reject_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'team run correction event ledger is append-only'
    USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER weave_team_run_correction_events_append_only
BEFORE UPDATE OR DELETE ON weave_team_run_correction_events
FOR EACH ROW
EXECUTE FUNCTION weave_team_run_correction_events_reject_mutation();
