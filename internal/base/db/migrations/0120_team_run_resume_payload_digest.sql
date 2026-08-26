ALTER TABLE weave_team_run_transitions
  ADD COLUMN payload_digest BYTEA;

ALTER TABLE weave_team_run_transitions
  ADD CONSTRAINT weave_team_run_transitions_payload_digest_check
  CHECK (payload_digest IS NULL OR octet_length(payload_digest) = 32);
