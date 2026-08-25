-- T14B-2A: candidate test run snapshots persist the team build round that
-- produced the candidate, paired with the existing candidate identity
-- (build_run_id + candidate_content_hash).
--
-- build_round_no is nullable for backward compatibility: pre-T14B-2A
-- candidate snapshots and admin API candidate runs (which test a candidate
-- outside any build round) carry no round number. When present it must be a
-- positive round and must be paired with a build_run_id so a round number is
-- never orphaned without its candidate run identity.

ALTER TABLE weave_team_run_snapshots
  ADD COLUMN build_round_no BIGINT,
  ADD CONSTRAINT weave_team_run_snapshots_build_round_check CHECK (
    build_round_no IS NULL OR build_round_no > 0
  ),
  ADD CONSTRAINT weave_team_run_snapshots_build_round_pair_check CHECK (
    build_round_no IS NULL OR build_run_id IS NOT NULL
  );
