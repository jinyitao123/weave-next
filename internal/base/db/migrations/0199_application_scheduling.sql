ALTER TABLE weave_capability_apps ADD COLUMN max_priority TEXT NOT NULL DEFAULT 'normal'
  CHECK (max_priority IN ('batch','normal','interactive'));
ALTER TABLE weave_capability_workflow_invocations ADD COLUMN scheduling JSONB
  CHECK (scheduling IS NULL OR jsonb_typeof(scheduling)='object');
