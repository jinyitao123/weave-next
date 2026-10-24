-- Authorization versions (#93). A model call of a run attempt checks its
-- credential in full once and then only reads these versions by key: the
-- full check runs again when one of them moved. Each change below that can
-- withdraw an authorization moves its scope's version in the same
-- transaction, so a reader either sees the change and the new version or
-- neither. A scope without a row has never changed and reads as version 0.
--
-- Scopes:
--   capability_app       application enabled, its grants, its credentials
--   team                 team status and lead, its members, a staged roster
--   provider_credential  provider enabled, revoked, deleted, its models
--
-- Exact member versions (weave_agent_versions) cannot change or be deleted,
-- so they have no scope.
CREATE TABLE weave_authorization_epochs (
  workspace_id TEXT NOT NULL,
  scope_kind TEXT NOT NULL CHECK (scope_kind IN ('capability_app','team','provider_credential')),
  scope_id TEXT NOT NULL,
  epoch BIGINT NOT NULL CHECK (epoch > 0),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, scope_kind, scope_id)
);

CREATE FUNCTION weave_bump_authorization_epoch(workspace TEXT, kind TEXT, id TEXT) RETURNS void LANGUAGE sql AS $$
  INSERT INTO weave_authorization_epochs(workspace_id, scope_kind, scope_id, epoch)
  VALUES (workspace, kind, id, 1)
  ON CONFLICT (workspace_id, scope_kind, scope_id)
  DO UPDATE SET epoch = weave_authorization_epochs.epoch + 1, updated_at = now();
$$;

-- One trigger function per table keeps each scope rule next to its columns.
CREATE FUNCTION weave_authorization_epoch_capability_app() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    PERFORM weave_bump_authorization_epoch(OLD.workspace_id, 'capability_app', OLD.id);
  ELSIF OLD.enabled IS DISTINCT FROM NEW.enabled THEN
    PERFORM weave_bump_authorization_epoch(NEW.workspace_id, 'capability_app', NEW.id);
  END IF;
  RETURN NULL;
END;
$$;
CREATE TRIGGER weave_capability_apps_authorization_epoch AFTER UPDATE OR DELETE ON weave_capability_apps
  FOR EACH ROW EXECUTE FUNCTION weave_authorization_epoch_capability_app();

CREATE FUNCTION weave_authorization_epoch_capability_grant() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    PERFORM weave_bump_authorization_epoch(OLD.workspace_id, 'capability_app', OLD.app_id);
  ELSIF TG_OP = 'INSERT' OR OLD.enabled IS DISTINCT FROM NEW.enabled OR OLD.app_id IS DISTINCT FROM NEW.app_id THEN
    PERFORM weave_bump_authorization_epoch(NEW.workspace_id, 'capability_app', NEW.app_id);
  END IF;
  RETURN NULL;
END;
$$;
CREATE TRIGGER weave_capability_grants_authorization_epoch AFTER INSERT OR UPDATE OR DELETE ON weave_capability_grants
  FOR EACH ROW EXECUTE FUNCTION weave_authorization_epoch_capability_grant();

CREATE FUNCTION weave_authorization_epoch_capability_credential() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    PERFORM weave_bump_authorization_epoch(OLD.workspace_id, 'capability_app', OLD.app_id);
  ELSIF OLD.revoked_at IS DISTINCT FROM NEW.revoked_at OR OLD.scopes IS DISTINCT FROM NEW.scopes THEN
    PERFORM weave_bump_authorization_epoch(NEW.workspace_id, 'capability_app', NEW.app_id);
  END IF;
  RETURN NULL;
END;
$$;
CREATE TRIGGER weave_capability_credentials_authorization_epoch AFTER UPDATE OR DELETE ON weave_capability_credentials
  FOR EACH ROW EXECUTE FUNCTION weave_authorization_epoch_capability_credential();

CREATE FUNCTION weave_authorization_epoch_team() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    PERFORM weave_bump_authorization_epoch(OLD.workspace_id, 'team', OLD.id);
  ELSIF OLD.status IS DISTINCT FROM NEW.status OR OLD.lead_avatar_id IS DISTINCT FROM NEW.lead_avatar_id THEN
    PERFORM weave_bump_authorization_epoch(NEW.workspace_id, 'team', NEW.id);
  END IF;
  RETURN NULL;
END;
$$;
CREATE TRIGGER weave_teams_authorization_epoch AFTER UPDATE OR DELETE ON weave_teams
  FOR EACH ROW EXECUTE FUNCTION weave_authorization_epoch_team();

-- Any change to a team's members or to a staged development roster moves the
-- team's version; member rows change rarely and the next call simply checks
-- again.
CREATE FUNCTION weave_authorization_epoch_team_member() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP IN ('UPDATE', 'DELETE') THEN
    PERFORM weave_bump_authorization_epoch(OLD.workspace_id, 'team', OLD.team_id);
  END IF;
  IF TG_OP IN ('INSERT', 'UPDATE') THEN
    PERFORM weave_bump_authorization_epoch(NEW.workspace_id, 'team', NEW.team_id);
  END IF;
  RETURN NULL;
END;
$$;
CREATE TRIGGER weave_team_workers_authorization_epoch AFTER INSERT OR UPDATE OR DELETE ON weave_team_workers
  FOR EACH ROW EXECUTE FUNCTION weave_authorization_epoch_team_member();
CREATE TRIGGER weave_team_development_candidates_authorization_epoch AFTER INSERT OR UPDATE OR DELETE ON weave_team_development_candidates
  FOR EACH ROW EXECUTE FUNCTION weave_authorization_epoch_team_member();

CREATE FUNCTION weave_authorization_epoch_provider_credential() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    PERFORM weave_bump_authorization_epoch(OLD.workspace_id, 'provider_credential', OLD.id);
  ELSIF OLD.enabled IS DISTINCT FROM NEW.enabled OR OLD.revoked_at IS DISTINCT FROM NEW.revoked_at
     OR OLD.deleted_at IS DISTINCT FROM NEW.deleted_at OR OLD.models IS DISTINCT FROM NEW.models
     OR OLD.credential_scope IS DISTINCT FROM NEW.credential_scope OR OLD.credential_user_id IS DISTINCT FROM NEW.credential_user_id
     OR OLD.credential_service_id IS DISTINCT FROM NEW.credential_service_id THEN
    PERFORM weave_bump_authorization_epoch(NEW.workspace_id, 'provider_credential', NEW.id);
  END IF;
  RETURN NULL;
END;
$$;
CREATE TRIGGER weave_provider_credentials_authorization_epoch AFTER UPDATE OR DELETE ON weave_provider_credentials
  FOR EACH ROW EXECUTE FUNCTION weave_authorization_epoch_provider_credential();
