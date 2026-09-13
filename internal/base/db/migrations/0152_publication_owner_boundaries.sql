-- Fresh-system owner boundary. Frozen facts no longer depend on mutable
-- product drafts/catalogs. There is no old-data conversion or dual writer.
ALTER TABLE weave_published_artifact_contents
  DROP CONSTRAINT weave_published_artifact_contents_version_fk;
ALTER TABLE weave_team_workflow_dependencies
  DROP CONSTRAINT weave_team_workflow_dependencies_version_fk;
ALTER TABLE weave_workflow_version_admission_statuses
  DROP CONSTRAINT weave_workflow_version_admission_statuses_version_fk;
ALTER TABLE weave_team_workflow_candidates
  DROP CONSTRAINT weave_team_workflow_candidates_version_fkey;

-- The revision envelope is the authority for frozen dependency/admission rows.
-- Deferred checks allow all three sets of facts to be written atomically.
-- Draft dependency editing belongs to the product catalog, in a separate table.
CREATE TABLE weave_draft_workflow_dependencies
  (LIKE weave_team_workflow_dependencies INCLUDING DEFAULTS INCLUDING GENERATED INCLUDING CONSTRAINTS INCLUDING INDEXES);
ALTER TABLE weave_draft_workflow_dependencies ADD CONSTRAINT weave_draft_dependency_version_fk
  FOREIGN KEY(workspace_id,workflow_id,workflow_version)
  REFERENCES weave_team_workflow_versions(workspace_id,workflow_id,version) ON DELETE CASCADE;
CREATE FUNCTION weave_draft_dependency_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE draft_status TEXT;
BEGIN
  IF TG_OP='DELETE' THEN
    SELECT status INTO draft_status FROM weave_team_workflow_versions
      WHERE workspace_id=OLD.workspace_id AND workflow_id=OLD.workflow_id AND version=OLD.workflow_version;
    IF draft_status IS NULL OR draft_status='draft' THEN RETURN OLD; END IF;
  ELSE
    SELECT status INTO draft_status FROM weave_team_workflow_versions
      WHERE workspace_id=NEW.workspace_id AND workflow_id=NEW.workflow_id AND version=NEW.workflow_version;
    IF draft_status='draft' THEN RETURN NEW; END IF;
  END IF;
  RAISE EXCEPTION 'draft dependencies require a mutable draft' USING ERRCODE='23514';
END; $$;
CREATE TRIGGER weave_draft_dependency_guard BEFORE INSERT OR UPDATE OR DELETE ON weave_draft_workflow_dependencies
  FOR EACH ROW EXECUTE FUNCTION weave_draft_dependency_guard();

ALTER TABLE weave_team_workflow_dependencies ADD CONSTRAINT weave_dependencies_frozen_revision_fk
  FOREIGN KEY (workspace_id,workflow_id,workflow_version)
  REFERENCES weave_published_artifact_contents(workspace_id,workflow_id,workflow_version)
  DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE weave_workflow_version_admission_statuses ADD CONSTRAINT weave_admission_frozen_revision_fk
  FOREIGN KEY (workspace_id,workflow_id,workflow_version)
  REFERENCES weave_published_artifact_contents(workspace_id,workflow_id,workflow_version)
  DEFERRABLE INITIALLY DEFERRED;

CREATE OR REPLACE FUNCTION weave_published_artifact_contents_guard()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP='INSERT' THEN RETURN NEW; END IF;
  RAISE EXCEPTION 'published artifact content is immutable' USING ERRCODE='23514';
END; $$;

CREATE OR REPLACE FUNCTION weave_team_workflow_dependencies_guard_publication()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE revision_xid BIGINT;
BEGIN
  IF TG_OP<>'INSERT' THEN
    RAISE EXCEPTION 'frozen workflow dependencies are immutable' USING ERRCODE='23514';
  END IF;
  SELECT xmin::text::bigint INTO revision_xid FROM weave_published_artifact_contents
    WHERE workspace_id=NEW.workspace_id AND workflow_id=NEW.workflow_id
      AND workflow_version=NEW.workflow_version;
  IF FOUND AND revision_xid <> (pg_current_xact_id()::text::bigint % 4294967296) THEN
    RAISE EXCEPTION 'dependencies must be frozen in the revision transaction' USING ERRCODE='23514';
  END IF;
  RETURN NEW;
END; $$;

CREATE OR REPLACE FUNCTION weave_workflow_version_admission_statuses_guard()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP='INSERT' THEN RETURN NEW; END IF;
  IF TG_OP='DELETE' THEN
    RAISE EXCEPTION 'workflow admission status is historical' USING ERRCODE='23514';
  END IF;
  IF (OLD.workspace_id,OLD.workflow_id,OLD.workflow_version)
    IS DISTINCT FROM (NEW.workspace_id,NEW.workflow_id,NEW.workflow_version) THEN
    RAISE EXCEPTION 'workflow admission identity is immutable' USING ERRCODE='23514';
  END IF;
  RETURN NEW;
END; $$;

-- These kernel records reference product identities as immutable scope facts,
-- not as cascading ownership. Admission services validate the trusted subject.
DO $$ DECLARE edge RECORD;
BEGIN
  FOR edge IN SELECT conrelid::regclass AS source_table, conname FROM pg_constraint
    WHERE contype='f'
      AND conrelid=ANY(ARRAY['weave_task_queue'::regclass,'weave_team_run_snapshots'::regclass])
      AND confrelid=ANY(ARRAY['weave_workspaces'::regclass,'weave_teams'::regclass,
        'weave_agents'::regclass,'weave_agent_versions'::regclass,'weave_projects'::regclass,
        'weave_team_build_runs'::regclass,'weave_team_workflows'::regclass,'weave_team_workflow_versions'::regclass])
  LOOP
    EXECUTE format('ALTER TABLE %s DROP CONSTRAINT %I',edge.source_table,edge.conname);
  END LOOP;
END; $$;

ALTER TABLE weave_team_run_snapshots DROP CONSTRAINT weave_team_run_snapshots_candidate_identity_check;
ALTER TABLE weave_team_run_snapshots ADD CONSTRAINT weave_team_run_snapshots_candidate_identity_check CHECK (
  (build_run_id IS NULL OR candidate_content_hash IS NOT NULL)
  AND (candidate_content_hash IS NULL OR candidate_content_hash ~ '^[0-9a-f]{64}$')
);
ALTER TABLE weave_team_run_snapshots ADD CONSTRAINT weave_snapshot_frozen_candidate_fk
  FOREIGN KEY(workspace_id,artifact_workflow_id,artifact_workflow_version,candidate_content_hash)
  REFERENCES weave_team_workflow_candidates(workspace_id,workflow_id,workflow_version,content_hash);

-- Evaluation provenance is a server-owned logical reference. Builder strategy
-- extraction must not require a reverse foreign key from product assets.
ALTER TABLE weave_teams DROP CONSTRAINT weave_teams_evaluation_build_run_fkey;

-- Kernel-owned idempotency receipts. The receipt commits in the same
-- transaction as the immutable revision or the admitted snapshot/platform task.
CREATE TABLE weave_kernel_publication_requests (
  workspace_id TEXT NOT NULL,
  request_id TEXT NOT NULL,
  operation TEXT NOT NULL CHECK(operation IN ('publish','candidate_run')),
  actor_subject JSONB NOT NULL,
  request_digest TEXT NOT NULL CHECK(request_digest ~ '^[0-9a-f]{64}$'),
  receipt JSONB NOT NULL CHECK(jsonb_typeof(receipt)='object'),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY(workspace_id,request_id),
  CHECK(COALESCE(actor_subject->>'workspace_id','')=workspace_id AND workspace_id<>''
    AND ((COALESCE(actor_subject->>'user_id','')<>'' AND NOT(actor_subject ? 'service_id'))
      OR (COALESCE(actor_subject->>'service_id','')<>'' AND NOT(actor_subject ? 'user_id'))))
);
CREATE TRIGGER weave_kernel_publication_requests_immutable
  BEFORE UPDATE OR DELETE ON weave_kernel_publication_requests
  FOR EACH ROW EXECUTE FUNCTION weave_team_workflow_candidates_reject_mutation();

-- Server-owned durable delivery intent. These rows do not carry execution
-- status, claim ownership, timeout policy or a second retry worker.
CREATE TABLE weave_team_publication_requests (
  workspace_id TEXT NOT NULL, request_id TEXT NOT NULL,
  actor_subject JSONB NOT NULL, request_digest TEXT NOT NULL,
  command JSONB NOT NULL, state TEXT NOT NULL CHECK(state IN ('pending','revision_obtained','activated')),
  receipt JSONB, created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY(workspace_id,request_id),
  CHECK(request_digest ~ '^[0-9a-f]{64}$' AND jsonb_typeof(command)='object'),
  CHECK(jsonb_typeof(actor_subject)='object' AND COALESCE(actor_subject->>'workspace_id','')=workspace_id AND workspace_id<>''
    AND ((COALESCE(actor_subject->>'user_id','')<>'' AND NOT(actor_subject ? 'service_id'))
      OR (COALESCE(actor_subject->>'service_id','')<>'' AND NOT(actor_subject ? 'user_id')))),
  CHECK((state='pending' AND receipt IS NULL) OR (state<>'pending' AND receipt IS NOT NULL AND jsonb_typeof(receipt)='object'))
);
CREATE TABLE weave_team_candidate_requests (
  workspace_id TEXT NOT NULL, request_id TEXT NOT NULL,
  actor_subject JSONB NOT NULL, request_digest TEXT NOT NULL,
  target JSONB NOT NULL, request JSONB NOT NULL, receipt JSONB,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY(workspace_id,request_id),
  CHECK(request_digest ~ '^[0-9a-f]{64}$' AND jsonb_typeof(target)='object' AND jsonb_typeof(request)='object'),
  CHECK(jsonb_typeof(actor_subject)='object' AND COALESCE(actor_subject->>'workspace_id','')=workspace_id AND workspace_id<>''
    AND ((COALESCE(actor_subject->>'user_id','')<>'' AND NOT(actor_subject ? 'service_id'))
      OR (COALESCE(actor_subject->>'service_id','')<>'' AND NOT(actor_subject ? 'user_id')))),
  CHECK(receipt IS NULL OR jsonb_typeof(receipt)='object')
);

CREATE FUNCTION weave_product_publication_request_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP='DELETE' OR (OLD.workspace_id,OLD.request_id,OLD.actor_subject,OLD.request_digest)
    IS DISTINCT FROM (NEW.workspace_id,NEW.request_id,NEW.actor_subject,NEW.request_digest) THEN
    RAISE EXCEPTION 'publication request identity is immutable' USING ERRCODE='23514';
  END IF;
  IF OLD.receipt IS NOT NULL AND OLD.receipt IS DISTINCT FROM NEW.receipt THEN
    RAISE EXCEPTION 'publication receipt is immutable' USING ERRCODE='23514';
  END IF;
  IF TG_TABLE_NAME='weave_team_publication_requests' THEN
    IF OLD.command IS DISTINCT FROM NEW.command OR
      (OLD.state='revision_obtained' AND NEW.state NOT IN ('revision_obtained','activated')) OR
      (OLD.state='activated' AND NEW.state<>'activated') OR
      (OLD.state='pending' AND NEW.state='activated') THEN
      RAISE EXCEPTION 'publication request transition is invalid' USING ERRCODE='23514';
    END IF;
  ELSE
    IF OLD.target IS DISTINCT FROM NEW.target OR OLD.request IS DISTINCT FROM NEW.request THEN
      RAISE EXCEPTION 'candidate request is immutable' USING ERRCODE='23514';
    END IF;
  END IF;
  RETURN NEW;
END; $$;
CREATE TRIGGER weave_team_publication_requests_guard BEFORE UPDATE OR DELETE ON weave_team_publication_requests
  FOR EACH ROW EXECUTE FUNCTION weave_product_publication_request_guard();
CREATE TRIGGER weave_team_candidate_requests_guard BEFORE UPDATE OR DELETE ON weave_team_candidate_requests
  FOR EACH ROW EXECUTE FUNCTION weave_product_publication_request_guard();
