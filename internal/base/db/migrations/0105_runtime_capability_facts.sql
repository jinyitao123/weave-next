ALTER TABLE weave_runtimes
  ADD COLUMN engine_capabilities JSONB NOT NULL DEFAULT '{}',
  ADD COLUMN total_slots INTEGER NOT NULL DEFAULT 1,
  ADD COLUMN active_slots INTEGER NOT NULL DEFAULT 0,
  ADD COLUMN pool_id TEXT,
  ADD COLUMN health_status TEXT NOT NULL DEFAULT 'healthy',
  ADD COLUMN consecutive_infra_failures INTEGER NOT NULL DEFAULT 0,
  ADD COLUMN quarantine_until TIMESTAMPTZ,
  ADD COLUMN last_failure_reason TEXT;

ALTER TABLE weave_runtimes
  ADD CONSTRAINT weave_runtimes_engine_capabilities_object_check CHECK (
    jsonb_typeof(engine_capabilities) = 'object'
  ),
  ADD CONSTRAINT weave_runtimes_total_slots_positive_check CHECK (total_slots >= 1),
  ADD CONSTRAINT weave_runtimes_active_slots_range_check CHECK (
    active_slots >= 0 AND active_slots <= total_slots
  ),
  ADD CONSTRAINT weave_runtimes_pool_id_check CHECK (
    pool_id IS NULL OR (length(btrim(pool_id)) BETWEEN 1 AND 80)
  ),
  ADD CONSTRAINT weave_runtimes_health_status_check CHECK (
    health_status IN ('healthy', 'degraded', 'quarantined')
  ),
  ADD CONSTRAINT weave_runtimes_infra_failures_nonnegative_check CHECK (
    consecutive_infra_failures >= 0
  );

CREATE INDEX weave_runtimes_pool_selection_idx
  ON weave_runtimes (workspace_id, pool_id, last_heartbeat_at DESC)
  WHERE enabled=true AND revoked_at IS NULL AND deleted_at IS NULL AND pool_id IS NOT NULL;
