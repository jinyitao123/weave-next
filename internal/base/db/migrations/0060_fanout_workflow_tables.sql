CREATE TABLE weave_fanout_intent (
  workspace_id TEXT NOT NULL,
  intent_id TEXT NOT NULL,
  parent_run_id TEXT NOT NULL,
  workflow_id TEXT NOT NULL,
  workflow_version BIGINT NOT NULL,
  run_snapshot_id TEXT NOT NULL,
  node_id TEXT NOT NULL,
  previous_checkpoint_sequence BIGINT NOT NULL,
  node_entry_ordinal BIGINT NOT NULL,
  generation TEXT NOT NULL,
  plan_hash TEXT NOT NULL,
  resume_token_hash BYTEA NOT NULL,
  creator_epoch BIGINT NOT NULL,
  creator_attempt_generation BIGINT NOT NULL,
  creator_attempt_id UUID NOT NULL,
  activation_deadline_at TIMESTAMPTZ NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('pending','active','voided')),
  checkpoint_sequence BIGINT,
  activated_at TIMESTAMPTZ,
  voided_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (workspace_id, intent_id),
  UNIQUE (workspace_id, parent_run_id, node_id, generation),
  UNIQUE (workspace_id, parent_run_id, resume_token_hash),
  CHECK ((status = 'pending' AND checkpoint_sequence IS NULL AND activated_at IS NULL) OR
         (status = 'active' AND checkpoint_sequence IS NOT NULL AND activated_at IS NOT NULL AND voided_at IS NULL) OR
         (status = 'voided' AND activated_at IS NULL AND voided_at IS NOT NULL))
);

CREATE TABLE weave_fanout_group (
  workspace_id TEXT NOT NULL,
  group_id TEXT NOT NULL,
  intent_id TEXT NOT NULL,
  mode TEXT NOT NULL CHECK (mode IN ('workflow_resume','free_collab_synthesis')),
  status TEXT NOT NULL CHECK (status IN ('pending_activation','active','decided','resumed','closed')),
  policy JSONB NOT NULL CHECK (jsonb_typeof(policy) = 'object'),
  generation TEXT NOT NULL,
  group_completion_id TEXT,
  decision TEXT CHECK (decision IS NULL OR decision IN ('succeeded','failed')),
  join_result JSONB CHECK (join_result IS NULL OR jsonb_typeof(join_result) = 'object'),
  decided_at TIMESTAMPTZ,
  resume_claim_id UUID,
  resume_claim_state TEXT CHECK (resume_claim_state IN ('claimed','admitted','advanced')),
  resume_claim_previous_attempt_generation BIGINT,
  resume_claim_previous_attempt_id UUID,
  resume_claim_new_attempt_generation BIGINT,
  resume_claim_new_attempt_id UUID,
  resume_claimed_group_completion_id TEXT,
  resume_claimed_at TIMESTAMPTZ,
  resume_receipt_id TEXT,
  resume_receipt JSONB CHECK (resume_receipt IS NULL OR jsonb_typeof(resume_receipt) = 'object'),
  resume_advanced_at TIMESTAMPTZ,
  resumed_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (workspace_id, group_id),
  UNIQUE (workspace_id, intent_id),
  UNIQUE (workspace_id, group_completion_id),
  UNIQUE (workspace_id, resume_claim_id),
  FOREIGN KEY (workspace_id, intent_id) REFERENCES weave_fanout_intent(workspace_id, intent_id),
  CHECK ((status IN ('pending_activation','active') AND group_completion_id IS NULL AND join_result IS NULL) OR
         (status IN ('decided','resumed','closed') AND group_completion_id IS NOT NULL AND join_result IS NOT NULL AND decided_at IS NOT NULL)),
  CHECK ((resume_claim_state IS NULL AND resume_claim_id IS NULL
          AND resume_claim_previous_attempt_generation IS NULL AND resume_claim_previous_attempt_id IS NULL
          AND resume_claim_new_attempt_generation IS NULL AND resume_claim_new_attempt_id IS NULL
          AND resume_claimed_group_completion_id IS NULL
          AND resume_claimed_at IS NULL AND resume_receipt_id IS NULL AND resume_receipt IS NULL
          AND resume_advanced_at IS NULL)
      OR (resume_claim_state = 'claimed' AND resume_claim_id IS NOT NULL
          AND resume_claim_previous_attempt_generation IS NOT NULL AND resume_claim_previous_attempt_id IS NOT NULL
          AND resume_claimed_group_completion_id = group_completion_id AND resume_claimed_at IS NOT NULL
          AND ((resume_claim_new_attempt_generation IS NULL AND resume_claim_new_attempt_id IS NULL)
            OR (resume_claim_new_attempt_generation = resume_claim_previous_attempt_generation + 1
                AND resume_claim_new_attempt_id IS NOT NULL))
          AND resume_receipt_id IS NULL AND resume_receipt IS NULL AND resume_advanced_at IS NULL)
      OR (resume_claim_state = 'admitted' AND resume_claim_id IS NOT NULL
          AND resume_claim_previous_attempt_generation IS NOT NULL AND resume_claim_previous_attempt_id IS NOT NULL
          AND resume_claim_new_attempt_generation = resume_claim_previous_attempt_generation + 1
          AND resume_claim_new_attempt_id IS NOT NULL
          AND resume_claimed_group_completion_id = group_completion_id AND resume_claimed_at IS NOT NULL
          AND resume_receipt_id IS NULL AND resume_receipt IS NULL AND resume_advanced_at IS NULL)
      OR (resume_claim_state = 'advanced' AND resume_claim_id IS NOT NULL
          AND resume_claim_previous_attempt_generation IS NOT NULL AND resume_claim_previous_attempt_id IS NOT NULL
          AND resume_claim_new_attempt_generation = resume_claim_previous_attempt_generation + 1
          AND resume_claim_new_attempt_id IS NOT NULL
          AND resume_claimed_group_completion_id = group_completion_id AND resume_claimed_at IS NOT NULL
          AND resume_receipt_id IS NOT NULL AND resume_receipt IS NOT NULL AND resume_advanced_at IS NOT NULL)),
  UNIQUE (workspace_id, resume_receipt_id)
);

CREATE TABLE weave_fanout_leg (
  workspace_id TEXT NOT NULL,
  group_id TEXT NOT NULL,
  leg_id TEXT NOT NULL,
  branch_id TEXT NOT NULL,
  branch_ordinal INTEGER NOT NULL CHECK (branch_ordinal >= 0),
  generation TEXT NOT NULL,
  frozen_bundle_ref JSONB NOT NULL CHECK (jsonb_typeof(frozen_bundle_ref) = 'object'),
  input_ref JSONB NOT NULL CHECK (jsonb_typeof(input_ref) = 'object'),
  may_yield_proof JSONB NOT NULL CHECK (jsonb_typeof(may_yield_proof) = 'object'),
  status TEXT NOT NULL CHECK (status IN
    ('pending_activation','queued','running','cancel_requested','succeeded','failed','timeout','cut','cancelled','abandoned')),
  result JSONB,
  error_code TEXT,
  cancellation_grace_deadline_at TIMESTAMPTZ,
  activated_at TIMESTAMPTZ,
  completed_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (workspace_id, group_id, leg_id),
  UNIQUE (workspace_id, group_id, branch_id),
  UNIQUE (workspace_id, group_id, branch_ordinal),
  FOREIGN KEY (workspace_id, group_id) REFERENCES weave_fanout_group(workspace_id, group_id)
);

CREATE TABLE weave_fanout_audit (
  workspace_id TEXT NOT NULL,
  audit_id TEXT NOT NULL,
  intent_id TEXT,
  group_id TEXT,
  leg_id TEXT,
  generation TEXT NOT NULL,
  event_type TEXT NOT NULL,
  payload JSONB NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
  occurred_at TIMESTAMPTZ NOT NULL,
  recorded_at TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (workspace_id, audit_id)
);

CREATE INDEX weave_fanout_intent_pending_idx
  ON weave_fanout_intent (activation_deadline_at, workspace_id, intent_id)
  WHERE status = 'pending';
CREATE INDEX weave_fanout_group_status_idx
  ON weave_fanout_group (status, workspace_id, group_id);
CREATE INDEX weave_fanout_leg_group_status_idx
  ON weave_fanout_leg (workspace_id, group_id, status, branch_ordinal);
CREATE INDEX weave_fanout_leg_cancel_grace_idx
  ON weave_fanout_leg (cancellation_grace_deadline_at, workspace_id, group_id, leg_id)
  WHERE status = 'cancel_requested';

CREATE FUNCTION weave_fanout_audit_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'weave_fanout_audit is append-only';
END;
$$;

CREATE TRIGGER weave_fanout_audit_append_only_trigger
BEFORE UPDATE OR DELETE ON weave_fanout_audit
FOR EACH ROW EXECUTE FUNCTION weave_fanout_audit_append_only();
