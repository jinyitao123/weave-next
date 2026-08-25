CREATE TABLE weave_run_terminal_markers (
  workspace_id TEXT NOT NULL,
  run_id TEXT NOT NULL,
  schema_version SMALLINT NOT NULL,
  attempt_generation BIGINT NOT NULL,
  attempt_id UUID NOT NULL,
  agent TEXT NOT NULL,
  attribution_scope TEXT NOT NULL,
  team_id TEXT,
  workflow_id TEXT,
  workflow_version INTEGER,
  run_snapshot_id TEXT,
  parent_run_id TEXT,
  parent_seq BIGINT,
  aggregation_parent_run_id TEXT,
  task_group_id TEXT,
  run_started_at TEXT NOT NULL,
  phase TEXT NOT NULL,
  status TEXT NOT NULL,
  stop_reason TEXT NOT NULL,
  source TEXT NOT NULL,
  terminal_at TIMESTAMPTZ NOT NULL,
  evidence_kind TEXT NOT NULL,
  checkpoint_graph TEXT,
  checkpoint_seq BIGINT,
  checkpoint_saved_at TIMESTAMPTZ,
  usage_input_tokens BIGINT NOT NULL,
  usage_output_tokens BIGINT NOT NULL,
  usage_cost_usd DOUBLE PRECISION NOT NULL,
  audit_state TEXT NOT NULL,
  audit_schema_version SMALLINT,
  lineage_state TEXT NOT NULL,
  last_error_code TEXT,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,

  CONSTRAINT weave_run_terminal_markers_pkey PRIMARY KEY (workspace_id, run_id),
  CONSTRAINT weave_run_terminal_markers_identity_nonempty_check CHECK (
    workspace_id <> '' AND run_id <> '' AND agent <> '' AND run_started_at <> '' AND stop_reason <> ''
  ),
  CONSTRAINT weave_run_terminal_markers_schema_version_check CHECK (schema_version = 1),
  CONSTRAINT weave_run_terminal_markers_attempt_generation_check CHECK (attempt_generation >= 1),
  CONSTRAINT weave_run_terminal_markers_attribution_scope_check CHECK (
    attribution_scope IN ('legacy_unattributed', 'team_free_collab', 'fixed_workflow')
  ),
  CONSTRAINT weave_run_terminal_markers_optional_text_check CHECK (
    (team_id IS NULL OR team_id <> '') AND (workflow_id IS NULL OR workflow_id <> '')
    AND (run_snapshot_id IS NULL OR run_snapshot_id <> '') AND (parent_run_id IS NULL OR parent_run_id <> '')
    AND (aggregation_parent_run_id IS NULL OR aggregation_parent_run_id <> '')
    AND (task_group_id IS NULL OR task_group_id <> '') AND (checkpoint_graph IS NULL OR checkpoint_graph <> '')
    AND (last_error_code IS NULL OR last_error_code <> '')
  ),
  CONSTRAINT weave_run_terminal_markers_workflow_pair_check CHECK (
    (workflow_id IS NULL AND workflow_version IS NULL)
    OR (workflow_id IS NOT NULL AND workflow_version IS NOT NULL AND workflow_version >= 1)
  ),
  CONSTRAINT weave_run_terminal_markers_scope_association_check CHECK (
    attribution_scope = 'legacy_unattributed'
    OR (attribution_scope = 'fixed_workflow' AND team_id IS NOT NULL AND workflow_id IS NOT NULL
      AND workflow_version IS NOT NULL AND run_snapshot_id IS NOT NULL)
    OR (attribution_scope = 'team_free_collab' AND team_id IS NOT NULL AND run_snapshot_id IS NOT NULL
      AND workflow_id IS NULL AND workflow_version IS NULL)
  ),
  CONSTRAINT weave_run_terminal_markers_parent_pair_check CHECK (
    (parent_run_id IS NULL AND parent_seq IS NULL)
    OR (parent_run_id IS NOT NULL AND parent_seq IS NOT NULL AND parent_seq >= 0 AND parent_run_id <> run_id)
  ),
  CONSTRAINT weave_run_terminal_markers_aggregation_parent_check CHECK (
    aggregation_parent_run_id IS NULL
    OR (parent_run_id IS NOT NULL AND aggregation_parent_run_id = parent_run_id)
  ),
  CONSTRAINT weave_run_terminal_markers_started_at_shape_check CHECK (
    run_started_at ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?(Z|[+-][0-9]{2}:[0-9]{2})$'
  ),
  CONSTRAINT weave_run_terminal_markers_phase_check CHECK (phase IN ('yielded', 'final')),
  CONSTRAINT weave_run_terminal_markers_status_check CHECK (status IN ('yielded', 'success', 'failed')),
  CONSTRAINT weave_run_terminal_markers_phase_status_check CHECK (
    (phase = 'yielded' AND status = 'yielded') OR (phase = 'final' AND status IN ('success', 'failed'))
  ),
  CONSTRAINT weave_run_terminal_markers_source_check CHECK (source IN ('normal', 'reconciler')),
  CONSTRAINT weave_run_terminal_markers_reconciler_shape_check CHECK (
    source <> 'reconciler' OR (phase = 'final' AND status = 'failed' AND stop_reason = 'interrupted')
  ),
  CONSTRAINT weave_run_terminal_markers_evidence_kind_check CHECK (
    evidence_kind IN ('run_result', 'checkpoint', 'registry_only')
  ),
  CONSTRAINT weave_run_terminal_markers_checkpoint_evidence_check CHECK (
    (evidence_kind = 'checkpoint' AND checkpoint_graph IS NOT NULL AND checkpoint_seq IS NOT NULL
      AND checkpoint_seq >= 1 AND checkpoint_saved_at IS NOT NULL)
    OR (evidence_kind IN ('run_result', 'registry_only') AND checkpoint_graph IS NULL
      AND checkpoint_seq IS NULL AND checkpoint_saved_at IS NULL)
  ),
  CONSTRAINT weave_run_terminal_markers_registry_only_check CHECK (
    evidence_kind <> 'registry_only' OR audit_state = 'blocked'
  ),
  CONSTRAINT weave_run_terminal_markers_usage_check CHECK (
    usage_input_tokens >= 0 AND usage_output_tokens >= 0 AND usage_cost_usd >= 0
    AND usage_cost_usd < 'Infinity'::DOUBLE PRECISION
  ),
  CONSTRAINT weave_run_terminal_markers_audit_state_check CHECK (audit_state IN ('materialized', 'blocked')),
  CONSTRAINT weave_run_terminal_markers_audit_schema_check CHECK (
    (audit_state = 'materialized' AND audit_schema_version = 3)
    OR (audit_state = 'blocked' AND audit_schema_version IS NULL)
  ),
  CONSTRAINT weave_run_terminal_markers_lineage_state_check CHECK (lineage_state IN ('pending', 'complete', 'failed')),
  CONSTRAINT weave_run_terminal_markers_error_state_check CHECK (
    (audit_state <> 'blocked' AND lineage_state <> 'failed') OR last_error_code IS NOT NULL
  )
);
