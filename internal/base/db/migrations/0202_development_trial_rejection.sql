-- A trial the kernel definitively refused to admit used to keep no trace of
-- the refusal, so it showed as submitting forever. The refusal is recorded
-- here; a trial with an admission receipt has none. Uncertain outcomes record
-- nothing and are settled by retrying the same request.
ALTER TABLE weave_team_development_trials
  ADD COLUMN rejected_code TEXT,
  ADD COLUMN rejected_at TIMESTAMPTZ,
  ADD CONSTRAINT weave_team_development_trials_rejection_check
    CHECK ((rejected_code IS NULL) = (rejected_at IS NULL)
      AND (rejected_code IS NULL OR (rejected_code <> '' AND receipt IS NULL)));
