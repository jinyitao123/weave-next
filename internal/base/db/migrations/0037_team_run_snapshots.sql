-- Immutable per-run configuration snapshots. Workflow and artifact references
-- stay nullable until the fixed-workflow publishing tables are introduced;
-- free-collaboration runs freeze their dependencies inline instead.
CREATE TABLE weave_team_run_snapshots (
  run_id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL,
  team_id TEXT NOT NULL,
  workflow_id TEXT,
  workflow_version INT,
  lead_avatar_id TEXT NOT NULL,
  lead_avatar_version INT NOT NULL,
  worker_versions JSONB NOT NULL,
  team_worker_snapshot JSONB NOT NULL,
  artifact_ref TEXT,
  admission_decision JSONB NOT NULL,
  inline_dependencies JSONB,
  run_associations JSONB NOT NULL,
  trigger_source TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT weave_team_run_snapshots_workflow_pair_check CHECK (
    (workflow_id IS NULL AND workflow_version IS NULL)
    OR (
      workflow_id IS NOT NULL AND workflow_id <> ''
      AND workflow_version IS NOT NULL AND workflow_version > 0
    )
  ),
  CONSTRAINT weave_team_run_snapshots_lead_version_check
    CHECK (lead_avatar_version > 0),
  CONSTRAINT weave_team_run_snapshots_worker_versions_check
    CHECK (jsonb_typeof(worker_versions) IS NOT DISTINCT FROM 'object'),
  CONSTRAINT weave_team_run_snapshots_team_workers_check
    CHECK (jsonb_typeof(team_worker_snapshot) IS NOT DISTINCT FROM 'array'),
  CONSTRAINT weave_team_run_snapshots_admission_check CHECK (
    jsonb_typeof(admission_decision) IS NOT DISTINCT FROM 'object'
    AND admission_decision <> '{}'::jsonb
  ),
  CONSTRAINT weave_team_run_snapshots_inline_dependencies_check CHECK (
    inline_dependencies IS NULL
    OR jsonb_typeof(inline_dependencies) IS NOT DISTINCT FROM 'object'
  ),
  CONSTRAINT weave_team_run_snapshots_run_associations_check
    CHECK (jsonb_typeof(run_associations) IS NOT DISTINCT FROM 'object'),
  CONSTRAINT weave_team_run_snapshots_team_fk
    FOREIGN KEY (workspace_id, team_id)
    REFERENCES weave_teams (workspace_id, id) ON DELETE RESTRICT,
  CONSTRAINT weave_team_run_snapshots_lead_fk
    FOREIGN KEY (workspace_id, lead_avatar_id)
    REFERENCES weave_agents (workspace_id, id) ON DELETE RESTRICT,
  CONSTRAINT weave_team_run_snapshots_lead_version_fk
    FOREIGN KEY (lead_avatar_id, lead_avatar_version)
    REFERENCES weave_agent_versions (agent_id, version) ON DELETE RESTRICT
);

CREATE FUNCTION weave_team_run_snapshots_reject_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'team run snapshots are immutable'
    USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER weave_team_run_snapshots_immutable
BEFORE UPDATE OR DELETE ON weave_team_run_snapshots
FOR EACH ROW
EXECUTE FUNCTION weave_team_run_snapshots_reject_mutation();
