-- Optional Feishu transport. Identities and task states remain in their native stores.
CREATE TABLE weave_feishu_links (
 app_id TEXT NOT NULL,
 workspace_id TEXT NOT NULL,
 user_id TEXT NOT NULL,
 code_hash TEXT,
 code_expires_at TIMESTAMPTZ,
 open_id TEXT,
 chat_id TEXT,
 permission_sets JSONB NOT NULL DEFAULT '[]',
 expires_at TIMESTAMPTZ NOT NULL,
 PRIMARY KEY(app_id,workspace_id,user_id),
 UNIQUE(app_id,open_id),
 UNIQUE(code_hash),
 FOREIGN KEY(user_id) REFERENCES weave_users(id),
 CHECK ((code_hash IS NULL) = (code_expires_at IS NULL))
);
-- This is a callback receipt journal, not a task queue. Frozen commands enter the
-- existing dispatch queue; response delivery is drained by the employee event worker.
CREATE TABLE weave_feishu_messages (
 app_id TEXT NOT NULL,
 message_id TEXT NOT NULL,
 open_id TEXT NOT NULL,
 workspace_id TEXT,
 user_id TEXT,
 content_hash TEXT NOT NULL,
 command JSONB NOT NULL,
 response TEXT,
 reply_message_id TEXT,
 input_revision_id TEXT,
 run_id TEXT,
 next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
 attempts INTEGER NOT NULL DEFAULT 0,
 created_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
 PRIMARY KEY(app_id,message_id)
);
ALTER TABLE weave_employee_run_event_outbox
 ADD COLUMN feishu_state TEXT NOT NULL DEFAULT 'pending' CHECK(feishu_state IN ('pending','delivered')),
 ADD COLUMN feishu_attempts INTEGER NOT NULL DEFAULT 0,
 ADD COLUMN feishu_next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
 ADD COLUMN feishu_last_error TEXT,
 ADD COLUMN feishu_message_id TEXT;
CREATE INDEX weave_employee_run_event_outbox_feishu_idx ON weave_employee_run_event_outbox(feishu_next_attempt_at)
 WHERE feishu_state='pending';
