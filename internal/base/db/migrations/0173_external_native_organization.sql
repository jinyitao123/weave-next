-- Bind native organization only after a trusted original employee login.
-- Existing empty bindings are not usable for task grants until that login.
ALTER TABLE weave_external_identities ADD COLUMN native_organization TEXT NOT NULL DEFAULT '';
CREATE FUNCTION weave_external_native_org_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.native_organization <> '' AND OLD.native_organization IS DISTINCT FROM NEW.native_organization THEN
    RAISE EXCEPTION 'native organization binding is immutable' USING ERRCODE='23514';
  END IF;
  RETURN NEW;
END; $$;
CREATE TRIGGER weave_external_native_org_immutable BEFORE UPDATE OF native_organization ON weave_external_identities
  FOR EACH ROW EXECUTE FUNCTION weave_external_native_org_guard();
ALTER TABLE weave_task_business_delegations ADD COLUMN grant_id TEXT NOT NULL DEFAULT '',
  ADD COLUMN scope_sha256 TEXT NOT NULL DEFAULT '';
-- Previously encrypted employee sessions must cease to exist in task storage.
-- Keep frozen input, action, credential-reference and prior audit identity.
UPDATE weave_task_business_delegations SET credential_ciphertext='',credential_sha256=repeat('0',64)
 WHERE grant_id='';

CREATE OR REPLACE FUNCTION weave_task_business_delegation_guard()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'task business delegations are retained for audit' USING ERRCODE='55000'; END IF;
 IF ROW(OLD.workspace_id,OLD.user_id,OLD.input_revision_id,OLD.delegation_id,OLD.credential_ref,OLD.issuer,OLD.external_subject,
    OLD.allowed_actions,OLD.resources,OLD.workflow_id,OLD.workflow_version)
  IS DISTINCT FROM ROW(NEW.workspace_id,NEW.user_id,NEW.input_revision_id,NEW.delegation_id,NEW.credential_ref,NEW.issuer,NEW.external_subject,
    NEW.allowed_actions,NEW.resources,NEW.workflow_id,NEW.workflow_version)
  OR (OLD.revoked_at IS NOT NULL AND NEW.revoked_at IS DISTINCT FROM OLD.revoked_at)
  OR (OLD.grant_id<>'' AND (NEW.external_organization<>OLD.external_organization OR NEW.grant_id<>OLD.grant_id OR NEW.scope_sha256<>OLD.scope_sha256 OR NEW.refresh_generation<OLD.refresh_generation))
  OR (NEW.issued_at<OLD.issued_at AND OLD.grant_id<>'') THEN
  RAISE EXCEPTION 'task business delegation scope is immutable' USING ERRCODE='55000';
 END IF;
 RETURN NEW;
END; $$;
