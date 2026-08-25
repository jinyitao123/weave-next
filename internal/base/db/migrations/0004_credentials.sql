-- Credentials in the legacy weave:providers and weave:embedder loom_store namespaces are intentionally not backfilled; plaintext secrets must be configured again.
CREATE TABLE IF NOT EXISTS weave_provider_credentials (
  workspace_id     TEXT NOT NULL REFERENCES weave_workspaces(id) ON DELETE CASCADE,
  id               TEXT NOT NULL,
  name             TEXT NOT NULL,
  base_url         TEXT NOT NULL,
  api_key_cipher   TEXT NOT NULL,
  models           TEXT[] NOT NULL,
  json_object_mode BOOLEAN NOT NULL DEFAULT false,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, id)
);

CREATE TABLE IF NOT EXISTS weave_embedder_credentials (
  workspace_id   TEXT PRIMARY KEY REFERENCES weave_workspaces(id) ON DELETE CASCADE,
  base_url       TEXT NOT NULL,
  api_key_cipher TEXT NOT NULL,
  model          TEXT NOT NULL,
  dimension      INT NOT NULL DEFAULT 0,
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
