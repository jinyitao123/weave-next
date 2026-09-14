CREATE TABLE weave_team_build_drafts (
  workspace_id TEXT NOT NULL,
  build_run_id TEXT NOT NULL,
  draft_kind TEXT NOT NULL,
  draft_key TEXT NOT NULL,
  payload_json JSONB NOT NULL,
  revision BIGINT NOT NULL DEFAULT 1,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

  CONSTRAINT weave_team_build_drafts_pkey
    PRIMARY KEY (workspace_id, build_run_id, draft_kind, draft_key),
  CONSTRAINT weave_team_build_drafts_run_fkey
    FOREIGN KEY (workspace_id, build_run_id)
    REFERENCES weave_team_build_runs(workspace_id, build_run_id)
    ON DELETE CASCADE,
  CONSTRAINT weave_team_build_drafts_identity_check CHECK (
    workspace_id <> '' AND build_run_id <> '' AND draft_key <> ''
  ),
  CONSTRAINT weave_team_build_drafts_kind_check CHECK (
    draft_kind IN ('agent_graph', 'team_workflow')
  ),
  CONSTRAINT weave_team_build_drafts_payload_check CHECK (
    jsonb_typeof(payload_json) = 'object'
  ),
  CONSTRAINT weave_team_build_drafts_revision_check CHECK (
    revision > 0
  )
);
