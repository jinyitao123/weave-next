ALTER TABLE weave_team_build_blueprint_revisions
  ADD COLUMN source_report_hash TEXT,
  ADD COLUMN blueprint_patch_json JSONB,
  ADD COLUMN blueprint_patch_hash TEXT;

ALTER TABLE weave_team_build_blueprint_revisions
  ADD CONSTRAINT weave_team_build_blueprint_revisions_patch_provenance_check CHECK (
    (
      source_report_hash IS NULL
      AND blueprint_patch_json IS NULL
      AND blueprint_patch_hash IS NULL
    )
    OR (
      revision_no > 1
      AND source_report_hash IS NOT NULL
      AND source_report_hash ~ '^[0-9a-f]{64}$'
      AND blueprint_patch_json IS NOT NULL
      AND jsonb_typeof(blueprint_patch_json) = 'object'
      AND blueprint_patch_hash IS NOT NULL
      AND blueprint_patch_hash ~ '^[0-9a-f]{64}$'
      AND blueprint_patch_json ->> 'source_report_hash' = source_report_hash
    )
  ) NOT VALID;

CREATE INDEX weave_team_build_blueprint_revisions_source_report_idx
  ON weave_team_build_blueprint_revisions (
    workspace_id, build_run_id, source_report_hash
  )
  WHERE source_report_hash IS NOT NULL;
