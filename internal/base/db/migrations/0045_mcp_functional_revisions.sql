LOCK TABLE weave_mcp_servers, weave_mcp_tools
  IN ACCESS EXCLUSIVE MODE;

ALTER TABLE weave_mcp_servers
  ADD COLUMN functional_revision BIGINT NOT NULL DEFAULT 1,
  ADD COLUMN revoked_at TIMESTAMPTZ,
  ADD CONSTRAINT weave_mcp_servers_functional_revision_check
    CHECK (functional_revision BETWEEN 1 AND 9007199254740991),
  ADD CONSTRAINT weave_mcp_servers_workspace_id_id_key
    UNIQUE (workspace_id, id);

CREATE FUNCTION weave_mcp_servers_enforce_functional_revision()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
  functional_changed BOOLEAN;
BEGIN
  functional_changed := ROW(NEW.transport, NEW.url, NEW.command, NEW.args)
    IS DISTINCT FROM ROW(OLD.transport, OLD.url, OLD.command, OLD.args);

  IF functional_changed THEN
    IF OLD.functional_revision >= 9007199254740991
       OR NEW.functional_revision IS DISTINCT FROM OLD.functional_revision + 1 THEN
      RAISE EXCEPTION 'MCP functional changes require exactly one revision advance'
        USING ERRCODE = '23514';
    END IF;
  ELSIF NEW.functional_revision IS DISTINCT FROM OLD.functional_revision THEN
    RAISE EXCEPTION 'MCP functional revision cannot change without functional changes'
      USING ERRCODE = '23514';
  END IF;

  RETURN NEW;
END;
$$;

CREATE TRIGGER weave_mcp_servers_functional_revision_guard
BEFORE UPDATE ON weave_mcp_servers
FOR EACH ROW
EXECUTE FUNCTION weave_mcp_servers_enforce_functional_revision();

ALTER TABLE weave_mcp_tools
  ADD COLUMN workspace_id TEXT;

UPDATE weave_mcp_tools child
SET workspace_id = parent.workspace_id
FROM weave_mcp_servers parent
WHERE parent.id = child.server_id;

ALTER TABLE weave_mcp_tools
  ALTER COLUMN workspace_id SET NOT NULL,
  DROP CONSTRAINT weave_mcp_tools_server_id_fkey,
  DROP CONSTRAINT weave_mcp_tools_pkey,
  ADD CONSTRAINT weave_mcp_tools_pkey
    PRIMARY KEY (workspace_id, server_id, name),
  ADD CONSTRAINT weave_mcp_tools_server_fk
    FOREIGN KEY (workspace_id, server_id)
    REFERENCES weave_mcp_servers (workspace_id, id) ON DELETE CASCADE;
