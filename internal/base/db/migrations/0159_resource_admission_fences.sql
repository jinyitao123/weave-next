-- Kernel owns authorization generations and their immutable operation receipts.
CREATE TABLE weave_resource_admission_fences (
 workspace_id TEXT NOT NULL CHECK(workspace_id<>''), resource_kind TEXT NOT NULL,
 resource_id TEXT NOT NULL CHECK(resource_id<>''), resource_version TEXT NOT NULL CHECK(resource_version<>''),
 epoch BIGINT NOT NULL DEFAULT 0 CHECK(epoch>=0), blocked BOOLEAN NOT NULL DEFAULT false,
 PRIMARY KEY(workspace_id,resource_kind,resource_id,resource_version),
 CHECK(resource_kind IN ('actor_user','actor_service','workspace_member','team','team_worker','workflow','credential','credential_resource'))
);
CREATE FUNCTION weave_resource_admission_fence_guard() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 IF TG_OP='DELETE' OR (NEW.workspace_id,NEW.resource_kind,NEW.resource_id,NEW.resource_version) IS DISTINCT FROM (OLD.workspace_id,OLD.resource_kind,OLD.resource_id,OLD.resource_version)
   OR NEW.epoch<>OLD.epoch+1 THEN RAISE EXCEPTION 'resource admission generation must advance' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END; $$;
CREATE TRIGGER weave_resource_admission_fence_guard BEFORE UPDATE OR DELETE ON weave_resource_admission_fences FOR EACH ROW EXECUTE FUNCTION weave_resource_admission_fence_guard();
CREATE TABLE weave_resource_admission_operations (
 workspace_id TEXT NOT NULL, operation_id TEXT NOT NULL, actor_subject JSONB NOT NULL,
 request_digest TEXT NOT NULL CHECK(request_digest ~ '^[0-9a-f]{64}$'), receipt JSONB NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(), PRIMARY KEY(workspace_id,operation_id),
 CHECK(workspace_id<>'' AND operation_id<>''),
 CHECK(jsonb_typeof(actor_subject)='object' AND COALESCE(actor_subject->>'workspace_id','')=workspace_id
 AND ((COALESCE(actor_subject->>'user_id','')<>'' AND NOT(actor_subject ? 'service_id')) OR (COALESCE(actor_subject->>'service_id','')<>'' AND NOT(actor_subject ? 'user_id')))),
 CHECK(jsonb_typeof(receipt)='object')
);
CREATE FUNCTION weave_resource_admission_operation_guard() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 RAISE EXCEPTION 'resource admission operation is immutable' USING ERRCODE='23514';
END; $$;
CREATE TRIGGER weave_resource_admission_operation_guard BEFORE UPDATE OR DELETE ON weave_resource_admission_operations FOR EACH ROW EXECUTE FUNCTION weave_resource_admission_operation_guard();
