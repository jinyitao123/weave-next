CREATE INDEX idx_weave_team_workflow_dependencies_source
  ON weave_team_workflow_dependencies (
    workspace_id,
    dependency_type,
    dependency_key
  );

CREATE FUNCTION weave_mcp_servers_reject_referenced_delete()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  PERFORM 1
  FROM weave_team_workflow_dependencies
  WHERE workspace_id = OLD.workspace_id
    AND dependency_type = 'mcp_binding'
    AND dependency_key = OLD.id;

  IF FOUND THEN
    RAISE EXCEPTION 'MCP server is referenced by a workflow dependency'
      USING ERRCODE = '23514';
  END IF;
  RETURN OLD;
END;
$$;

CREATE TRIGGER weave_mcp_servers_delete_dependency_guard
BEFORE DELETE ON weave_mcp_servers
FOR EACH ROW
EXECUTE FUNCTION weave_mcp_servers_reject_referenced_delete();

CREATE FUNCTION weave_runtimes_reject_referenced_delete()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  PERFORM 1
  FROM weave_team_workflow_dependencies
  WHERE workspace_id = OLD.workspace_id
    AND dependency_type = 'runtime_binding'
    AND dependency_key = OLD.id;

  IF FOUND THEN
    RAISE EXCEPTION 'runtime is referenced by a workflow dependency'
      USING ERRCODE = '23514';
  END IF;
  RETURN OLD;
END;
$$;

CREATE TRIGGER weave_runtimes_delete_dependency_guard
BEFORE DELETE ON weave_runtimes
FOR EACH ROW
EXECUTE FUNCTION weave_runtimes_reject_referenced_delete();
