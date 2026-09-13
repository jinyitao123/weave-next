-- A member execution edit can materialize several workflow revisions. Persist
-- the complete product plan before the first kernel publication so retries
-- resume the same agent version and workflow drafts.
CREATE TABLE weave_agent_execution_requests (
  workspace_id TEXT NOT NULL,
  request_id TEXT NOT NULL,
  actor_subject JSONB NOT NULL,
  request_digest TEXT NOT NULL,
  agent_name TEXT NOT NULL,
  request JSONB NOT NULL,
  agent_version INTEGER NOT NULL,
  publication_commands JSONB NOT NULL,
  completed_publications JSONB NOT NULL DEFAULT '[]'::jsonb,
  state TEXT NOT NULL DEFAULT 'prepared',
  response JSONB,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, request_id),
  CHECK (workspace_id <> '' AND request_id <> '' AND agent_name <> ''),
  CHECK (request_digest ~ '^[0-9a-f]{64}$'),
  CHECK (jsonb_typeof(actor_subject) = 'object'
    AND COALESCE(actor_subject->>'workspace_id','') = workspace_id
    AND ((COALESCE(actor_subject->>'user_id','') <> '' AND NOT(actor_subject ? 'service_id'))
      OR (COALESCE(actor_subject->>'service_id','') <> '' AND NOT(actor_subject ? 'user_id')))),
  CHECK (jsonb_typeof(request) = 'object'
    AND jsonb_typeof(publication_commands) = 'array'
    AND jsonb_typeof(completed_publications) = 'array'),
  CHECK (agent_version > 0),
  CHECK (state IN ('prepared','completed')),
  CHECK ((state = 'prepared' AND response IS NULL)
    OR (state = 'completed' AND jsonb_typeof(response) = 'object'))
);

CREATE FUNCTION weave_agent_execution_request_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' OR
    (OLD.workspace_id, OLD.request_id, OLD.actor_subject, OLD.request_digest,
     OLD.agent_name, OLD.request, OLD.agent_version, OLD.publication_commands)
    IS DISTINCT FROM
    (NEW.workspace_id, NEW.request_id, NEW.actor_subject, NEW.request_digest,
     NEW.agent_name, NEW.request, NEW.agent_version, NEW.publication_commands) THEN
    RAISE EXCEPTION 'agent execution request identity is immutable' USING ERRCODE='23514';
  END IF;
  IF OLD.state = 'completed' AND NEW IS DISTINCT FROM OLD THEN
    RAISE EXCEPTION 'completed agent execution request is immutable' USING ERRCODE='23514';
  END IF;
  IF OLD.state = 'prepared' AND NEW.state NOT IN ('prepared','completed') THEN
    RAISE EXCEPTION 'agent execution request transition is invalid' USING ERRCODE='23514';
  END IF;
  IF NOT (OLD.completed_publications <@ NEW.completed_publications) THEN
    RAISE EXCEPTION 'completed publications are append-only' USING ERRCODE='23514';
  END IF;
  RETURN NEW;
END; $$;

CREATE TRIGGER weave_agent_execution_requests_guard
  BEFORE UPDATE OR DELETE ON weave_agent_execution_requests
  FOR EACH ROW EXECUTE FUNCTION weave_agent_execution_request_guard();
