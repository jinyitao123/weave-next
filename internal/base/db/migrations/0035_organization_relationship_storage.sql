-- R6 organization relationship storage. Existing writers still use the
-- legacy columns until the following cutover migration; this migration only
-- establishes the new facts and backfills rows that can be proven safe.

ALTER TABLE weave_teams
  ADD COLUMN objective TEXT NOT NULL DEFAULT '',
  ADD COLUMN primary_scenario TEXT NOT NULL DEFAULT '',
  ADD COLUMN success_criteria TEXT NOT NULL DEFAULT '',
  ADD COLUMN lead_avatar_id TEXT,
  ADD COLUMN status TEXT NOT NULL DEFAULT 'needs_repair',
  ADD COLUMN updated_at TIMESTAMPTZ NOT NULL DEFAULT now();

ALTER TABLE weave_teams
  ADD CONSTRAINT weave_teams_status_check
    CHECK (status IN ('active', 'archived', 'needs_repair')),
  ADD CONSTRAINT weave_teams_active_lead_check
    CHECK (status <> 'active' OR lead_avatar_id IS NOT NULL),
  ADD CONSTRAINT weave_teams_workspace_id_id_key
    UNIQUE (workspace_id, id);

ALTER TABLE weave_agents
  ADD CONSTRAINT weave_agents_workspace_id_id_key
    UNIQUE (workspace_id, id);

ALTER VIEW weave_org_migration_diagnostics
  RENAME TO weave_org_migration_diagnostics_pre_role;

CREATE VIEW weave_org_migration_diagnostics AS
SELECT
  workspace_id,
  issue_code,
  team_id,
  agent_id,
  agent_version,
  link_id
FROM weave_org_migration_diagnostics_pre_role

UNION ALL

SELECT
  agent.workspace_id,
  'agent_role_spec_mismatch'::TEXT AS issue_code,
  agent.team_id,
  agent.id AS agent_id,
  NULL::INT AS agent_version,
  NULL::TEXT AS link_id
FROM weave_agents AS agent
WHERE agent.deleted = false
  AND agent.spec->>'role' IS DISTINCT FROM agent.role

UNION ALL

SELECT
  link.workspace_id,
  'manages_target_role_spec_mismatch'::TEXT AS issue_code,
  team.id AS team_id,
  target.id AS agent_id,
  NULL::INT AS agent_version,
  link.id::TEXT AS link_id
FROM weave_agent_links AS link
JOIN weave_agents AS lead
  ON lead.workspace_id = link.workspace_id
 AND lead.id = link.from_agent_id
JOIN weave_teams AS team
  ON team.workspace_id = link.workspace_id
 AND team.id = lead.team_id
JOIN weave_agents AS target
  ON target.workspace_id = link.workspace_id
 AND target.id = link.to_agent_id
WHERE link.type = 'manages'
  AND lead.role = 'avatar'
  AND lead.spec->>'role' = 'avatar'
  AND lead.deleted = false
  AND target.role = 'worker'
  AND target.deleted = false
  AND target.spec->>'role' IS DISTINCT FROM target.role;

CREATE TABLE weave_team_workers (
  workspace_id TEXT NOT NULL,
  team_id TEXT NOT NULL,
  worker_agent_id TEXT NOT NULL,
  duty TEXT NOT NULL DEFAULT '',
  when_to_use TEXT NOT NULL DEFAULT '',
  context_instruction TEXT NOT NULL DEFAULT '',
  allowed_kinds TEXT[] NOT NULL,
  default_kind TEXT NOT NULL,
  result_requirement TEXT NOT NULL DEFAULT '',
  enabled BOOLEAN NOT NULL DEFAULT true,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (team_id, worker_agent_id),
  CONSTRAINT weave_team_workers_team_fk
    FOREIGN KEY (workspace_id, team_id)
    REFERENCES weave_teams (workspace_id, id) ON DELETE CASCADE,
  CONSTRAINT weave_team_workers_worker_fk
    FOREIGN KEY (workspace_id, worker_agent_id)
    REFERENCES weave_agents (workspace_id, id) ON DELETE RESTRICT,
  CONSTRAINT weave_team_workers_allowed_kinds_check CHECK (
    array_ndims(allowed_kinds) = 1
    AND cardinality(allowed_kinds) > 0
    AND allowed_kinds <@ ARRAY['consult', 'dispatch', 'handoff']::TEXT[]
    AND cardinality(allowed_kinds) =
      (CASE WHEN 'consult' = ANY(allowed_kinds) THEN 1 ELSE 0 END) +
      (CASE WHEN 'dispatch' = ANY(allowed_kinds) THEN 1 ELSE 0 END) +
      (CASE WHEN 'handoff' = ANY(allowed_kinds) THEN 1 ELSE 0 END)
  ),
  CONSTRAINT weave_team_workers_default_kind_check
    CHECK (default_kind = ANY(allowed_kinds))
);

UPDATE weave_teams AS team
SET lead_avatar_id = lead.id,
    updated_at = now()
FROM weave_agents AS lead
WHERE lead.workspace_id = team.workspace_id
  AND lead.team_id = team.id
  AND lead.role = 'avatar'
  AND lead.spec->>'role' = 'avatar'
  AND lead.deleted = false;

INSERT INTO weave_team_workers (
  workspace_id,
  team_id,
  worker_agent_id,
  duty,
  allowed_kinds,
  default_kind,
  created_at,
  updated_at
)
SELECT
  team.workspace_id,
  team.id,
  worker.id,
  link.instruction,
  ARRAY[link.kind]::TEXT[],
  link.kind,
  link.created_at,
  link.created_at
FROM weave_teams AS team
JOIN weave_agents AS lead
  ON lead.workspace_id = team.workspace_id
 AND lead.id = team.lead_avatar_id
 AND lead.role = 'avatar'
 AND lead.spec->>'role' = 'avatar'
 AND lead.deleted = false
JOIN weave_agent_links AS link
  ON link.workspace_id = team.workspace_id
 AND link.from_agent_id = lead.id
 AND link.type = 'manages'
JOIN weave_agents AS worker
  ON worker.workspace_id = team.workspace_id
 AND worker.id = link.to_agent_id
 AND worker.role = 'worker'
 AND worker.spec->>'role' = 'worker'
 AND worker.deleted = false;

UPDATE weave_teams AS team
SET status = 'active',
    updated_at = now()
WHERE team.lead_avatar_id IS NOT NULL
  AND EXISTS (
    SELECT 1
    FROM weave_team_workers AS worker
    WHERE worker.workspace_id = team.workspace_id
      AND worker.team_id = team.id
      AND worker.enabled = true
  )
  AND NOT EXISTS (
    SELECT 1
    FROM weave_org_migration_diagnostics AS diagnostic
    WHERE diagnostic.workspace_id = team.workspace_id
      AND diagnostic.team_id = team.id
      AND diagnostic.issue_code IN (
        'team_without_active_avatar',
        'team_without_migratable_worker',
        'team_worker_without_manages_relation',
        'manages_source_not_team_avatar',
        'manages_target_not_active_worker',
        'agent_role_spec_mismatch',
        'manages_target_role_spec_mismatch'
      )
  )
  AND NOT EXISTS (
    SELECT 1
    FROM weave_agents AS lead
    JOIN weave_agent_links AS link
      ON link.workspace_id = team.workspace_id
     AND link.from_agent_id = lead.id
     AND link.type = 'manages'
    JOIN weave_agents AS worker
      ON worker.workspace_id = team.workspace_id
     AND worker.id = link.to_agent_id
    WHERE lead.workspace_id = team.workspace_id
      AND lead.team_id = team.id
      AND lead.role = 'avatar'
      AND lead.deleted = false
      AND (
        lead.spec->>'role' IS DISTINCT FROM lead.role
        OR worker.spec->>'role' IS DISTINCT FROM worker.role
      )
  );

ALTER TABLE weave_teams
  ADD CONSTRAINT weave_teams_lead_avatar_fk
    FOREIGN KEY (workspace_id, lead_avatar_id)
    REFERENCES weave_agents (workspace_id, id) ON DELETE RESTRICT;

CREATE UNIQUE INDEX uniq_weave_active_team_lead_avatar
  ON weave_teams (lead_avatar_id)
  WHERE status = 'active' AND lead_avatar_id IS NOT NULL;
