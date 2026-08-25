CREATE INDEX weave_team_runs_timer_wake_idx
  ON weave_team_runs ((wait_detail->>'wake_at'), workspace_id, run_id)
  WHERE status = 'parked' AND wait_kind = 'timer';
