ALTER TABLE weave_capability_invocation_cancellations
  ALTER COLUMN requested_credential_id DROP NOT NULL,
  ADD COLUMN requested_by_user_id TEXT,
  ADD CONSTRAINT weave_capability_cancellations_requested_by_user_fk
    FOREIGN KEY (workspace_id, requested_by_user_id)
    REFERENCES weave_users(tenant_id, id) ON DELETE RESTRICT,
  ADD CONSTRAINT weave_capability_cancellations_actor_check CHECK (
    (requested_credential_id IS NULL) <> (requested_by_user_id IS NULL)
  );
