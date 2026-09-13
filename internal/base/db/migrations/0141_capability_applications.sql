CREATE TABLE weave_capability_apps (
 workspace_id TEXT NOT NULL REFERENCES weave_workspaces(id),
 id TEXT NOT NULL,
 name TEXT NOT NULL CHECK(length(name)>0),
 enabled BOOLEAN NOT NULL DEFAULT true,
 created_by TEXT NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 PRIMARY KEY(workspace_id,id)
);

CREATE TABLE weave_capability_credentials (
 workspace_id TEXT NOT NULL,
 app_id TEXT NOT NULL,
 id TEXT NOT NULL,
 name TEXT NOT NULL,
 key_hash TEXT NOT NULL UNIQUE,
 scopes TEXT[] NOT NULL CHECK(cardinality(scopes)>0 AND scopes <@ ARRAY['invoke','read','cancel']::text[]),
 revoked_at TIMESTAMPTZ,
 created_by TEXT NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 PRIMARY KEY(workspace_id,id),
 FOREIGN KEY(workspace_id,app_id) REFERENCES weave_capability_apps(workspace_id,id)
);

CREATE TABLE weave_capability_grants (
 workspace_id TEXT NOT NULL,
 app_id TEXT NOT NULL,
 capability_id TEXT NOT NULL,
 revision BIGINT NOT NULL,
 enabled BOOLEAN NOT NULL,
 updated_by TEXT NOT NULL,
 updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 PRIMARY KEY(workspace_id,app_id,capability_id,revision),
 FOREIGN KEY(workspace_id,app_id) REFERENCES weave_capability_apps(workspace_id,id),
 FOREIGN KEY(workspace_id,capability_id,revision) REFERENCES weave_capability_revisions(workspace_id,capability_id,revision)
);

CREATE TABLE weave_capability_access_events (
 id BIGSERIAL PRIMARY KEY,
 workspace_id TEXT NOT NULL,
 app_id TEXT NOT NULL,
 actor_id TEXT NOT NULL,
 action TEXT NOT NULL,
 resource_id TEXT NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 FOREIGN KEY(workspace_id,app_id) REFERENCES weave_capability_apps(workspace_id,id)
);

ALTER TABLE weave_capability_invocations
 ADD COLUMN credential_id TEXT,
 ADD COLUMN caller_kind TEXT NOT NULL DEFAULT 'developer' CHECK(caller_kind IN ('developer','application'));
ALTER TABLE weave_capability_invocations ADD CONSTRAINT weave_capability_invocation_credential_fk
 FOREIGN KEY(workspace_id,credential_id) REFERENCES weave_capability_credentials(workspace_id,id);
ALTER TABLE weave_capability_invocations ADD CONSTRAINT weave_capability_invocation_caller_check
 CHECK((caller_kind='developer' AND credential_id IS NULL) OR (caller_kind='application' AND credential_id IS NOT NULL AND run_kind='published'));
