CREATE INDEX weave_team_runs_status_cancel_grace_deadline_idx
  ON weave_team_runs (status, cancel_grace_deadline_at)
  WHERE status = 'cancel_requested';
