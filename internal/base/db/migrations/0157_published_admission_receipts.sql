-- Kernel owns atomic published-run acceptance, including ambiguous retries.
ALTER TABLE weave_kernel_publication_requests DROP CONSTRAINT weave_kernel_publication_requests_operation_check;
ALTER TABLE weave_kernel_publication_requests ADD CONSTRAINT weave_kernel_publication_requests_operation_check
  CHECK(operation IN ('publish','candidate_run','published_run','published_closed'));

-- Server owns delivery intent and product associations, never execution status.
CREATE TABLE weave_workflow_admission_requests (
  workspace_id TEXT NOT NULL,
  request_id TEXT NOT NULL,
  actor_subject JSONB NOT NULL,
  request_digest TEXT NOT NULL CHECK(request_digest ~ '^[0-9a-f]{64}$'),
  request JSONB NOT NULL CHECK(jsonb_typeof(request)='object'),
  target JSONB NOT NULL CHECK(jsonb_typeof(target)='object'),
  receipt JSONB,
  associated BOOLEAN NOT NULL DEFAULT false,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY(workspace_id,request_id),
  CHECK(workspace_id<>'' AND request_id<>''),
  CHECK(jsonb_typeof(actor_subject)='object' AND COALESCE(actor_subject->>'workspace_id','')=workspace_id
    AND ((COALESCE(actor_subject->>'user_id','')<>'' AND NOT(actor_subject ? 'service_id'))
      OR (COALESCE(actor_subject->>'service_id','')<>'' AND NOT(actor_subject ? 'user_id')))),
  CHECK(receipt IS NULL OR jsonb_typeof(receipt)='object'),
  CHECK(NOT associated OR receipt IS NOT NULL)
);
CREATE FUNCTION weave_workflow_admission_request_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP='DELETE' OR (OLD.workspace_id,OLD.request_id,OLD.actor_subject,OLD.request_digest,OLD.request,OLD.target)
    IS DISTINCT FROM (NEW.workspace_id,NEW.request_id,NEW.actor_subject,NEW.request_digest,NEW.request,NEW.target)
    OR (OLD.receipt IS NOT NULL AND OLD.receipt IS DISTINCT FROM NEW.receipt)
    OR (OLD.associated AND NOT NEW.associated) THEN
    RAISE EXCEPTION 'workflow admission request is immutable' USING ERRCODE='23514';
  END IF;
  RETURN NEW;
END; $$;
CREATE TRIGGER weave_workflow_admission_request_guard BEFORE UPDATE OR DELETE ON weave_workflow_admission_requests
  FOR EACH ROW EXECUTE FUNCTION weave_workflow_admission_request_guard();

-- A product occurrence is prepared before the kernel is contacted. Its only
-- execution evidence is the exact verified receipt, never a cross-domain FK.
ALTER TABLE weave_schedule_occurrences DROP CONSTRAINT weave_schedule_occurrences_snapshot_fk;
ALTER TABLE weave_schedule_occurrences DROP CONSTRAINT weave_schedule_occurrences_task_fk;
CREATE OR REPLACE FUNCTION weave_schedule_occurrences_guard()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
  expected_occurrence_key TEXT;
BEGIN
  IF TG_OP <> 'DELETE' THEN
    expected_occurrence_key := encode(
      sha256(
        convert_to(NEW.workspace_id, 'UTF8')
        || decode('00', 'hex')
        || convert_to(NEW.schedule_id, 'UTF8')
        || decode('00', 'hex')
        || convert_to(
          to_char(
            NEW.scheduled_for AT TIME ZONE 'UTC',
            'YYYY-MM-DD"T"HH24:MI:SS.US'
          ) || '000Z',
          'UTF8'
        )
      ),
      'hex'
    );
    IF NEW.occurrence_key <> expected_occurrence_key THEN
      RAISE EXCEPTION 'occurrence_key does not match canonical schedule occurrence'
        USING ERRCODE = '23514';
    END IF;
  END IF;
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'pending'
       OR NEW.workflow_version IS NOT NULL
       OR NEW.run_snapshot_id IS NOT NULL
       OR NEW.task_id IS NOT NULL THEN
      RAISE EXCEPTION 'schedule occurrences must be inserted pending'
        USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
  END IF;
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'schedule occurrences cannot be deleted'
      USING ERRCODE = '23514';
  END IF;
  IF OLD.status = 'committed' THEN
    RAISE EXCEPTION 'committed schedule occurrences are immutable'
      USING ERRCODE = '23514';
  END IF;
  IF NEW.status <> 'committed'
     OR OLD.workspace_id IS DISTINCT FROM NEW.workspace_id
     OR OLD.occurrence_key IS DISTINCT FROM NEW.occurrence_key
     OR OLD.schedule_id IS DISTINCT FROM NEW.schedule_id
     OR OLD.target_workflow_id IS DISTINCT FROM NEW.target_workflow_id
     OR OLD.scheduled_for IS DISTINCT FROM NEW.scheduled_for
     OR OLD.created_at IS DISTINCT FROM NEW.created_at THEN
    RAISE EXCEPTION 'schedule occurrence may only transition pending to committed'
      USING ERRCODE = '23514';
  END IF;
  IF NOT EXISTS (SELECT 1 FROM weave_workflow_admission_requests r
    WHERE r.workspace_id=NEW.workspace_id AND r.request_id='schedule:'||NEW.occurrence_key
      AND r.target->>'schedule_id'=NEW.schedule_id AND r.target->>'occurrence_key'=NEW.occurrence_key
      AND r.request->'revision'->>'workflow_id'=NEW.target_workflow_id
      AND (r.request->'revision'->>'workflow_version')::int=NEW.workflow_version
      AND r.request->>'run_id'=NEW.run_snapshot_id AND r.request->>'task_id'=NEW.task_id) THEN
    RAISE EXCEPTION 'occurrence differs from immutable admission intent' USING ERRCODE='23514';
  END IF;
  RETURN NEW;
END; $$;

CREATE OR REPLACE FUNCTION weave_schedule_occurrences_require_committed()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM weave_workflow_admission_requests r
    JOIN weave_schedule_occurrences o ON o.workspace_id=r.workspace_id AND r.request_id='schedule:'||o.occurrence_key
    WHERE o.workspace_id=NEW.workspace_id AND o.occurrence_key=NEW.occurrence_key
      AND r.target->>'schedule_id'=o.schedule_id AND r.target->>'occurrence_key'=o.occurrence_key
      AND r.request->'revision'->>'workflow_id'=o.target_workflow_id
      AND (o.status='pending' OR (r.associated AND r.receipt->>'task_id'=o.task_id
        AND r.receipt->>'run_snapshot_id'=o.run_snapshot_id
        AND (r.receipt->'revision'->>'workflow_version')::int=o.workflow_version))) THEN
    RAISE EXCEPTION 'schedule occurrence requires durable intent and verified receipt' USING ERRCODE='23514';
  END IF;
  RETURN NULL;
END; $$;

-- Chat stores only verified receipt references after product association.
ALTER TABLE weave_chat_requests DROP CONSTRAINT weave_chat_requests_task_fk;
