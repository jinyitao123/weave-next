ALTER TABLE weave_teams
  ADD COLUMN default_workflow_id TEXT;

ALTER TABLE weave_teams
  ADD CONSTRAINT weave_teams_default_workflow_fk
    FOREIGN KEY (workspace_id, default_workflow_id)
    REFERENCES weave_team_workflows (workspace_id, id)
    ON DELETE RESTRICT;

CREATE INDEX idx_weave_teams_default_workflow
  ON weave_teams (workspace_id, default_workflow_id)
  WHERE default_workflow_id IS NOT NULL;

-- Template instantiation now keeps the aggregate building until publication.
-- Roster materialization therefore records building -> building before the
-- final publication transaction performs building -> active.
ALTER TABLE weave_team_roster_audits
  DROP CONSTRAINT weave_team_roster_audits_status_check,
  ADD CONSTRAINT weave_team_roster_audits_status_check CHECK (
    old_team_status IN ('active', 'archived', 'needs_repair', 'building')
    AND new_team_status IN ('active', 'archived', 'building')
  );

ALTER TABLE weave_team_roster_receipts
  DROP CONSTRAINT weave_team_roster_receipts_response_check,
  ADD CONSTRAINT weave_team_roster_receipts_response_check CHECK (
    jsonb_typeof(response) IS NOT DISTINCT FROM 'object'
    AND response ?& ARRAY[
      'schema_version', 'team_id', 'team_status', 'lead_agent_id',
      'updated_at', 'workers', 'changed', 'audit_id', 'affected_workers'
    ]
    AND response - ARRAY[
      'schema_version', 'team_id', 'team_status', 'lead_agent_id',
      'updated_at', 'workers', 'changed', 'audit_id', 'affected_workers'
    ] = '{}'::jsonb
    AND jsonb_typeof(response->'schema_version') IS NOT DISTINCT FROM 'number'
    AND (response->'schema_version')::text = '1'
    AND jsonb_typeof(response->'team_id') IS NOT DISTINCT FROM 'string'
    AND response->>'team_id' = team_id
    AND response->>'team_status' IN ('active', 'building', 'archived')
    AND jsonb_typeof(response->'lead_agent_id') IS NOT DISTINCT FROM 'string'
    AND response->>'lead_agent_id' <> ''
    AND jsonb_typeof(response->'updated_at') IS NOT DISTINCT FROM 'string'
    AND response->>'updated_at' ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{6}Z$'
    AND weave_team_roster_workers_valid(response->'workers')
    AND jsonb_typeof(response->'changed') IS NOT DISTINCT FROM 'boolean'
    AND weave_team_roster_affected_workers_valid(response->'affected_workers')
    AND (
      (
        response->'changed' = 'true'::jsonb
        AND jsonb_typeof(response->'audit_id') IS NOT DISTINCT FROM 'string'
        AND response->>'audit_id' <> ''
      ) OR (
        response->'changed' = 'false'::jsonb
        AND response->'audit_id' = 'null'::jsonb
        AND response->'affected_workers' = '[]'::jsonb
      )
    )
  );
