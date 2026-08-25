CREATE TABLE weave_expected_run_domain_index (
  workspace_id TEXT NOT NULL,
  run_id TEXT NOT NULL,
  team_id TEXT,
  workflow_id TEXT,
  workflow_version BIGINT,
  run_snapshot_id TEXT,
  created_at TIMESTAMPTZ NOT NULL,

  CONSTRAINT weave_expected_run_domain_index_pkey PRIMARY KEY (workspace_id, run_id),
  CONSTRAINT weave_expected_run_domain_index_identity_nonempty_check CHECK (
    workspace_id <> '' AND run_id <> ''
  ),
  CONSTRAINT weave_expected_run_domain_index_optional_text_check CHECK (
    (team_id IS NULL OR team_id <> '')
    AND (workflow_id IS NULL OR workflow_id <> '')
    AND (run_snapshot_id IS NULL OR run_snapshot_id <> '')
  ),
  CONSTRAINT weave_expected_run_domain_index_workflow_pair_check CHECK (
    (workflow_id IS NULL) = (workflow_version IS NULL)
  ),
  CONSTRAINT weave_expected_run_domain_index_workflow_version_check CHECK (
    workflow_version IS NULL OR workflow_version >= 1
  )
);

CREATE INDEX idx_weave_expected_run_domain_index_team
  ON weave_expected_run_domain_index (workspace_id, team_id);

CREATE INDEX idx_weave_expected_run_domain_index_workflow
  ON weave_expected_run_domain_index (workspace_id, workflow_id, workflow_version);
