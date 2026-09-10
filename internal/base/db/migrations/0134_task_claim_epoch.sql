ALTER TABLE weave_task_queue ADD COLUMN claim_epoch bigint NOT NULL DEFAULT 0 CHECK (claim_epoch >= 0);
