-- Existing teams and all legacy creation paths remain evaluated. The template
-- instantiate strategy opts into unevaluated explicitly when materializing its
-- team through the compiler applier.
ALTER TABLE weave_teams
  ADD COLUMN evaluation TEXT NOT NULL DEFAULT 'evaluated',
  ADD CONSTRAINT weave_teams_evaluation_check
    CHECK (evaluation IN ('unevaluated', 'evaluated'));
