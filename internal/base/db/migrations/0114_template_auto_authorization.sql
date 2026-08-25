ALTER TABLE weave_team_build_runs
  DROP CONSTRAINT weave_team_build_runs_authorization_authority_check,
  DROP CONSTRAINT weave_team_build_runs_authorized_revision_check;

ALTER TABLE weave_team_build_runs
  ADD COLUMN authorization_decision_subject TEXT,
  ADD COLUMN authorization_decision_reason TEXT,
  ADD COLUMN authorization_decided_at TIMESTAMPTZ;

UPDATE weave_team_build_runs
SET authorization_decided_at = COALESCE(decided_at, updated_at, created_at)
WHERE authorization_authority IS NOT NULL;

ALTER TABLE weave_team_build_runs
  ADD CONSTRAINT weave_team_build_runs_authorization_authority_check CHECK (
    authorization_authority IS NULL
    OR authorization_authority IN ('reviewed_blueprint', 'auto_build', 'template_auto')
  ),
  ADD CONSTRAINT weave_team_build_runs_authorized_revision_check CHECK (
    (
      authorization_authority IN ('reviewed_blueprint', 'template_auto')
      AND authorized_revision_no IS NOT NULL
      AND authorized_revision_no > 0
      AND authorized_blueprint_hash ~ '^[0-9a-f]{64}$'
      AND authorized_change_set_hash ~ '^[0-9a-f]{64}$'
    )
    OR (
      (authorization_authority IS NULL OR authorization_authority = 'auto_build')
      AND authorized_revision_no IS NULL
      AND authorized_blueprint_hash IS NULL
      AND authorized_change_set_hash IS NULL
    )
  ),
  ADD CONSTRAINT weave_team_build_runs_authorization_decision_check CHECK (
    (
      authorization_authority = 'template_auto'
      AND authorization_decision_subject = 'platform-template-authorizer'
      AND authorization_decision_reason IS NOT NULL
      AND authorization_decision_reason <> ''
      AND authorization_decided_at IS NOT NULL
    )
    OR (
      authorization_authority IS DISTINCT FROM 'template_auto'
      AND authorization_decision_subject IS NULL
      AND authorization_decision_reason IS NULL
    )
  );

CREATE INDEX weave_team_build_runs_template_auto_quota_idx
  ON weave_team_build_runs (workspace_id, authorization_decided_at)
  WHERE execution_strategy = 'template_instantiate'
    AND authorization_authority IS NOT NULL;
