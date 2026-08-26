ALTER TABLE weave_team_build_runs
  ADD COLUMN evaluation_only BOOLEAN NOT NULL DEFAULT FALSE;

UPDATE weave_team_build_runs
SET evaluation_only = TRUE
WHERE evaluation_team_id IS NOT NULL;

ALTER TABLE weave_team_build_runs
  ADD CONSTRAINT weave_team_build_runs_evaluation_only_check CHECK (
    evaluation_only = (evaluation_team_id IS NOT NULL)
  );

ALTER TABLE weave_teams
  ADD COLUMN evaluation_build_run_id TEXT,
  ADD COLUMN evaluation_contract_hash TEXT,
  ADD COLUMN evaluated_at TIMESTAMPTZ;

ALTER TABLE weave_teams
  ADD CONSTRAINT weave_teams_evaluation_provenance_check CHECK (
    (
      evaluation = 'unevaluated'
      AND evaluation_build_run_id IS NULL
      AND evaluation_contract_hash IS NULL
      AND evaluated_at IS NULL
    )
    OR (
      evaluation = 'evaluated'
      AND (
        (
          evaluation_build_run_id IS NULL
          AND evaluation_contract_hash IS NULL
          AND evaluated_at IS NULL
        )
        OR (
          evaluation_build_run_id <> ''
          AND evaluation_contract_hash ~ '^[0-9a-f]{64}$'
          AND evaluated_at IS NOT NULL
        )
      )
    )
  ),
  ADD CONSTRAINT weave_teams_evaluation_build_run_fkey FOREIGN KEY (
    workspace_id, evaluation_build_run_id
  ) REFERENCES weave_team_build_runs (
    workspace_id, build_run_id
  ) ON DELETE RESTRICT;
