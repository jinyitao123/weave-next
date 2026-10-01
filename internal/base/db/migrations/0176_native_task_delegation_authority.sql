-- 0173 is already published. Add the native identity and signed task-token
-- facts here so an existing installation receives the same schema as a fresh
-- database. Legacy employee-session credentials are erased without deleting
-- the original delegation, input or execution audit references.
ALTER TABLE weave_external_identities
  ADD COLUMN native_organization TEXT NOT NULL DEFAULT '';

CREATE FUNCTION weave_external_native_org_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.native_organization <> '' AND OLD.native_organization IS DISTINCT FROM NEW.native_organization THEN
    RAISE EXCEPTION 'native organization binding is immutable' USING ERRCODE='23514';
  END IF;
  RETURN NEW;
END;
$$;
CREATE TRIGGER weave_external_native_org_immutable BEFORE UPDATE OF native_organization ON weave_external_identities
  FOR EACH ROW EXECUTE FUNCTION weave_external_native_org_guard();

-- The input owns the originating Forge organization, independently of Weave's
-- workspace and of any later desktop session. The existing input-fact guard
-- also protects this field against changes after registration.
ALTER TABLE weave_dispatch_input_revisions
  ADD COLUMN native_organization TEXT NOT NULL DEFAULT '';

ALTER TABLE weave_task_business_delegations
  ADD COLUMN grant_id TEXT NOT NULL DEFAULT '',
  ADD COLUMN scope_sha256 TEXT NOT NULL DEFAULT '',
  DROP CONSTRAINT weave_task_business_delegations_revocation_reason_check,
  ADD CONSTRAINT weave_task_business_delegations_revocation_reason_check
    CHECK (revocation_reason IN ('run_terminal','employee_cancel','account_disabled','superseded','expired'));

-- This one-time credential retirement runs under the migration transaction;
-- the regular immutable-fact guard is immediately reinstated below.
DROP TRIGGER weave_task_business_delegation_guard ON weave_task_business_delegations;
UPDATE weave_task_business_delegations
  SET revoked_at=COALESCE(revoked_at,statement_timestamp()),
      revocation_reason=COALESCE(revocation_reason,'expired'),
      credential_ciphertext='',credential_sha256=repeat('0',64),
      revoke_next_attempt_at=NULL,revoke_last_error=NULL
  WHERE grant_id='';

ALTER TABLE weave_task_business_delegations
  ADD CHECK (grant_id='' OR grant_id=forge_delegation_id),
  ADD CHECK (grant_id='' OR scope_sha256 ~ '^[0-9a-f]{64}$'),
  ADD CHECK (grant_id='' OR issuer !~* '^https?://');

CREATE OR REPLACE FUNCTION weave_task_business_delegation_guard()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP='DELETE' THEN
    RAISE EXCEPTION 'task business delegations are retained for audit' USING ERRCODE='55000';
  END IF;
  IF ROW(OLD.workspace_id,OLD.user_id,OLD.input_revision_id,OLD.delegation_id,
      OLD.credential_ref,OLD.issuer,OLD.external_subject,OLD.external_organization,
      OLD.allowed_actions,OLD.resources,OLD.workflow_id,OLD.workflow_version,
      OLD.forge_delegation_id,OLD.grant_id,OLD.scope_sha256)
    IS DISTINCT FROM
    ROW(NEW.workspace_id,NEW.user_id,NEW.input_revision_id,NEW.delegation_id,
      NEW.credential_ref,NEW.issuer,NEW.external_subject,NEW.external_organization,
      NEW.allowed_actions,NEW.resources,NEW.workflow_id,NEW.workflow_version,
      NEW.forge_delegation_id,NEW.grant_id,NEW.scope_sha256)
    OR ((OLD.forge_base_url IS DISTINCT FROM NEW.forge_base_url
      OR OLD.issued_at IS DISTINCT FROM NEW.issued_at
      OR OLD.expires_at IS DISTINCT FROM NEW.expires_at
      OR OLD.credential_ciphertext IS DISTINCT FROM NEW.credential_ciphertext
      OR OLD.credential_sha256 IS DISTINCT FROM NEW.credential_sha256)
      AND NEW.refresh_generation<=OLD.refresh_generation)
    OR NEW.refresh_generation<OLD.refresh_generation
    OR (OLD.revoked_at IS NOT NULL AND ROW(NEW.revoked_at,NEW.revocation_reason)
      IS DISTINCT FROM ROW(OLD.revoked_at,OLD.revocation_reason)) THEN
    RAISE EXCEPTION 'task business delegation facts are immutable' USING ERRCODE='55000';
  END IF;
  RETURN NEW;
END;
$$;
CREATE TRIGGER weave_task_business_delegation_guard
  BEFORE UPDATE OR DELETE ON weave_task_business_delegations
  FOR EACH ROW EXECUTE FUNCTION weave_task_business_delegation_guard();
