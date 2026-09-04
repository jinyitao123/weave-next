ALTER TABLE weave_teams
    ADD COLUMN IF NOT EXISTS display_name TEXT NOT NULL DEFAULT '';

UPDATE weave_teams
SET display_name = name
WHERE btrim(display_name) = '';
