ALTER TABLE weave_capability_invocation_tasks
  ADD COLUMN execution_budget_ms BIGINT NOT NULL DEFAULT 2700000,
  ADD COLUMN execution_consumed_ms BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN claim_started_at TIMESTAMPTZ;

ALTER TABLE weave_capability_invocation_tasks
  ADD CONSTRAINT weave_capability_invocation_tasks_execution_budget_check CHECK (
    execution_budget_ms > 0
    AND execution_consumed_ms >= 0
    AND execution_consumed_ms <= execution_budget_ms
  );
