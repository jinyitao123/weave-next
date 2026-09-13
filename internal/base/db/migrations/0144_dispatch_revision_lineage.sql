-- A user revision is a new dispatch input whose relationship to the previous
-- accepted run and materials is fixed by the server at registration time.
ALTER TABLE weave_dispatch_input_revisions
  ADD COLUMN execution_task TEXT NOT NULL,
  ADD COLUMN revision_kind TEXT NOT NULL CHECK (revision_kind IN ('initial','revision')),
  ADD COLUMN root_input_revision_id TEXT NOT NULL,
  ADD COLUMN parent_input_revision_id TEXT,
  ADD COLUMN parent_run_id TEXT,
  ADD COLUMN parent_delivery_digest TEXT,
  ADD COLUMN parent_materials JSONB NOT NULL DEFAULT '[]'::jsonb
    CHECK (jsonb_typeof(parent_materials) = 'array'),
  ADD CONSTRAINT weave_dispatch_input_revision_shape CHECK (
    (revision_kind = 'initial' AND parent_input_revision_id IS NULL
      AND parent_run_id IS NULL AND parent_delivery_digest IS NULL
      AND jsonb_array_length(parent_materials) = 0)
    OR
    (revision_kind = 'revision' AND parent_input_revision_id IS NOT NULL
      AND parent_run_id IS NOT NULL AND parent_delivery_digest ~ '^[0-9a-f]{64}$'
      AND jsonb_array_length(parent_materials) > 0)
  ),
  ADD CONSTRAINT weave_dispatch_input_root_fk
    FOREIGN KEY (workspace_id,user_id,root_input_revision_id)
    REFERENCES weave_dispatch_input_revisions(workspace_id,user_id,input_revision_id)
    DEFERRABLE INITIALLY DEFERRED,
  ADD CONSTRAINT weave_dispatch_input_parent_fk
    FOREIGN KEY (workspace_id,user_id,parent_input_revision_id)
    REFERENCES weave_dispatch_input_revisions(workspace_id,user_id,input_revision_id)
    DEFERRABLE INITIALLY DEFERRED;

CREATE INDEX weave_dispatch_input_parent
  ON weave_dispatch_input_revisions(workspace_id,user_id,parent_input_revision_id);
