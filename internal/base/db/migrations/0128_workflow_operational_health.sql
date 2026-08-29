CREATE TABLE weave_workflow_health_observations (
  workspace_id TEXT NOT NULL,
  workflow_id TEXT NOT NULL,
  workflow_version INTEGER NOT NULL,
  artifact_content_hash TEXT NOT NULL,
  observation_id TEXT NOT NULL,
  ruleset_version INTEGER NOT NULL,
  config_json JSONB NOT NULL,
  config_hash TEXT NOT NULL,
  fact_hash TEXT NOT NULL,
  cutoff_at TIMESTAMPTZ NOT NULL,
  window_started_at TIMESTAMPTZ,
  window_ended_at TIMESTAMPTZ,
  sample_count INTEGER NOT NULL,
  succeeded_count INTEGER NOT NULL,
  failed_count INTEGER NOT NULL,
  slow_count INTEGER NOT NULL,
  human_completed_count INTEGER NOT NULL,
  human_timeout_count INTEGER NOT NULL,
  selected_run_ids JSONB NOT NULL,
  reason_codes JSONB NOT NULL,
  conclusion TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, observation_id),
  UNIQUE (
    workspace_id, workflow_id, workflow_version, artifact_content_hash,
    ruleset_version, config_hash, fact_hash
  ),
  CONSTRAINT weave_workflow_health_observations_identity_check CHECK (
    workspace_id <> '' AND workflow_id <> '' AND workflow_version > 0
    AND artifact_content_hash ~ '^[0-9a-f]{64}$'
    AND observation_id <> '' AND ruleset_version > 0
    AND config_hash ~ '^[0-9a-f]{64}$' AND fact_hash ~ '^[0-9a-f]{64}$'
  ),
  CONSTRAINT weave_workflow_health_observations_count_check CHECK (
    sample_count >= 0 AND succeeded_count >= 0 AND failed_count >= 0 AND slow_count >= 0
    AND human_completed_count >= 0 AND human_timeout_count >= 0
    AND succeeded_count + failed_count = sample_count AND slow_count <= sample_count
    AND human_completed_count <= sample_count AND human_timeout_count <= sample_count
  ),
  CONSTRAINT weave_workflow_health_observations_json_check CHECK (
    jsonb_typeof(config_json) IS NOT DISTINCT FROM 'object'
    AND jsonb_typeof(selected_run_ids) IS NOT DISTINCT FROM 'array'
    AND jsonb_typeof(reason_codes) IS NOT DISTINCT FROM 'array'
  ),
  CONSTRAINT weave_workflow_health_observations_conclusion_check
    CHECK (conclusion IN ('unknown', 'healthy', 'warning')),
  CONSTRAINT weave_workflow_health_observations_window_check CHECK (
    (sample_count = 0 AND window_started_at IS NULL AND window_ended_at IS NULL)
    OR (sample_count > 0 AND window_started_at IS NOT NULL AND window_ended_at IS NOT NULL
      AND window_started_at <= window_ended_at AND window_ended_at <= cutoff_at)
  ),
  CONSTRAINT weave_workflow_health_observations_artifact_fk FOREIGN KEY (
    workspace_id, workflow_id, workflow_version
  ) REFERENCES weave_published_artifact_contents (
    workspace_id, workflow_id, workflow_version
  ) ON DELETE RESTRICT
);

CREATE TABLE weave_workflow_current_health (
  workspace_id TEXT NOT NULL,
  workflow_id TEXT NOT NULL,
  workflow_version INTEGER NOT NULL,
  artifact_content_hash TEXT NOT NULL,
  observation_id TEXT NOT NULL,
  conclusion TEXT NOT NULL,
  observed_at TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (workspace_id, workflow_id, workflow_version, artifact_content_hash),
  CONSTRAINT weave_workflow_current_health_conclusion_check
    CHECK (conclusion IN ('unknown', 'healthy', 'warning')),
  CONSTRAINT weave_workflow_current_health_observation_fk FOREIGN KEY (
    workspace_id, observation_id
  ) REFERENCES weave_workflow_health_observations (
    workspace_id, observation_id
  ) ON DELETE RESTRICT
);

CREATE INDEX weave_workflow_health_observations_version_created_idx
  ON weave_workflow_health_observations (
    workspace_id, workflow_id, workflow_version, artifact_content_hash, created_at DESC
  );

CREATE FUNCTION weave_workflow_health_observations_reject_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'workflow health observations are immutable'
    USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER weave_workflow_health_observations_immutable
BEFORE UPDATE OR DELETE ON weave_workflow_health_observations
FOR EACH ROW
EXECUTE FUNCTION weave_workflow_health_observations_reject_mutation();
