-- The Forge business action catalog as last read for a workspace by a
-- developer's verified Forge sign-in. It holds definitions only, never a
-- credential or business data, and is a selection aid: publication and runs
-- read the current catalog under the employee delegation.
CREATE TABLE IF NOT EXISTS weave_business_capability_catalog (
  workspace_id TEXT        PRIMARY KEY,
  version      TEXT        NOT NULL,
  capabilities JSONB       NOT NULL,
  entries      INTEGER     NOT NULL CHECK (entries >= 0),
  fetched_by   TEXT        NOT NULL,
  fetched_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
