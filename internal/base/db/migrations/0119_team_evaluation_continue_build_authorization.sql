ALTER TABLE weave_team_build_runs
  DROP CONSTRAINT weave_team_build_runs_authorization_authority_check,
  DROP CONSTRAINT weave_team_build_runs_authorized_revision_check;

ALTER TABLE weave_team_build_runs
  ADD CONSTRAINT weave_team_build_runs_authorization_authority_check CHECK (
    authorization_authority IS NULL
    OR authorization_authority IN (
      'reviewed_blueprint',
      'continue_build',
      'auto_build',
      'template_auto'
    )
  ),
  ADD CONSTRAINT weave_team_build_runs_authorized_revision_check CHECK (
    (
      authorization_authority IN (
        'reviewed_blueprint',
        'continue_build',
        'template_auto'
      )
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
  );
