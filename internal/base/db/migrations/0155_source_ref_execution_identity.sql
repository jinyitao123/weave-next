-- Source identity belongs to execution, without interpreting product BuildRun data.
ALTER TABLE weave_team_run_snapshots ADD COLUMN source_ref TEXT NOT NULL DEFAULT '';
ALTER TABLE weave_task_queue ADD COLUMN source_ref TEXT NOT NULL DEFAULT '';
ALTER TABLE weave_team_run_snapshots ADD CONSTRAINT weave_snapshot_api_source_ref CHECK (
  trigger_source <> 'api' OR (source_ref<>'' AND source_ref=trigger_source_v2->>'source_ref')
);
ALTER TABLE weave_task_queue ADD CONSTRAINT weave_task_api_source_ref CHECK (
  source<>'api' OR kind<>'team_workflow' OR source_ref<>''
);
CREATE FUNCTION weave_reject_source_ref_rewrite() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.source_ref IS DISTINCT FROM NEW.source_ref THEN
    RAISE EXCEPTION 'execution source identity is immutable' USING ERRCODE='23514';
  END IF;
  RETURN NEW;
END;
$$;
CREATE TRIGGER weave_task_source_ref_immutable BEFORE UPDATE OF source_ref ON weave_task_queue
  FOR EACH ROW EXECUTE FUNCTION weave_reject_source_ref_rewrite();
CREATE TRIGGER weave_snapshot_source_ref_immutable BEFORE UPDATE OF source_ref ON weave_team_run_snapshots
  FOR EACH ROW EXECUTE FUNCTION weave_reject_source_ref_rewrite();

-- Product build/round provenance is retained only by Server candidate requests.
ALTER TABLE weave_team_run_snapshots DROP COLUMN build_round_no;
ALTER TABLE weave_team_run_snapshots DROP COLUMN build_run_id;
ALTER TABLE weave_team_run_snapshots ADD CONSTRAINT weave_snapshot_candidate_hash
 CHECK(candidate_content_hash IS NULL OR candidate_content_hash ~ '^[0-9a-f]{64}$');
