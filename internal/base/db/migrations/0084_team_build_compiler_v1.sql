ALTER TABLE weave_team_build_runs
  ADD COLUMN execution_strategy TEXT NOT NULL DEFAULT 'legacy';

ALTER TABLE weave_team_build_runs
  ADD CONSTRAINT weave_team_build_runs_execution_strategy_check CHECK (
    execution_strategy IN ('legacy', 'compiler_v1')
  );

CREATE TABLE weave_team_build_blueprint_revisions (
  workspace_id TEXT NOT NULL,
  build_run_id TEXT NOT NULL,
  revision_no INTEGER NOT NULL,
  blueprint_json JSONB NOT NULL,
  blueprint_hash TEXT NOT NULL,
  change_set_json JSONB NOT NULL,
  change_set_hash TEXT NOT NULL,
  baseline_hash TEXT NOT NULL,
  workflow_mode TEXT NOT NULL,
  template_gap_authorization_json JSONB,
  template_gap_authorization_hash TEXT,
  evaluation_contract_hash TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL,

  CONSTRAINT weave_team_build_blueprint_revisions_pkey PRIMARY KEY (
    workspace_id, build_run_id, revision_no
  ),
  CONSTRAINT weave_team_build_blueprint_revisions_run_fkey FOREIGN KEY (
    workspace_id, build_run_id
  ) REFERENCES weave_team_build_runs (
    workspace_id, build_run_id
  ) ON DELETE RESTRICT,
  CONSTRAINT weave_team_build_blueprint_revisions_revision_check CHECK (
    revision_no > 0
  ),
  CONSTRAINT weave_team_build_blueprint_revisions_hash_check CHECK (
    blueprint_hash ~ '^[0-9a-f]{64}$'
    AND change_set_hash ~ '^[0-9a-f]{64}$'
    AND baseline_hash ~ '^[0-9a-f]{64}$'
    AND evaluation_contract_hash ~ '^[0-9a-f]{64}$'
    AND (
      template_gap_authorization_hash IS NULL
      OR template_gap_authorization_hash ~ '^[0-9a-f]{64}$'
    )
  ),
  CONSTRAINT weave_team_build_blueprint_revisions_json_check CHECK (
    jsonb_typeof(blueprint_json) = 'object'
    AND jsonb_typeof(change_set_json) = 'object'
    AND (
      template_gap_authorization_json IS NULL
      OR jsonb_typeof(template_gap_authorization_json) = 'object'
    )
  ),
  CONSTRAINT weave_team_build_blueprint_revisions_workflow_check CHECK (
    (
      workflow_mode = 'template'
      AND template_gap_authorization_json IS NULL
      AND template_gap_authorization_hash IS NULL
    )
    OR (
      workflow_mode = 'custom'
      AND template_gap_authorization_json IS NOT NULL
      AND template_gap_authorization_hash IS NOT NULL
    )
  )
);

CREATE INDEX weave_team_build_blueprint_revisions_created_idx
  ON weave_team_build_blueprint_revisions (
    workspace_id, build_run_id, created_at, revision_no
  );

CREATE FUNCTION weave_team_build_blueprint_revisions_guard()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
  expected_revision INTEGER;
BEGIN
  IF TG_OP <> 'INSERT' THEN
    RAISE EXCEPTION 'team build blueprint revisions are append-only'
      USING ERRCODE = '23514';
  END IF;

  PERFORM 1
  FROM weave_team_build_runs
  WHERE workspace_id = NEW.workspace_id
    AND build_run_id = NEW.build_run_id
  FOR UPDATE;

  SELECT COALESCE(MAX(revision_no), 0) + 1
    INTO expected_revision
  FROM weave_team_build_blueprint_revisions
  WHERE workspace_id = NEW.workspace_id
    AND build_run_id = NEW.build_run_id;

  IF NEW.revision_no <> expected_revision THEN
    RAISE EXCEPTION 'team build blueprint revision must append at %, got %',
      expected_revision, NEW.revision_no
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER weave_team_build_blueprint_revisions_guard_trigger
BEFORE INSERT OR UPDATE OR DELETE ON weave_team_build_blueprint_revisions
FOR EACH ROW
EXECUTE FUNCTION weave_team_build_blueprint_revisions_guard();

CREATE TABLE weave_team_build_operation_steps (
  workspace_id TEXT NOT NULL,
  build_run_id TEXT NOT NULL,
  revision_no INTEGER NOT NULL,
  operation_id TEXT NOT NULL,
  operation_index INTEGER NOT NULL,
  operation_type TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending',
  depends_on JSONB NOT NULL DEFAULT '[]'::jsonb,
  input_hash TEXT NOT NULL,
  lease_owner TEXT,
  lease_epoch BIGINT NOT NULL DEFAULT 0,
  lease_until TIMESTAMPTZ,
  attempt INTEGER NOT NULL DEFAULT 0,
  error_class TEXT,
  error_code TEXT,
  evidence_json JSONB,
  output_hash TEXT,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  started_at TIMESTAMPTZ,
  completed_at TIMESTAMPTZ,

  CONSTRAINT weave_team_build_operation_steps_pkey PRIMARY KEY (
    workspace_id, build_run_id, revision_no, operation_id
  ),
  CONSTRAINT weave_team_build_operation_steps_revision_fkey FOREIGN KEY (
    workspace_id, build_run_id, revision_no
  ) REFERENCES weave_team_build_blueprint_revisions (
    workspace_id, build_run_id, revision_no
  ) ON DELETE RESTRICT,
  CONSTRAINT weave_team_build_operation_steps_identity_check CHECK (
    operation_id ~ '^[0-9a-f]{64}$'
    AND operation_index >= 0
  ),
  CONSTRAINT weave_team_build_operation_steps_type_check CHECK (
    operation_type IN (
      'agent_create', 'agent_update', 'team_create', 'team_update',
      'roster_set', 'agent_graph_compile', 'workflow_compile',
      'candidate_run', 'publish'
    )
  ),
  CONSTRAINT weave_team_build_operation_steps_status_check CHECK (
    status IN ('pending', 'running', 'succeeded', 'skipped', 'failed')
  ),
  CONSTRAINT weave_team_build_operation_steps_hash_check CHECK (
    input_hash ~ '^[0-9a-f]{64}$'
    AND (output_hash IS NULL OR output_hash ~ '^[0-9a-f]{64}$')
  ),
  CONSTRAINT weave_team_build_operation_steps_dependency_check CHECK (
    jsonb_typeof(depends_on) = 'array'
  ),
  CONSTRAINT weave_team_build_operation_steps_attempt_check CHECK (
    attempt >= 0 AND lease_epoch >= 0 AND lease_epoch = attempt
  ),
  CONSTRAINT weave_team_build_operation_steps_lease_check CHECK (
    (
      status = 'running'
      AND lease_owner IS NOT NULL
      AND lease_owner <> ''
      AND lease_until IS NOT NULL
      AND started_at IS NOT NULL
      AND completed_at IS NULL
      AND attempt > 0
    )
    OR (
      status <> 'running'
      AND lease_owner IS NULL
      AND lease_until IS NULL
    )
  ),
  CONSTRAINT weave_team_build_operation_steps_terminal_check CHECK (
    (
      status IN ('succeeded', 'skipped')
      AND completed_at IS NOT NULL
      AND evidence_json IS NOT NULL
      AND output_hash IS NOT NULL
      AND error_class IS NULL
      AND error_code IS NULL
    )
    OR (
      status = 'failed'
      AND completed_at IS NOT NULL
      AND evidence_json IS NOT NULL
      AND error_class IS NOT NULL
      AND error_class <> ''
      AND error_code IS NOT NULL
      AND error_code <> ''
    )
    OR (
      status IN ('pending', 'running')
      AND completed_at IS NULL
      AND evidence_json IS NULL
      AND output_hash IS NULL
      AND error_class IS NULL
      AND error_code IS NULL
    )
  ),
  CONSTRAINT weave_team_build_operation_steps_evidence_check CHECK (
    evidence_json IS NULL OR jsonb_typeof(evidence_json) = 'object'
  ),
  CONSTRAINT weave_team_build_operation_steps_time_check CHECK (
    created_at <= updated_at
    AND (started_at IS NULL OR created_at <= started_at)
    AND (completed_at IS NULL OR started_at <= completed_at)
  ),
  CONSTRAINT weave_team_build_operation_steps_index_unique UNIQUE (
    workspace_id, build_run_id, revision_no, operation_index
  )
);

CREATE UNIQUE INDEX weave_team_build_operation_steps_owner_idx
  ON weave_team_build_operation_steps (
    workspace_id, build_run_id, revision_no, lease_owner
  )
  WHERE status = 'running';

CREATE INDEX weave_team_build_operation_steps_ready_idx
  ON weave_team_build_operation_steps (
    workspace_id, build_run_id, revision_no, status, operation_index
  )
  WHERE status IN ('pending', 'running');

CREATE FUNCTION weave_team_build_operation_steps_guard()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'team build operation steps cannot be deleted'
      USING ERRCODE = '23514';
  END IF;

  IF OLD.status IN ('succeeded', 'skipped', 'failed') THEN
    RAISE EXCEPTION 'terminal team build operation step is immutable'
      USING ERRCODE = '23514';
  END IF;

  IF OLD.workspace_id <> NEW.workspace_id
     OR OLD.build_run_id <> NEW.build_run_id
     OR OLD.revision_no <> NEW.revision_no
     OR OLD.operation_id <> NEW.operation_id
     OR OLD.operation_index <> NEW.operation_index
     OR OLD.operation_type <> NEW.operation_type
     OR OLD.depends_on <> NEW.depends_on
     OR OLD.input_hash <> NEW.input_hash
     OR OLD.created_at <> NEW.created_at THEN
    RAISE EXCEPTION 'immutable team build operation step facts changed'
      USING ERRCODE = '23514';
  END IF;

  IF (OLD.status = 'pending' AND NEW.status NOT IN ('pending', 'running'))
     OR (OLD.status = 'running' AND NEW.status NOT IN ('pending', 'running', 'succeeded', 'skipped', 'failed')) THEN
    RAISE EXCEPTION 'invalid team build operation step transition % -> %',
      OLD.status, NEW.status
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER weave_team_build_operation_steps_guard_trigger
BEFORE UPDATE OR DELETE ON weave_team_build_operation_steps
FOR EACH ROW
EXECUTE FUNCTION weave_team_build_operation_steps_guard();

CREATE TABLE weave_team_build_operation_attempts (
  workspace_id TEXT NOT NULL,
  build_run_id TEXT NOT NULL,
  revision_no INTEGER NOT NULL,
  operation_id TEXT NOT NULL,
  attempt INTEGER NOT NULL,
  lease_epoch BIGINT NOT NULL,
  worker_id TEXT NOT NULL,
  status TEXT NOT NULL,
  error_class TEXT,
  error_code TEXT,
  evidence_json JSONB,
  output_hash TEXT,
  started_at TIMESTAMPTZ NOT NULL,
  completed_at TIMESTAMPTZ,

  CONSTRAINT weave_team_build_operation_attempts_pkey PRIMARY KEY (
    workspace_id, build_run_id, revision_no, operation_id, attempt
  ),
  CONSTRAINT weave_team_build_operation_attempts_step_fkey FOREIGN KEY (
    workspace_id, build_run_id, revision_no, operation_id
  ) REFERENCES weave_team_build_operation_steps (
    workspace_id, build_run_id, revision_no, operation_id
  ) ON DELETE RESTRICT,
  CONSTRAINT weave_team_build_operation_attempts_identity_check CHECK (
    attempt > 0 AND lease_epoch > 0 AND worker_id <> ''
  ),
  CONSTRAINT weave_team_build_operation_attempts_status_check CHECK (
    status IN ('running', 'retryable_failed', 'succeeded', 'skipped', 'failed')
  ),
  CONSTRAINT weave_team_build_operation_attempts_hash_check CHECK (
    output_hash IS NULL OR output_hash ~ '^[0-9a-f]{64}$'
  ),
  CONSTRAINT weave_team_build_operation_attempts_terminal_check CHECK (
    (
      status = 'running'
      AND completed_at IS NULL
      AND error_class IS NULL
      AND error_code IS NULL
      AND evidence_json IS NULL
      AND output_hash IS NULL
    )
    OR (
      status IN ('succeeded', 'skipped')
      AND completed_at IS NOT NULL
      AND error_class IS NULL
      AND error_code IS NULL
      AND evidence_json IS NOT NULL
      AND output_hash IS NOT NULL
    )
    OR (
      status = 'retryable_failed'
      AND completed_at IS NOT NULL
      AND error_class IN ('compile_failure', 'runtime_infrastructure_failure')
      AND error_code IS NOT NULL
      AND error_code <> ''
      AND evidence_json IS NOT NULL
    )
    OR (
      status = 'failed'
      AND completed_at IS NOT NULL
      AND error_class IS NOT NULL
      AND error_class <> ''
      AND error_code IS NOT NULL
      AND error_code <> ''
      AND evidence_json IS NOT NULL
    )
  ),
  CONSTRAINT weave_team_build_operation_attempts_evidence_check CHECK (
    evidence_json IS NULL OR jsonb_typeof(evidence_json) = 'object'
  )
);

CREATE INDEX weave_team_build_operation_attempts_created_idx
  ON weave_team_build_operation_attempts (
    workspace_id, build_run_id, revision_no, operation_id, started_at, attempt
  );

CREATE FUNCTION weave_team_build_operation_attempts_guard()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'team build operation attempt evidence cannot be deleted'
      USING ERRCODE = '23514';
  END IF;
  IF OLD.status <> 'running' THEN
    RAISE EXCEPTION 'terminal team build operation attempt is immutable'
      USING ERRCODE = '23514';
  END IF;
  IF NEW.status NOT IN ('retryable_failed', 'succeeded', 'skipped', 'failed')
     OR OLD.workspace_id <> NEW.workspace_id
     OR OLD.build_run_id <> NEW.build_run_id
     OR OLD.revision_no <> NEW.revision_no
     OR OLD.operation_id <> NEW.operation_id
     OR OLD.attempt <> NEW.attempt
     OR OLD.lease_epoch <> NEW.lease_epoch
     OR OLD.worker_id <> NEW.worker_id
     OR OLD.started_at <> NEW.started_at THEN
    RAISE EXCEPTION 'invalid team build operation attempt transition'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER weave_team_build_operation_attempts_guard_trigger
BEFORE UPDATE OR DELETE ON weave_team_build_operation_attempts
FOR EACH ROW
EXECUTE FUNCTION weave_team_build_operation_attempts_guard();
