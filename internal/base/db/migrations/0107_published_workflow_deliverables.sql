-- Published conversation workflows predate node-output artifact projection.
-- Backfill their terminal task output so completed conversations never show
-- an empty deliverables panel. Intermediate outputs cannot be reconstructed
-- historically; new runs persist them at execution time.
WITH completed_workflow_outputs AS (
  SELECT
    snapshot.workspace_id,
    snapshot.project_id,
    snapshot.run_id AS run_snapshot_id,
    run.run_id,
    conversation.id AS conversation_id,
    conversation.user_id,
    team.lead_avatar_id,
    COALESCE(task.completed_at, run.terminal_at, run.updated_at) AS created_at,
    CASE
      WHEN jsonb_typeof(task.result->'output') = 'string'
        THEN task.result->>'output'
      ELSE jsonb_pretty(task.result->'output')
    END AS content
  FROM weave_team_run_snapshots AS snapshot
  JOIN weave_team_runs AS run
    ON run.workspace_id=snapshot.workspace_id
   AND run.run_snapshot_id=snapshot.run_id
   AND run.status='succeeded'
  JOIN weave_task_queue AS task
    ON task.workspace_id=run.workspace_id
   AND task.id=run.source_task_id
   AND task.status='completed'
   AND jsonb_typeof(task.result)='object'
   AND task.result ? 'output'
  JOIN weave_teams AS team
    ON team.workspace_id=snapshot.workspace_id AND team.id=snapshot.team_id
  JOIN weave_conversations AS conversation
    ON conversation.workspace_id=snapshot.workspace_id
   AND conversation.id=snapshot.trigger_source_v2->>'source_ref'
   AND conversation.parent_message_id IS NULL
  WHERE snapshot.mode='fixed_workflow'
    AND snapshot.trigger_source_v2->>'type'='conversation_explicit'
)
INSERT INTO weave_final_deliverables (
  id, workspace_id, project_id, conversation_id, user_id, lead_avatar_id,
  session_id, event_id, run_id, run_snapshot_id, title, content,
  content_type, metadata, created_at
)
SELECT
  'deliverable_' || md5(
    output.workspace_id || chr(31) || output.run_id || chr(31) ||
    'workflow-final:deliver'
  ),
  output.workspace_id,
  output.project_id,
  output.conversation_id,
  output.user_id,
  output.lead_avatar_id,
  output.conversation_id,
  'workflow-final:deliver:' || output.run_id,
  output.run_id,
  output.run_snapshot_id,
  '最终产物 · 最终交付',
  output.content,
  CASE
    WHEN lower(btrim(output.content)) LIKE '<!doctype html%'
      OR lower(btrim(output.content)) LIKE '<html%'
      THEN 'text/html'
    WHEN lower(btrim(output.content)) LIKE '<svg%'
      THEN 'image/svg+xml'
    WHEN left(btrim(output.content), 1) IN ('{', '[')
      THEN 'application/json'
    ELSE 'text/markdown'
  END,
  jsonb_build_object(
    'source', 'published_workflow',
    'artifact_kind', 'final',
    'node_id', 'deliver',
    'node_label', '最终交付',
    'node_type', 'deliver',
    'backfilled', true
  ),
  output.created_at
FROM completed_workflow_outputs AS output
WHERE btrim(output.content) <> ''
ON CONFLICT DO NOTHING;
