ALTER TABLE weave_team_workflow_dependencies
  DROP CONSTRAINT weave_team_workflow_dependencies_pkey;

ALTER TABLE weave_team_workflow_dependencies
  ADD COLUMN owner_agent_version_key BIGINT
    GENERATED ALWAYS AS (COALESCE(owner_agent_version, 0::BIGINT)) STORED;

ALTER TABLE weave_team_workflow_dependencies
  ADD CONSTRAINT weave_team_workflow_dependencies_pkey PRIMARY KEY (
    workspace_id,
    workflow_id,
    workflow_version,
    owner_type,
    owner_id,
    owner_agent_version_key,
    dependency_type,
    dependency_key
  );
