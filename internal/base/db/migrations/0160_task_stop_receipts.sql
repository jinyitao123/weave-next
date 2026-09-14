-- Normalized observed execution facts received after cancellation/lease loss.
-- This ledger never determines or changes the task's terminal status.
CREATE TABLE weave_task_stop_receipts (
  workspace_id TEXT NOT NULL,
  task_id TEXT NOT NULL,
  claim_epoch BIGINT NOT NULL CHECK (claim_epoch > 0),
  receipt_id TEXT NOT NULL,
  receipt_digest TEXT NOT NULL CHECK (length(receipt_digest) = 64),
  receipt JSONB NOT NULL CHECK (octet_length(receipt::text) <= 4194304),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, task_id, claim_epoch),
  UNIQUE (workspace_id, receipt_id),
  FOREIGN KEY (task_id) REFERENCES weave_task_queue(id)
);
