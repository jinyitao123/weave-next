-- New execution records require authenticated actor identity. Deployment uses
-- a freshly created database; no shared fallback is invented for old rows.
ALTER TABLE weave_task_queue ADD COLUMN actor_subject JSONB NOT NULL;
ALTER TABLE weave_team_run_snapshots ADD COLUMN actor_subject JSONB NOT NULL;

ALTER TABLE weave_task_queue ADD CONSTRAINT weave_task_queue_actor_subject_check CHECK (
 jsonb_typeof(actor_subject)='object' AND actor_subject->>'workspace_id'=workspace_id
 AND ((COALESCE(actor_subject->>'user_id','')<>'' AND NOT (actor_subject ? 'service_id'))
   OR (COALESCE(actor_subject->>'service_id','')<>'' AND NOT (actor_subject ? 'user_id')))
);
ALTER TABLE weave_team_run_snapshots ADD CONSTRAINT weave_team_run_snapshots_actor_subject_check CHECK (
 jsonb_typeof(actor_subject)='object' AND actor_subject->>'workspace_id'=workspace_id
 AND ((COALESCE(actor_subject->>'user_id','')<>'' AND NOT (actor_subject ? 'service_id'))
   OR (COALESCE(actor_subject->>'service_id','')<>'' AND NOT (actor_subject ? 'user_id')))
);
CREATE FUNCTION weave_reject_execution_subject_rewrite() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 RAISE EXCEPTION 'execution actor subject is immutable' USING ERRCODE='23514';
END; $$;
CREATE TRIGGER weave_task_queue_actor_immutable BEFORE UPDATE OF actor_subject ON weave_task_queue
 FOR EACH ROW WHEN (OLD.actor_subject IS DISTINCT FROM NEW.actor_subject)
 EXECUTE FUNCTION weave_reject_execution_subject_rewrite();
CREATE TRIGGER weave_team_run_snapshots_actor_immutable BEFORE UPDATE OF actor_subject ON weave_team_run_snapshots
 FOR EACH ROW WHEN (OLD.actor_subject IS DISTINCT FROM NEW.actor_subject)
 EXECUTE FUNCTION weave_reject_execution_subject_rewrite();
CREATE INDEX weave_task_queue_actor_idx ON weave_task_queue(workspace_id, (actor_subject->>'user_id'),created_at DESC);

-- Physical invocation facts remain owned by the platform queue across resumes.
ALTER TABLE weave_task_queue ADD COLUMN deadline_at timestamptz,
 ADD COLUMN outcome_sensitive boolean NOT NULL DEFAULT false,
 ADD COLUMN physical_usage jsonb NOT NULL DEFAULT '{"input_tokens":0,"output_tokens":0,"cost_usd":0,"tool_calls":0}',
 ADD COLUMN usage_epoch bigint NOT NULL DEFAULT 0,
 ADD COLUMN unreported_attempts integer NOT NULL DEFAULT 0 CHECK (unreported_attempts>=0),
 ADD COLUMN stopped_epoch bigint NOT NULL DEFAULT 0,
 ADD COLUMN stopped_worker_id text;
CREATE TRIGGER weave_task_queue_execution_admission_immutable BEFORE UPDATE OF deadline_at,outcome_sensitive ON weave_task_queue
 FOR EACH ROW WHEN (OLD.deadline_at IS DISTINCT FROM NEW.deadline_at OR OLD.outcome_sensitive IS DISTINCT FROM NEW.outcome_sensitive)
 EXECUTE FUNCTION weave_reject_execution_subject_rewrite();
