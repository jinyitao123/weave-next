ALTER TABLE weave_schedule
  DROP CONSTRAINT weave_schedule_pkey,
  ADD CONSTRAINT weave_schedule_pkey PRIMARY KEY (workspace_id, id);

-- Keep weave_schedule_workspace_id_key: the occurrences composite foreign key
-- depends on it, and removing it would require rebuilding an unchanged FK.
