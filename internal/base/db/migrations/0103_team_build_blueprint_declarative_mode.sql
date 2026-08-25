ALTER TABLE weave_team_build_blueprint_revisions
  DROP CONSTRAINT weave_team_build_blueprint_revisions_workflow_check;

ALTER TABLE weave_team_build_blueprint_revisions
  ADD CONSTRAINT weave_team_build_blueprint_revisions_workflow_check CHECK (
    (
      workflow_mode = 'template'
      AND template_gap_authorization_json IS NULL
      AND template_gap_authorization_hash IS NULL
    )
    OR (
      workflow_mode = 'custom'
      AND template_gap_authorization_json IS NOT NULL
      AND template_gap_authorization_hash IS NOT NULL
    )
    OR (
      workflow_mode = 'declarative_v1'
      AND template_gap_authorization_json IS NULL
      AND template_gap_authorization_hash IS NULL
    )
  );
