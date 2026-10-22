-- An explicit functional declaration, frozen in each provider revision. Empty
-- retains the adapter's existing field and leaves existing revision hashes intact.
ALTER TABLE weave_provider_credentials ADD COLUMN output_limit_field TEXT NOT NULL DEFAULT ''
 CHECK (output_limit_field IN ('', 'max_tokens', 'max_completion_tokens'));
ALTER TABLE weave_provider_revisions ADD COLUMN output_limit_field TEXT NOT NULL DEFAULT ''
 CHECK (output_limit_field IN ('', 'max_tokens', 'max_completion_tokens'));
