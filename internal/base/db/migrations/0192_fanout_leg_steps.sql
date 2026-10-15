-- The steps of one parallel branch that have finished. A branch runs its
-- dispatch workers in turn inside one leg task; when the task runs again after
-- a failure or a retry, a step recorded here is not run a second time, so its
-- tool calls and business actions are never repeated. Only the step's
-- workflow-visible output and the tasks holding its artifacts are kept.
CREATE TABLE IF NOT EXISTS weave_fanout_leg_steps (
  workspace_id      TEXT        NOT NULL,
  group_id          TEXT        NOT NULL,
  leg_id            TEXT        NOT NULL,
  node_id           TEXT        NOT NULL,
  step_ordinal      INTEGER     NOT NULL CHECK (step_ordinal >= 0),
  output            JSONB       NOT NULL,
  artifact_task_ids JSONB       NOT NULL CHECK (jsonb_typeof(artifact_task_ids) = 'array'),
  completed_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, group_id, leg_id, node_id),
  UNIQUE (workspace_id, group_id, leg_id, step_ordinal)
);
