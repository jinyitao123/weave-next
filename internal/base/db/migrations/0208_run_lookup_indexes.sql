-- Lookups by run that scanned whole tables under load (#106).
--
-- Every team_workflow enqueue inherits the priority of its run's first task
-- (SELECT priority ... WHERE workspace_id=$1 AND run_snapshot_id=$2 ORDER BY
-- created_at,id LIMIT 1), and fanout joins, member result reuse and run views
-- find a run's tasks by run_snapshot_id; nothing indexed it, so each of them
-- read every task of the workspace. Published-workflow invocations read their
-- final deliverable by run, which scanned every deliverable.
--
-- Built without CONCURRENTLY, like every migration here: on a large task
-- queue the build briefly blocks writes to it.
CREATE INDEX IF NOT EXISTS weave_task_queue_run_snapshot_idx
  ON weave_task_queue (workspace_id, run_snapshot_id, created_at, id)
  WHERE run_snapshot_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS weave_final_deliverables_run_idx
  ON weave_final_deliverables (workspace_id, run_id);

-- Every task that stops (completed, failed, cancelled, ...) runs
-- weave_task_queue_stop_descendants, which looks for child tasks by
-- parent_task_id. With no index it read the workspace's whole task queue on
-- each completion. Few tasks have a parent, so the partial index stays small.
CREATE INDEX IF NOT EXISTS weave_task_queue_parent_idx
  ON weave_task_queue (workspace_id, parent_task_id)
  WHERE parent_task_id IS NOT NULL;
