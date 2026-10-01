-- Decision 002 (weave-workbench): the Forge credential behind a task is a
-- Forge-issued task delegation, `issuer` is Forge's stable identity source,
-- and the network address is kept separately. Weave revokes the delegation
-- once the consuming run is terminal.
ALTER TABLE weave_task_business_delegations
  ADD COLUMN forge_base_url TEXT,
  ADD COLUMN forge_delegation_id TEXT,
  ADD COLUMN revocation_reason TEXT CHECK (revocation_reason IN ('run_terminal', 'expired')),
  ADD COLUMN revoke_attempts INTEGER NOT NULL DEFAULT 0 CHECK (revoke_attempts >= 0),
  ADD COLUMN revoke_next_attempt_at TIMESTAMPTZ,
  ADD COLUMN revoke_last_error TEXT;

-- Rows from before this migration held the employee's own Forge session and
-- cannot be revoked at Forge; close them so no run keeps using them.
UPDATE weave_task_business_delegations
  SET forge_base_url = issuer, forge_delegation_id = delegation_id::text,
      revoked_at = COALESCE(revoked_at, statement_timestamp()),
      revocation_reason = COALESCE(revocation_reason, 'expired')
  WHERE forge_base_url IS NULL;

ALTER TABLE weave_task_business_delegations
  ALTER COLUMN forge_base_url SET NOT NULL,
  ALTER COLUMN forge_delegation_id SET NOT NULL,
  ADD CHECK (forge_base_url <> '' AND forge_delegation_id <> ''),
  ADD CHECK ((revoked_at IS NULL) = (revocation_reason IS NULL));

CREATE INDEX weave_task_business_delegations_revoke_idx
  ON weave_task_business_delegations(revoke_next_attempt_at NULLS FIRST)
  WHERE revoked_at IS NULL;

CREATE OR REPLACE FUNCTION weave_task_business_delegation_guard()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'task business delegations are retained for audit' USING ERRCODE = '55000';
  END IF;
  IF ROW(OLD.workspace_id, OLD.user_id, OLD.input_revision_id, OLD.delegation_id,
      OLD.credential_ref, OLD.issuer, OLD.external_subject, OLD.external_organization,
      OLD.allowed_actions, OLD.resources, OLD.workflow_id, OLD.workflow_version, OLD.issued_at,
      OLD.forge_base_url, OLD.forge_delegation_id)
    IS DISTINCT FROM
    ROW(NEW.workspace_id, NEW.user_id, NEW.input_revision_id, NEW.delegation_id,
      NEW.credential_ref, NEW.issuer, NEW.external_subject, NEW.external_organization,
      NEW.allowed_actions, NEW.resources, NEW.workflow_id, NEW.workflow_version, NEW.issued_at,
      NEW.forge_base_url, NEW.forge_delegation_id)
    OR NEW.refresh_generation < OLD.refresh_generation
    OR (OLD.revoked_at IS NOT NULL AND (NEW.revoked_at IS DISTINCT FROM OLD.revoked_at
      OR NEW.revocation_reason IS DISTINCT FROM OLD.revocation_reason)) THEN
    RAISE EXCEPTION 'task business delegation facts are immutable' USING ERRCODE = '55000';
  END IF;
  RETURN NEW;
END;
$$;
