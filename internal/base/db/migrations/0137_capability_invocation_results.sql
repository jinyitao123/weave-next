ALTER TABLE weave_capability_invocations
  ADD COLUMN IF NOT EXISTS result JSONB,
  ADD COLUMN IF NOT EXISTS error TEXT;
