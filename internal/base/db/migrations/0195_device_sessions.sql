-- A device control session is one task pinned to the person's device runtime
-- (weave-workbench decision 009). It carries no agent, workflow, build or
-- capability identity; the person who opened it is the task's actor.
ALTER TABLE weave_task_queue
 DROP CONSTRAINT weave_task_queue_identity_kind_check,
 DROP CONSTRAINT weave_task_queue_identity_shape_check;

ALTER TABLE weave_task_queue
 ADD CONSTRAINT weave_task_queue_identity_kind_check
 CHECK (identity_kind IN ('agent','team_workflow','audit','team_build','capability','device')),
 ADD CONSTRAINT weave_task_queue_identity_shape_check CHECK (
 (build_run_id IS NULL AND capability_invocation_id IS NULL AND (
    (identity_schema_version=1 AND identity_kind='agent' AND agent IS NOT NULL
      AND execution_scope IS NULL AND workflow_id IS NULL AND workflow_version IS NULL AND run_snapshot_id IS NULL)
    OR (identity_schema_version=2 AND (
      (identity_kind='agent' AND agent IS NOT NULL AND agent<>'' AND agent_id IS NOT NULL AND agent_version IS NOT NULL
       AND execution_scope IS NOT NULL AND workflow_id IS NULL AND workflow_version IS NULL
       AND ((execution_scope IN ('team_free_collab','team_worker_leaf') AND run_snapshot_id IS NOT NULL AND run_snapshot_id<>'')
         OR (execution_scope NOT IN ('team_free_collab','team_worker_leaf') AND run_snapshot_id IS NULL)))
      OR (identity_kind='team_workflow' AND agent IS NULL AND agent_id IS NULL AND agent_version IS NULL
       AND execution_scope IS NULL AND workflow_id IS NOT NULL AND workflow_id<>'' AND workflow_version IS NOT NULL
       AND workflow_version>0 AND run_snapshot_id IS NOT NULL AND run_snapshot_id<>'')
      OR (identity_kind='audit' AND agent IS NULL AND agent_id IS NULL AND agent_version IS NULL
       AND execution_scope IS NULL AND workflow_id IS NULL AND workflow_version IS NULL AND run_snapshot_id IS NULL
       AND status IN ('completed','failed','cancelled','superseded','cut','timed_out'))
      OR (identity_kind='device' AND kind='device_session' AND source='device'
       AND runtime_id IS NOT NULL AND runtime_id<>''
       AND agent IS NULL AND agent_id IS NULL AND agent_version IS NULL
       AND execution_scope IS NULL AND workflow_id IS NULL AND workflow_version IS NULL AND run_snapshot_id IS NULL)
    )))
 ) OR (
   identity_kind='team_build' AND identity_schema_version=2 AND kind='team_build' AND source='team_build'
   AND build_run_id IS NOT NULL AND build_run_id<>'' AND capability_invocation_id IS NULL
   AND agent IS NULL AND agent_id IS NULL AND agent_version IS NULL AND execution_scope IS NULL
   AND workflow_id IS NULL AND workflow_version IS NULL AND run_snapshot_id IS NULL AND runtime_id IS NULL
 ) OR (
   identity_kind='capability' AND identity_schema_version=2
   AND kind='capability_invocation' AND source='capability'
   AND capability_invocation_id IS NOT NULL AND capability_invocation_id<>'' AND build_run_id IS NULL
   AND agent IS NULL AND agent_id IS NULL AND agent_version IS NULL AND execution_scope IS NULL
   AND workflow_id IS NULL AND workflow_version IS NULL AND run_snapshot_id IS NULL
 ));

-- What a device last reported about itself: platform, driver, system
-- permissions and the levels its owner allows locally.
ALTER TABLE weave_runtimes ADD COLUMN IF NOT EXISTS device_facts JSONB NOT NULL DEFAULT '{}';

-- One operation of a session. params and result exist only until they are
-- delivered (params to the device, result to the caller) and are then cleared;
-- what stays is the audit record: which operation, on what, when and how it
-- ended, never what was typed, shown or read.
CREATE TABLE IF NOT EXISTS weave_device_operations (
  id             UUID        PRIMARY KEY,
  workspace_id   TEXT        NOT NULL,
  session_id     TEXT        NOT NULL,
  runtime_id     TEXT        NOT NULL,
  user_id        TEXT        NOT NULL,
  op_id          TEXT        NOT NULL,
  kind           TEXT        NOT NULL,
  request_digest TEXT        NOT NULL,
  audit          JSONB       NOT NULL DEFAULT '{}',
  params         JSONB,
  result         JSONB,
  status         TEXT        NOT NULL DEFAULT 'queued'
                 CHECK (status IN ('queued','delivered','succeeded','failed','unknown','stopped')),
  error          TEXT        NOT NULL DEFAULT '',
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  delivered_at   TIMESTAMPTZ,
  completed_at   TIMESTAMPTZ,
  UNIQUE (session_id, op_id)
);
CREATE INDEX IF NOT EXISTS idx_device_operations_pending
  ON weave_device_operations (session_id, created_at) WHERE status='queued';

-- Why a session ended, written once by whoever ended it first. The session's
-- state stays the task's; this is only the audit of the reason.
CREATE TABLE IF NOT EXISTS weave_device_session_ends (
  session_id   TEXT        PRIMARY KEY,
  workspace_id TEXT        NOT NULL,
  reason       TEXT        NOT NULL,
  ended_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
