-- Per-model request parameter declarations, frozen in each provider revision
-- and in the binding of each model they name. An empty object declares
-- nothing and leaves existing revision hashes intact.
ALTER TABLE weave_provider_credentials ADD COLUMN model_parameters JSONB NOT NULL DEFAULT '{}'::jsonb
 CHECK (jsonb_typeof(model_parameters) = 'object');
ALTER TABLE weave_provider_revisions ADD COLUMN model_parameters JSONB NOT NULL DEFAULT '{}'::jsonb
 CHECK (jsonb_typeof(model_parameters) = 'object');
