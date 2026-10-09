-- Optional server-held git credential for an environment. When it is set,
-- runtime nodes reach the repository only through a task-scoped proxy on the
-- server and never receive the token; the token is sealed with the server's
-- credential key. Without it, nodes keep using their own git credentials.
ALTER TABLE weave_environments
  ADD COLUMN IF NOT EXISTS git_username TEXT NOT NULL DEFAULT '' CHECK (length(git_username) <= 100),
  ADD COLUMN IF NOT EXISTS git_token_sealed TEXT,
  ADD COLUMN IF NOT EXISTS git_token_set_at TIMESTAMPTZ;
