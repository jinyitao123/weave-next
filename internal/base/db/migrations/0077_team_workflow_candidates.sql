-- T06: frozen publication candidates for "test what you publish".
--
-- PublicationCandidate currently lives only inside the publish transaction.
-- This migration persists each frozen candidate immutably so the admin test
-- run entry and the same-hash CAS publish can both consume the exact content
-- that the next publication will (or did) freeze.

CREATE TABLE weave_team_workflow_candidates (
  workspace_id TEXT NOT NULL,
  workflow_id TEXT NOT NULL,
  workflow_version INT NOT NULL,
  content_hash TEXT NOT NULL,

  -- Complete canonical Artifact envelope (metadata + canonical payload JSONB).
  envelope_json JSONB NOT NULL,
  dependencies_json JSONB NOT NULL,
  expected_updated_at TIMESTAMPTZ NOT NULL,
  created_by TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL,

  CONSTRAINT weave_team_workflow_candidates_pkey PRIMARY KEY (
    workspace_id, workflow_id, workflow_version, content_hash
  ),
  CONSTRAINT weave_team_workflow_candidates_identity_check CHECK (
    workspace_id <> ''
    AND workflow_id <> ''
    AND workflow_version > 0
    AND created_by <> ''
  ),
  CONSTRAINT weave_team_workflow_candidates_hash_format_check CHECK (
    content_hash ~ '^[0-9a-f]{64}$'
  ),
  CONSTRAINT weave_team_workflow_candidates_version_fkey FOREIGN KEY (
    workspace_id, workflow_id, workflow_version
  ) REFERENCES weave_team_workflow_versions (
    workspace_id, workflow_id, version
  ) ON DELETE RESTRICT,
  CONSTRAINT weave_team_workflow_candidates_envelope_shape_check CHECK (
    jsonb_typeof(envelope_json) = 'object'
    AND jsonb_typeof(dependencies_json) = 'array'
    AND envelope_json->>'workspace_id' = workspace_id
    AND envelope_json->>'workflow_id' = workflow_id
    AND (envelope_json->>'workflow_version')::int = workflow_version
    AND envelope_json->>'content_hash' = content_hash
    AND envelope_json->>'content_hash' ~ '^[0-9a-f]{64}$'
  )
);

CREATE INDEX weave_team_workflow_candidates_hash_idx
  ON weave_team_workflow_candidates (workspace_id, workflow_id, content_hash);

CREATE FUNCTION weave_team_workflow_candidates_reject_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'team workflow candidate rows are immutable'
    USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER weave_team_workflow_candidates_immutable
BEFORE UPDATE OR DELETE ON weave_team_workflow_candidates
FOR EACH ROW
EXECUTE FUNCTION weave_team_workflow_candidates_reject_mutation();

-- Candidate test run identity: the immutable run snapshot records which
-- TeamBuildRun authorized the run and which frozen candidate it consumed.
-- Both columns are set together; drafts never carry them.
ALTER TABLE weave_team_run_snapshots
  ADD COLUMN build_run_id TEXT,
  ADD COLUMN candidate_content_hash TEXT;

ALTER TABLE weave_team_run_snapshots
  ADD CONSTRAINT weave_team_run_snapshots_candidate_identity_check CHECK (
    (build_run_id IS NULL AND candidate_content_hash IS NULL)
    OR (
      build_run_id IS NOT NULL
      AND build_run_id <> ''
      AND candidate_content_hash IS NOT NULL
      AND candidate_content_hash ~ '^[0-9a-f]{64}$'
    )
  );

-- Candidate test runs are fixed-workflow snapshots whose artifact reference
-- points at the frozen draft version (there is deliberately no published
-- artifact yet). The 0040 foreign key to weave_published_artifact_contents
-- would reject those rows, so it is replaced by an equivalent guard: the
-- published-artifact requirement is enforced for every normal run and
-- skipped only for rows carrying a candidate content hash.
ALTER TABLE weave_team_run_snapshots
  DROP CONSTRAINT weave_team_run_snapshots_artifact_workflow_fk;

CREATE FUNCTION weave_team_run_snapshots_artifact_fk_guard()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.candidate_content_hash IS NOT NULL
     OR NEW.artifact_workflow_id IS NULL THEN
    RETURN NEW;
  END IF;
  -- Only enforce the published-artifact requirement when the fixed-mode
  -- identity checks would accept the row; invalid rows must keep failing
  -- those CHECK constraints exactly as before.
  IF NEW.workflow_id IS NOT NULL
     AND NEW.workflow_id = NEW.artifact_workflow_id
     AND NEW.workflow_version IS NOT NULL
     AND NEW.workflow_version = NEW.artifact_workflow_version THEN
    IF NOT EXISTS (
      SELECT 1
      FROM weave_published_artifact_contents
      WHERE workspace_id = NEW.workspace_id
        AND workflow_id = NEW.artifact_workflow_id
        AND workflow_version = NEW.artifact_workflow_version
    ) THEN
      RAISE EXCEPTION 'fixed workflow snapshot artifact reference must be published'
        USING ERRCODE = '23503',
              CONSTRAINT = 'weave_team_run_snapshots_artifact_workflow_fk';
    END IF;
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER weave_team_run_snapshots_artifact_fk_guard
BEFORE INSERT OR UPDATE OF
  workspace_id,
  artifact_workflow_id,
  artifact_workflow_version,
  candidate_content_hash
ON weave_team_run_snapshots
FOR EACH ROW
EXECUTE FUNCTION weave_team_run_snapshots_artifact_fk_guard();

CREATE INDEX weave_team_run_snapshots_candidate_idx
  ON weave_team_run_snapshots (workspace_id, build_run_id, candidate_content_hash)
  WHERE build_run_id IS NOT NULL;
