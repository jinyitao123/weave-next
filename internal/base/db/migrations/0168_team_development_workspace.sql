-- Product authoring state only. Execution remains owned by Kernel/taskqueue.
CREATE TABLE weave_team_development_drafts (
 workspace_id TEXT NOT NULL, team_id TEXT NOT NULL,
 revision BIGINT NOT NULL DEFAULT 1, published_revision BIGINT NOT NULL DEFAULT 0,
 publishing_revision BIGINT NOT NULL DEFAULT 0,
 document JSONB NOT NULL, published_document JSONB NOT NULL, baseline JSONB NOT NULL,
 prepared_actor TEXT NOT NULL DEFAULT '',
 prepared_revision BIGINT NOT NULL DEFAULT 0, prepared JSONB NOT NULL DEFAULT '[]',
 updated_by TEXT NOT NULL, updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 PRIMARY KEY(workspace_id,team_id),
 FOREIGN KEY(workspace_id,team_id) REFERENCES weave_teams(workspace_id,id)
);
CREATE TABLE weave_team_development_candidates (
 workspace_id TEXT NOT NULL, workflow_id TEXT NOT NULL, workflow_version INT NOT NULL,
 team_id TEXT NOT NULL, revision BIGINT NOT NULL, team_read JSONB NOT NULL,
 PRIMARY KEY(workspace_id,workflow_id,workflow_version),
 FOREIGN KEY(workspace_id,workflow_id,workflow_version) REFERENCES weave_team_workflow_versions(workspace_id,workflow_id,version)
);
CREATE TABLE weave_team_development_trials (
 workspace_id TEXT NOT NULL, team_id TEXT NOT NULL, request_id UUID NOT NULL,
 revision BIGINT NOT NULL, workflow_id TEXT NOT NULL, actor_id TEXT NOT NULL,
 request_digest TEXT NOT NULL, request JSONB NOT NULL, receipt JSONB,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 PRIMARY KEY(workspace_id,request_id)
);
