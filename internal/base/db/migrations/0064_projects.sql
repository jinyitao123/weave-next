CREATE TABLE weave_projects (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES weave_workspaces(id) ON DELETE CASCADE,
  avatar_id TEXT NOT NULL,
  name TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  archived_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT weave_projects_workspace_id_id_key UNIQUE (workspace_id, id),
  CONSTRAINT weave_projects_avatar_fk
    FOREIGN KEY (workspace_id, avatar_id)
    REFERENCES weave_agents (workspace_id, id) ON DELETE RESTRICT,
  CONSTRAINT weave_projects_name_check CHECK (name = btrim(name) AND name <> '')
);

CREATE UNIQUE INDEX weave_projects_active_name_key
  ON weave_projects (workspace_id, avatar_id, lower(name))
  WHERE archived_at IS NULL;

CREATE INDEX weave_projects_workspace_list_idx
  ON weave_projects (workspace_id, archived_at, updated_at DESC, id);

CREATE FUNCTION weave_projects_require_avatar()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  PERFORM 1
  FROM weave_agents
  WHERE workspace_id = NEW.workspace_id
    AND id = NEW.avatar_id
    AND role = 'avatar'
    AND deleted = false
  FOR SHARE;

  IF NOT FOUND THEN
    RAISE EXCEPTION 'project avatar must reference an active avatar'
      USING
        ERRCODE = '23514',
        CONSTRAINT = 'weave_projects_avatar_role_check';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER weave_projects_avatar_role_guard
BEFORE INSERT OR UPDATE OF workspace_id, avatar_id ON weave_projects
FOR EACH ROW
EXECUTE FUNCTION weave_projects_require_avatar();

CREATE TRIGGER weave_projects_restore_avatar_role_guard
BEFORE UPDATE OF archived_at ON weave_projects
FOR EACH ROW
WHEN (OLD.archived_at IS NOT NULL AND NEW.archived_at IS NULL)
EXECUTE FUNCTION weave_projects_require_avatar();

CREATE FUNCTION weave_agents_reject_active_project_owner_change()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF EXISTS (
    SELECT 1
    FROM weave_projects
    WHERE workspace_id = OLD.workspace_id
      AND avatar_id = OLD.id
      AND archived_at IS NULL
  ) AND (TG_OP = 'DELETE' OR NEW.deleted = true OR NEW.role <> 'avatar') THEN
    RAISE EXCEPTION 'active project owner must remain an active avatar'
      USING
        ERRCODE = '23503',
        CONSTRAINT = 'weave_agents_active_project_owner_check';
  END IF;
  RETURN CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
END;
$$;

CREATE TRIGGER weave_agents_active_project_owner_guard
BEFORE UPDATE OF role, deleted OR DELETE ON weave_agents
FOR EACH ROW
EXECUTE FUNCTION weave_agents_reject_active_project_owner_change();

CREATE TABLE weave_project_move_audits (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES weave_workspaces(id) ON DELETE CASCADE,
  project_id TEXT NOT NULL,
  from_avatar_id TEXT NOT NULL,
  to_avatar_id TEXT NOT NULL,
  operator_id TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT weave_project_move_audits_project_fk
    FOREIGN KEY (workspace_id, project_id)
    REFERENCES weave_projects (workspace_id, id) ON DELETE RESTRICT,
  CONSTRAINT weave_project_move_audits_from_avatar_fk
    FOREIGN KEY (workspace_id, from_avatar_id)
    REFERENCES weave_agents (workspace_id, id) ON DELETE RESTRICT,
  CONSTRAINT weave_project_move_audits_to_avatar_fk
    FOREIGN KEY (workspace_id, to_avatar_id)
    REFERENCES weave_agents (workspace_id, id) ON DELETE RESTRICT,
  CONSTRAINT weave_project_move_audits_distinct_avatar_check
    CHECK (from_avatar_id <> to_avatar_id),
  CONSTRAINT weave_project_move_audits_operator_check
    CHECK (btrim(operator_id) <> '')
);

CREATE INDEX weave_project_move_audits_project_idx
  ON weave_project_move_audits (workspace_id, project_id, created_at DESC, id);

CREATE FUNCTION weave_project_move_audits_reject_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'weave_project_move_audits is append-only'
    USING ERRCODE = '55000';
END;
$$;

CREATE TRIGGER weave_project_move_audits_append_only
BEFORE UPDATE OR DELETE ON weave_project_move_audits
FOR EACH ROW
EXECUTE FUNCTION weave_project_move_audits_reject_mutation();
