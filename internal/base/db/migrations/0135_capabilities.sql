CREATE TABLE IF NOT EXISTS weave_capability_definitions (
    workspace_id TEXT NOT NULL,
    capability_id TEXT NOT NULL,
    definition JSONB NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, capability_id)
);

CREATE TABLE IF NOT EXISTS weave_capability_revisions (
    workspace_id TEXT NOT NULL,
    capability_id TEXT NOT NULL,
    revision BIGINT NOT NULL CHECK (revision > 0),
    definition_hash TEXT NOT NULL,
    definition JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, capability_id, revision)
);

CREATE TABLE IF NOT EXISTS weave_capability_invocations (
    workspace_id TEXT NOT NULL,
    application_id TEXT NOT NULL,
    request_id TEXT NOT NULL,
    invocation_id TEXT NOT NULL,
    capability_id TEXT NOT NULL,
    revision BIGINT NOT NULL CHECK (revision > 0),
    input JSONB NOT NULL,
    status TEXT NOT NULL,
    result_state TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, application_id, request_id),
    UNIQUE (workspace_id, invocation_id)
);

CREATE INDEX IF NOT EXISTS weave_capability_invocations_lookup
    ON weave_capability_invocations (workspace_id, invocation_id);
