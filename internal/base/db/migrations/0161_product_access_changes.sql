-- Server owns permission-change intent, product mutation result and resource
-- coordination. Kernel fence epochs/receipts remain owned by 0159.
CREATE TABLE weave_access_change_operations (
 workspace_id TEXT NOT NULL, operation_id TEXT NOT NULL,
 actor_subject JSONB NOT NULL, request_digest TEXT NOT NULL,
 intent JSONB NOT NULL, state TEXT NOT NULL DEFAULT 'prepared',
 block_receipt JSONB, product_result JSONB, grant_receipt JSONB,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 PRIMARY KEY(workspace_id,operation_id),
 CHECK(workspace_id<>'' AND operation_id<>'' AND request_digest ~ '^[0-9a-f]{64}$'),
 CHECK(state IN ('prepared','blocked','applied','completed')),
 CHECK(jsonb_typeof(intent)='object'),
 CHECK(jsonb_typeof(actor_subject)='object' AND COALESCE(actor_subject->>'workspace_id','')=workspace_id
 AND ((COALESCE(actor_subject->>'user_id','')<>'' AND NOT(actor_subject ? 'service_id')) OR (COALESCE(actor_subject->>'service_id','')<>'' AND NOT(actor_subject ? 'user_id')))),
 CHECK(state NOT IN ('applied','completed') OR product_result IS NOT NULL)
);
CREATE TABLE weave_access_change_resources (
 workspace_id TEXT NOT NULL, resource_kind TEXT NOT NULL, resource_id TEXT NOT NULL, resource_version TEXT NOT NULL,
 operation_id TEXT NOT NULL,
 PRIMARY KEY(workspace_id,resource_kind,resource_id,resource_version)
);
CREATE FUNCTION weave_access_change_operation_guard() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 IF TG_OP='DELETE' OR (NEW.workspace_id,NEW.operation_id,NEW.actor_subject,NEW.request_digest,NEW.intent) IS DISTINCT FROM (OLD.workspace_id,OLD.operation_id,OLD.actor_subject,OLD.request_digest,OLD.intent)
 OR (OLD.block_receipt IS NOT NULL AND NEW.block_receipt IS DISTINCT FROM OLD.block_receipt)
 OR (OLD.product_result IS NOT NULL AND NEW.product_result IS DISTINCT FROM OLD.product_result)
 OR (OLD.grant_receipt IS NOT NULL AND NEW.grant_receipt IS DISTINCT FROM OLD.grant_receipt)
 OR (OLD.state='completed' AND NEW IS DISTINCT FROM OLD)
 OR (OLD.state='blocked' AND NEW.state='prepared')
 OR (OLD.state='applied' AND NEW.state NOT IN ('applied','completed'))
 THEN RAISE EXCEPTION 'permission change facts are immutable' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END; $$;
CREATE TRIGGER weave_access_change_operation_guard BEFORE UPDATE OR DELETE ON weave_access_change_operations FOR EACH ROW EXECUTE FUNCTION weave_access_change_operation_guard();
