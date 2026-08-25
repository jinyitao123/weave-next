CREATE TABLE weave_team_build_execution_jobs (
  workspace_id TEXT NOT NULL,
  build_run_id TEXT NOT NULL,
  status TEXT NOT NULL,
  requested_at TIMESTAMPTZ NOT NULL,
  started_at TIMESTAMPTZ,
  completed_at TIMESTAMPTZ,
  updated_at TIMESTAMPTZ NOT NULL,
  worker_id TEXT,
  lease_epoch BIGINT NOT NULL DEFAULT 0,
  lease_until TIMESTAMPTZ,
  last_error TEXT NOT NULL DEFAULT '',

  CONSTRAINT weave_team_build_execution_jobs_pkey PRIMARY KEY (
    workspace_id, build_run_id
  ),
  CONSTRAINT weave_team_build_execution_jobs_run_fkey FOREIGN KEY (
    workspace_id, build_run_id
  ) REFERENCES weave_team_build_runs (
    workspace_id, build_run_id
  ) ON DELETE RESTRICT,
  CONSTRAINT weave_team_build_execution_jobs_status_check CHECK (
    status IN ('queued', 'running', 'succeeded', 'failed')
  ),
  CONSTRAINT weave_team_build_execution_jobs_lease_check CHECK (
    (
      status = 'running'
      AND worker_id IS NOT NULL
      AND worker_id <> ''
      AND lease_until IS NOT NULL
      AND started_at IS NOT NULL
      AND completed_at IS NULL
    )
    OR (
      status <> 'running'
      AND worker_id IS NULL
      AND lease_until IS NULL
    )
  ),
  CONSTRAINT weave_team_build_execution_jobs_completion_check CHECK (
    (status IN ('succeeded', 'failed')) = (completed_at IS NOT NULL)
  ),
  CONSTRAINT weave_team_build_execution_jobs_time_check CHECK (
    requested_at <= updated_at
    AND (started_at IS NULL OR requested_at <= started_at)
    AND (completed_at IS NULL OR started_at <= completed_at)
  )
);

CREATE INDEX weave_team_build_execution_jobs_claim_idx
  ON weave_team_build_execution_jobs (status, lease_until, requested_at)
  WHERE status IN ('queued', 'running');
