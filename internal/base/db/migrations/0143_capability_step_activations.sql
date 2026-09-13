DROP TABLE weave_capability_step_runs;

CREATE TABLE weave_capability_step_runs (
 workspace_id TEXT NOT NULL,
 invocation_id TEXT NOT NULL,
 step_id TEXT NOT NULL,
 activation_id TEXT NOT NULL,
 run_id TEXT NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 PRIMARY KEY(workspace_id,invocation_id,activation_id,run_id),
 UNIQUE(workspace_id,invocation_id,run_id),
 FOREIGN KEY(workspace_id,invocation_id) REFERENCES weave_capability_invocations(workspace_id,invocation_id)
);

CREATE INDEX weave_capability_step_runs_step
 ON weave_capability_step_runs(workspace_id,invocation_id,step_id,created_at);
