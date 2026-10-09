-- Feishu apps entered in the admin console, one per workspace. Secrets are
-- sealed with the server credential key and never read back by the console.
-- An app_id serves one workspace only, so every existing app_id-scoped query
-- stays scoped to that workspace.
CREATE TABLE IF NOT EXISTS weave_feishu_apps (
  workspace_id              TEXT        PRIMARY KEY,
  app_id                    TEXT        NOT NULL UNIQUE,
  tenant_key                TEXT        NOT NULL,
  app_secret_sealed         TEXT        NOT NULL,
  verification_token_sealed TEXT        NOT NULL,
  encrypt_key_sealed        TEXT        NOT NULL,
  callback_key              TEXT        NOT NULL UNIQUE,
  revision                  BIGINT      NOT NULL DEFAULT 1,
  updated_by                TEXT        NOT NULL,
  updated_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_check_at             TIMESTAMPTZ,
  last_check_ok             BOOLEAN,
  last_check_reason         TEXT
);

-- Field names only; values never enter the audit.
CREATE TABLE IF NOT EXISTS weave_feishu_app_audit (
  id           BIGSERIAL   PRIMARY KEY,
  workspace_id TEXT        NOT NULL,
  actor        TEXT        NOT NULL,
  action       TEXT        NOT NULL CHECK (action IN ('save', 'delete', 'check')),
  revision     BIGINT      NOT NULL,
  fields       JSONB       NOT NULL DEFAULT '[]',
  outcome      TEXT,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS weave_feishu_app_audit_workspace_idx ON weave_feishu_app_audit(workspace_id, created_at);

-- Teams are not reachable from Feishu until enabled here.
CREATE TABLE IF NOT EXISTS weave_feishu_team_access (
  workspace_id TEXT        NOT NULL,
  team_id      TEXT        NOT NULL,
  enabled      BOOLEAN     NOT NULL DEFAULT false,
  workflow_id  TEXT,
  notify       JSONB       NOT NULL,
  revision     BIGINT      NOT NULL DEFAULT 1,
  updated_by   TEXT        NOT NULL,
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, team_id)
);

CREATE TABLE IF NOT EXISTS weave_feishu_team_access_audit (
  id           BIGSERIAL   PRIMARY KEY,
  workspace_id TEXT        NOT NULL,
  team_id      TEXT        NOT NULL,
  actor        TEXT        NOT NULL,
  revision     BIGINT      NOT NULL,
  before       JSONB,
  after        JSONB       NOT NULL,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- A kind the team chose not to deliver is settled, not retried.
ALTER TABLE weave_employee_run_event_outbox DROP CONSTRAINT IF EXISTS weave_employee_run_event_outbox_feishu_state_check;
ALTER TABLE weave_employee_run_event_outbox ADD CONSTRAINT weave_employee_run_event_outbox_feishu_state_check
  CHECK (feishu_state IN ('pending', 'delivered', 'suppressed'));
