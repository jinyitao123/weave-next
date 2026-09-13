CREATE TABLE weave_capability_revision_tool_bindings (
    workspace_id TEXT NOT NULL,
    capability_id TEXT NOT NULL,
    revision BIGINT NOT NULL,
    bindings JSONB NOT NULL CHECK (jsonb_typeof(bindings) = 'array'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (workspace_id, capability_id, revision),
    FOREIGN KEY (workspace_id, capability_id, revision)
        REFERENCES weave_capability_revisions(workspace_id, capability_id, revision)
        ON DELETE CASCADE
);

CREATE OR REPLACE FUNCTION weave_reject_capability_tool_binding_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'published capability tool bindings are immutable';
END;
$$;

CREATE TRIGGER weave_capability_tool_bindings_immutable_update
BEFORE UPDATE ON weave_capability_revision_tool_bindings
FOR EACH ROW EXECUTE FUNCTION weave_reject_capability_tool_binding_mutation();

CREATE TRIGGER weave_capability_tool_bindings_immutable_delete
BEFORE DELETE ON weave_capability_revision_tool_bindings
FOR EACH ROW EXECUTE FUNCTION weave_reject_capability_tool_binding_mutation();
