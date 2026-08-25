CREATE TABLE weave_attachments (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES weave_workspaces(id) ON DELETE CASCADE,
  filename TEXT NOT NULL,
  size_bytes BIGINT NOT NULL CHECK (size_bytes >= 0),
  sha256 TEXT NOT NULL,
  content_type TEXT NOT NULL DEFAULT 'application/octet-stream',
  created_by TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT weave_attachments_workspace_id_id_key UNIQUE (workspace_id, id),
  CONSTRAINT weave_attachments_metadata_check CHECK (
    btrim(filename) <> ''
    AND filename = regexp_replace(filename, '^.*[/\\]', '')
    AND sha256 ~ '^[0-9a-f]{64}$'
    AND btrim(created_by) <> ''
  )
);

CREATE INDEX weave_attachments_workspace_created_idx
  ON weave_attachments (workspace_id, created_at DESC, id);

CREATE TABLE weave_project_resources (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES weave_workspaces(id) ON DELETE CASCADE,
  project_id TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('attachment','mcp_server','local_workspace')),
  resource_ref TEXT NOT NULL,
  display_name TEXT NOT NULL,
  runtime_id TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT weave_project_resources_workspace_id_id_key UNIQUE (workspace_id, id),
  CONSTRAINT weave_project_resources_project_fk
    FOREIGN KEY (workspace_id, project_id)
    REFERENCES weave_projects (workspace_id, id) ON DELETE CASCADE,
  CONSTRAINT weave_project_resources_identity_key
    UNIQUE (workspace_id, project_id, kind, resource_ref),
  CONSTRAINT weave_project_resources_shape_check CHECK (
    btrim(resource_ref) <> ''
    AND btrim(display_name) <> ''
    AND (
      (kind = 'local_workspace' AND runtime_id IS NOT NULL AND btrim(runtime_id) <> '')
      OR
      (kind <> 'local_workspace' AND runtime_id IS NULL)
    )
  )
);

CREATE INDEX weave_project_resources_project_idx
  ON weave_project_resources (workspace_id, project_id, created_at DESC, id);

CREATE FUNCTION weave_project_resources_validate_source()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.kind = 'attachment' THEN
    IF NOT EXISTS (
      SELECT 1 FROM weave_attachments
      WHERE workspace_id = NEW.workspace_id AND id = NEW.resource_ref
    ) THEN
      RAISE EXCEPTION 'project attachment is unavailable'
        USING ERRCODE = '23514', CONSTRAINT = 'weave_project_resources_attachment_check';
    END IF;
  ELSIF NEW.kind = 'mcp_server' THEN
    IF NOT EXISTS (
      SELECT 1 FROM weave_mcp_servers
      WHERE workspace_id = NEW.workspace_id AND id = NEW.resource_ref
        AND enabled = true AND revoked_at IS NULL AND deleted_at IS NULL
    ) THEN
      RAISE EXCEPTION 'project MCP server is unavailable'
        USING ERRCODE = '23514', CONSTRAINT = 'weave_project_resources_mcp_server_check';
    END IF;
  ELSIF NEW.kind = 'local_workspace' THEN
    IF NOT EXISTS (
      SELECT 1 FROM weave_runtimes
      WHERE workspace_id = NEW.workspace_id AND id = NEW.runtime_id
        AND enabled = true AND revoked_at IS NULL AND deleted_at IS NULL
    ) THEN
      RAISE EXCEPTION 'project local workspace runtime is unavailable'
        USING ERRCODE = '23514', CONSTRAINT = 'weave_project_resources_runtime_check';
    END IF;
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER weave_project_resources_source_guard
BEFORE INSERT OR UPDATE OF workspace_id, kind, resource_ref, runtime_id
ON weave_project_resources
FOR EACH ROW
EXECUTE FUNCTION weave_project_resources_validate_source();
