UPDATE weave_task_queue AS task
SET project_id = snapshot.project_id
FROM weave_team_run_snapshots AS snapshot
WHERE task.project_id IS NULL
  AND task.run_snapshot_id IS NOT NULL
  AND snapshot.workspace_id = task.workspace_id
  AND snapshot.run_id = task.run_snapshot_id
  AND snapshot.project_id IS NOT NULL;
