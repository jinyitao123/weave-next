ALTER TABLE weave_team_build_runs
  ADD COLUMN authorization_authority TEXT,
  ADD COLUMN authorized_revision_no INTEGER,
  ADD COLUMN authorized_blueprint_hash TEXT,
  ADD COLUMN authorized_change_set_hash TEXT;

ALTER TABLE weave_team_build_runs
  ADD CONSTRAINT weave_team_build_runs_authorization_authority_check CHECK (
    authorization_authority IS NULL
    OR authorization_authority IN ('reviewed_blueprint', 'auto_build')
  ),
  ADD CONSTRAINT weave_team_build_runs_authorized_revision_check CHECK (
    (
      authorization_authority = 'reviewed_blueprint'
      AND authorized_revision_no IS NOT NULL
      AND authorized_revision_no > 0
      AND authorized_blueprint_hash ~ '^[0-9a-f]{64}$'
      AND authorized_change_set_hash ~ '^[0-9a-f]{64}$'
    )
    OR (
      authorization_authority IS NULL
      OR authorization_authority = 'auto_build'
    )
  );

CREATE TABLE weave_team_run_interactions (
  workspace_id TEXT NOT NULL,
  run_id TEXT NOT NULL,
  route_key TEXT NOT NULL,
  worker_agent_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  attempt_no INTEGER NOT NULL,
  failure_class TEXT,
  failure_cause TEXT,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,

  CONSTRAINT weave_team_run_interactions_pkey PRIMARY KEY (
    workspace_id, run_id, route_key, attempt_no
  ),
  CONSTRAINT weave_team_run_interactions_kind_check CHECK (
    kind IN ('consult', 'dispatch', 'handoff')
  ),
  CONSTRAINT weave_team_run_interactions_attempt_check CHECK (
    attempt_no > 0
  ),
  CONSTRAINT weave_team_run_interactions_failure_class_check CHECK (
    failure_class IS NULL OR failure_class IN ('timeout', 'infra', 'business')
  )
);

CREATE INDEX weave_team_run_interactions_worker_idx
  ON weave_team_run_interactions (workspace_id, run_id, worker_agent_id, created_at);
