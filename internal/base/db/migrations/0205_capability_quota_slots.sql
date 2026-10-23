-- Capability admission holds one slot per in-flight invocation instead of
-- counting in-flight work under one workspace lock, so admissions that find a
-- free slot do not wait for each other. A workspace's slots are created by its
-- first admission or quota change after this migration (slots_ready), from the
-- same live-owner queries the count used, so work admitted before keeps its
-- place. A slot whose holder is no longer in flight is reclaimed when an
-- admission finds no free slot; a slot above a lowered quota is retired and
-- removed once its holder ends.
ALTER TABLE weave_capability_quotas ADD COLUMN slots_ready BOOLEAN NOT NULL DEFAULT false;

CREATE TABLE weave_capability_quota_slots (
  workspace_id TEXT NOT NULL REFERENCES weave_capability_quotas (workspace_id) ON DELETE CASCADE,
  slot INTEGER NOT NULL CHECK (slot >= 0),
  retired BOOLEAN NOT NULL DEFAULT false,
  holder_kind TEXT CHECK (holder_kind IN ('workflow', 'inline')),
  -- workflow holders are named by invocation_id, inline holders by request_id.
  holder_application_id TEXT,
  holder_id TEXT,
  held_at TIMESTAMPTZ,
  PRIMARY KEY (workspace_id, slot),
  CHECK ((holder_kind IS NULL) = (holder_application_id IS NULL)
     AND (holder_kind IS NULL) = (holder_id IS NULL)
     AND (holder_kind IS NULL) = (held_at IS NULL))
);
CREATE UNIQUE INDEX weave_capability_quota_slots_holder_idx
  ON weave_capability_quota_slots (workspace_id, holder_kind, holder_application_id, holder_id)
  WHERE holder_kind IS NOT NULL;
CREATE INDEX weave_capability_quota_slots_free_idx
  ON weave_capability_quota_slots (workspace_id, slot)
  WHERE holder_kind IS NULL AND NOT retired;
CREATE INDEX weave_capability_quota_slots_retired_idx
  ON weave_capability_quota_slots (workspace_id)
  WHERE retired;
