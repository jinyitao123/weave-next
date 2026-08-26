CREATE INDEX weave_team_runs_human_inbox_idx
  ON weave_team_runs (workspace_id, updated_at DESC, run_id DESC)
  WHERE status = 'parked' AND wait_kind = 'human';

CREATE INDEX weave_team_runs_human_deadline_idx
  ON weave_team_runs (
    (wait_detail->>'deadline_at'), workspace_id, run_id
  )
  WHERE status = 'parked'
    AND wait_kind = 'human'
    AND wait_detail ? 'deadline_at';
