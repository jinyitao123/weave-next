CREATE TABLE IF NOT EXISTS weave_users (
  id           TEXT PRIMARY KEY,
  tenant_id    TEXT NOT NULL DEFAULT 'default',
  username     TEXT NOT NULL,
  password     TEXT NOT NULL,
  display_name TEXT DEFAULT '',
  role         TEXT NOT NULL DEFAULT 'user',
  disabled     BOOLEAN DEFAULT false,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  UNIQUE(tenant_id, username)
);

CREATE TABLE IF NOT EXISTS weave_api_keys (
  id          TEXT PRIMARY KEY,
  tenant_id   TEXT NOT NULL DEFAULT 'default',
  name        TEXT NOT NULL,
  key_hash    TEXT NOT NULL,
  role        TEXT NOT NULL DEFAULT 'service',
  scopes      TEXT[],
  created_by  TEXT DEFAULT '',
  expires_at  TIMESTAMPTZ,
  last_used   TIMESTAMPTZ,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_weave_api_keys_tenant ON weave_api_keys(tenant_id);
CREATE INDEX IF NOT EXISTS idx_weave_api_keys_hash ON weave_api_keys(key_hash);

CREATE TABLE IF NOT EXISTS weave_jobs (
  id            TEXT PRIMARY KEY,
  tenant        TEXT NOT NULL,
  agent         TEXT NOT NULL,
  status        TEXT NOT NULL DEFAULT 'pending',
  request       JSONB NOT NULL,
  result        JSONB,
  error         TEXT DEFAULT '',
  run_id        TEXT DEFAULT '',
  created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  started_at    TIMESTAMPTZ,
  completed_at  TIMESTAMPTZ,
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_weave_jobs_tenant_status ON weave_jobs(tenant, status);
CREATE INDEX IF NOT EXISTS idx_weave_jobs_status_created ON weave_jobs(status, created_at);

CREATE TABLE IF NOT EXISTS weave_schedules (
  source_url  TEXT NOT NULL,
  tenant      TEXT NOT NULL DEFAULT 'default',
  config      JSONB NOT NULL,
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (tenant, source_url)
);
