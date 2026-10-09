-- A paused runtime takes no new work but finishes and renews what it holds;
-- unlike disabling, pausing is reversible. A probe request asks the Host to
-- re-detect its engines at its next heartbeat; its next hello clears it.
ALTER TABLE weave_runtimes
  ADD COLUMN IF NOT EXISTS paused_at TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS probe_requested_at TIMESTAMPTZ;
