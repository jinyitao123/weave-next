CREATE TABLE weave_team_build_runs (
  workspace_id TEXT NOT NULL,
  build_run_id TEXT NOT NULL,
  mode TEXT NOT NULL,
  status TEXT NOT NULL,

  brief_json JSONB NOT NULL,
  brief_hash TEXT NOT NULL,
  contract_json JSONB NOT NULL,
  contract_hash TEXT NOT NULL,
  asset_scope_json JSONB NOT NULL,
  baseline_snapshot_json JSONB,
  round_budget_json JSONB NOT NULL,
  total_budget_json JSONB NOT NULL,

  expires_at TIMESTAMPTZ NOT NULL,
  publish_eligible BOOLEAN NOT NULL DEFAULT FALSE,
  rollback_status TEXT NOT NULL DEFAULT 'none',
  confirmed_by TEXT,
  final_ref_json JSONB,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  decided_at TIMESTAMPTZ,

  CONSTRAINT weave_team_build_runs_pkey PRIMARY KEY (workspace_id, build_run_id),
  CONSTRAINT weave_team_build_runs_identity_check CHECK (
    workspace_id <> '' AND build_run_id <> ''
  ),
  CONSTRAINT weave_team_build_runs_mode_check CHECK (
    mode IN ('create', 'optimize')
  ),
  CONSTRAINT weave_team_build_runs_status_check CHECK (
    status IN (
      'planning', 'authorized', 'round_running',
      'publishing', 'passed', 'blocked', 'cancelled'
    )
  ),
  CONSTRAINT weave_team_build_runs_rollback_status_check CHECK (
    rollback_status IN ('none', 'rolled_back', 'rollback_failed')
  ),
  CONSTRAINT weave_team_build_runs_hash_format_check CHECK (
    brief_hash ~ '^[0-9a-f]{64}$'
    AND contract_hash ~ '^[0-9a-f]{64}$'
  ),
  CONSTRAINT weave_team_build_runs_json_shape_check CHECK (
    jsonb_typeof(brief_json) = 'object'
    AND jsonb_typeof(contract_json) = 'object'
    AND jsonb_typeof(asset_scope_json) = 'object'
    AND jsonb_typeof(round_budget_json) = 'object'
    AND jsonb_typeof(total_budget_json) = 'object'
    AND (
      baseline_snapshot_json IS NULL
      OR jsonb_typeof(baseline_snapshot_json) = 'object'
    )
    AND (
      final_ref_json IS NULL
      OR jsonb_typeof(final_ref_json) = 'object'
    )
  ),
  CONSTRAINT weave_team_build_runs_brief_derived_check CHECK (
    brief_json->>'mode' = mode
    AND asset_scope_json = brief_json->'allowed_assets'
    AND round_budget_json = brief_json->'round_budget'
    AND total_budget_json = brief_json->'total_budget'
  ),
  CONSTRAINT weave_team_build_runs_baseline_shape_check CHECK (
    (mode = 'optimize' AND status <> 'planning') = (
      baseline_snapshot_json IS NOT NULL
    )
  ),
  CONSTRAINT weave_team_build_runs_planning_shape_check CHECK (
    (status = 'planning') = (confirmed_by IS NULL)
  ),
  CONSTRAINT weave_team_build_runs_decided_shape_check CHECK (
    (
      status IN ('passed', 'blocked', 'cancelled')
      AND decided_at IS NOT NULL
      AND created_at <= updated_at
      AND updated_at <= decided_at
    )
    OR (
      status NOT IN ('passed', 'blocked', 'cancelled')
      AND decided_at IS NULL
      AND created_at <= updated_at
    )
  ),
  CONSTRAINT weave_team_build_runs_expiry_check CHECK (
    expires_at > created_at
  ),
  CONSTRAINT weave_team_build_runs_publish_eligible_check CHECK (
    publish_eligible = (status IN ('publishing', 'passed'))
  ),
  CONSTRAINT weave_team_build_runs_final_ref_shape_check CHECK (
    (status = 'passed') = (final_ref_json IS NOT NULL)
  ),
  CONSTRAINT weave_team_build_runs_optional_text_check CHECK (
    (confirmed_by IS NULL OR confirmed_by <> '')
    AND (final_ref_json IS NULL OR (final_ref_json->>'ref') <> '')
  )
);

CREATE INDEX weave_team_build_runs_status_updated_idx
  ON weave_team_build_runs (status, updated_at, workspace_id, build_run_id);

CREATE INDEX weave_team_build_runs_expiry_idx
  ON weave_team_build_runs (expires_at, workspace_id, build_run_id)
  WHERE status NOT IN ('passed', 'blocked', 'cancelled');

CREATE TABLE weave_team_build_run_transitions (
  workspace_id TEXT NOT NULL,
  build_run_id TEXT NOT NULL,
  seq BIGINT NOT NULL,
  from_status TEXT,
  to_status TEXT NOT NULL,
  reason TEXT NOT NULL,
  actor TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL,

  CONSTRAINT weave_team_build_run_transitions_pkey PRIMARY KEY (
    workspace_id, build_run_id, seq
  ),
  CONSTRAINT weave_team_build_run_transitions_run_fkey FOREIGN KEY (
    workspace_id, build_run_id
  ) REFERENCES weave_team_build_runs (
    workspace_id, build_run_id
  ) ON DELETE RESTRICT,
  CONSTRAINT weave_team_build_run_transitions_seq_check CHECK (seq > 0),
  CONSTRAINT weave_team_build_run_transitions_status_check CHECK (
    (
      from_status IS NULL
      OR from_status IN (
        'planning', 'authorized', 'round_running',
        'publishing', 'passed', 'blocked', 'cancelled'
      )
    )
    AND to_status IN (
      'planning', 'authorized', 'round_running',
      'publishing', 'passed', 'blocked', 'cancelled'
    )
  ),
  CONSTRAINT weave_team_build_run_transitions_identity_check CHECK (
    reason <> '' AND actor <> ''
  ),
  CONSTRAINT weave_team_build_run_transitions_allowed_check CHECK (
    (from_status IS NULL AND to_status = 'planning')
    OR (
      from_status = 'planning'
      AND to_status IN ('authorized', 'cancelled')
    )
    OR (
      from_status = 'authorized'
      AND to_status IN ('round_running', 'blocked', 'cancelled')
    )
    OR (
      from_status = 'round_running'
      AND to_status IN ('authorized', 'publishing', 'blocked', 'cancelled')
    )
    OR (
      from_status = 'publishing'
      AND to_status IN ('passed', 'blocked', 'cancelled')
    )
  )
);

CREATE INDEX weave_team_build_run_transitions_occurred_idx
  ON weave_team_build_run_transitions (
    workspace_id, build_run_id, created_at, seq
  );

CREATE FUNCTION weave_team_build_run_transitions_reject_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'team build run transition ledger is append-only'
    USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER weave_team_build_run_transitions_append_only
BEFORE UPDATE OR DELETE ON weave_team_build_run_transitions
FOR EACH ROW
EXECUTE FUNCTION weave_team_build_run_transitions_reject_mutation();

CREATE TABLE weave_team_build_run_rounds (
  workspace_id TEXT NOT NULL,
  build_run_id TEXT NOT NULL,
  round_no INTEGER NOT NULL,
  candidate_ref TEXT NOT NULL,
  report_ref TEXT NOT NULL,
  conclusion TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL,

  CONSTRAINT weave_team_build_run_rounds_pkey PRIMARY KEY (
    workspace_id, build_run_id, round_no
  ),
  CONSTRAINT weave_team_build_run_rounds_run_fkey FOREIGN KEY (
    workspace_id, build_run_id
  ) REFERENCES weave_team_build_runs (
    workspace_id, build_run_id
  ) ON DELETE RESTRICT,
  CONSTRAINT weave_team_build_run_rounds_identity_check CHECK (
    round_no > 0 AND candidate_ref <> '' AND report_ref <> ''
  ),
  CONSTRAINT weave_team_build_run_rounds_conclusion_check CHECK (
    conclusion IN ('pass', 'revise', 'blocked')
  )
);

CREATE INDEX weave_team_build_run_rounds_occurred_idx
  ON weave_team_build_run_rounds (workspace_id, build_run_id, created_at);

CREATE FUNCTION weave_team_build_run_rounds_reject_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'team build run round ledger is append-only'
    USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER weave_team_build_run_rounds_append_only
BEFORE UPDATE OR DELETE ON weave_team_build_run_rounds
FOR EACH ROW
EXECUTE FUNCTION weave_team_build_run_rounds_reject_mutation();
