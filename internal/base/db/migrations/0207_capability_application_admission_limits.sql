-- An application may have its own limit on capability invocations in flight,
-- next to its workspace's. A limited application's slots live in
-- weave_capability_quota_slots under scope_application_id; the workspace's
-- keep scope_application_id ''. An admission of a limited application holds
-- one slot in each scope. An application without a row here is limited only
-- by its workspace, as before.
CREATE TABLE weave_capability_application_quotas (
  workspace_id TEXT NOT NULL,
  application_id TEXT NOT NULL,
  max_active_invocations INTEGER NOT NULL CHECK (max_active_invocations BETWEEN 1 AND 20000),
  slots_ready BOOLEAN NOT NULL DEFAULT false,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, application_id),
  FOREIGN KEY (workspace_id, application_id) REFERENCES weave_capability_apps (workspace_id, id) ON DELETE CASCADE
);

ALTER TABLE weave_capability_quota_slots ADD COLUMN scope_application_id TEXT NOT NULL DEFAULT '';
ALTER TABLE weave_capability_quota_slots DROP CONSTRAINT weave_capability_quota_slots_pkey;
ALTER TABLE weave_capability_quota_slots ADD PRIMARY KEY (workspace_id, scope_application_id, slot);

DROP INDEX weave_capability_quota_slots_holder_idx;
CREATE UNIQUE INDEX weave_capability_quota_slots_holder_idx
  ON weave_capability_quota_slots (workspace_id, scope_application_id, holder_kind, holder_application_id, holder_id)
  WHERE holder_kind IS NOT NULL;
DROP INDEX weave_capability_quota_slots_free_idx;
CREATE INDEX weave_capability_quota_slots_free_idx
  ON weave_capability_quota_slots (workspace_id, scope_application_id, slot)
  WHERE holder_kind IS NULL AND NOT retired;
DROP INDEX weave_capability_quota_slots_retired_idx;
CREATE INDEX weave_capability_quota_slots_retired_idx
  ON weave_capability_quota_slots (workspace_id, scope_application_id)
  WHERE retired;
