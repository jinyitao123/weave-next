LOCK TABLE weave_runtimes, weave_task_queue IN ACCESS EXCLUSIVE MODE;

-- Refuse to guess how an unsupported legacy engine should be represented.
-- The migration transaction, including its ledger row, must roll back instead.
DO $$
DECLARE
  invalid_runtime_id TEXT;
BEGIN
  SELECT runtime.id
  INTO invalid_runtime_id
  FROM weave_runtimes AS runtime
  WHERE CASE
    WHEN jsonb_typeof(runtime.engines) IS DISTINCT FROM 'array' THEN true
    ELSE EXISTS (
      SELECT 1
      FROM jsonb_array_elements(runtime.engines) AS engine(value)
      WHERE jsonb_typeof(engine.value) IS DISTINCT FROM 'string'
         OR engine.value #>> '{}' NOT IN ('claude', 'codex', 'opencode')
    )
  END
  LIMIT 1;

  IF FOUND THEN
    RAISE EXCEPTION 'runtime % has invalid legacy engines', invalid_runtime_id
      USING ERRCODE = '23514';
  END IF;
END;
$$;

UPDATE weave_runtimes AS runtime
SET engines = (
  SELECT COALESCE(
    jsonb_agg(to_jsonb(distinct_engine.engine) ORDER BY distinct_engine.engine),
    '[]'::jsonb
  )
  FROM (
    SELECT DISTINCT engine.value #>> '{}' AS engine
    FROM jsonb_array_elements(runtime.engines) AS engine(value)
  ) AS distinct_engine
);

ALTER TABLE weave_runtimes
  ADD COLUMN functional_revision BIGINT NOT NULL DEFAULT 1,
  ADD COLUMN enabled BOOLEAN NOT NULL DEFAULT true,
  ADD COLUMN revoked_at TIMESTAMPTZ,
  ADD COLUMN deleted_at TIMESTAMPTZ,
  ADD CONSTRAINT weave_runtimes_functional_revision_check
    CHECK (functional_revision BETWEEN 1 AND 9007199254740991),
  ADD CONSTRAINT weave_runtimes_engines_check CHECK (
    jsonb_typeof(engines) IS NOT DISTINCT FROM 'array'
    AND NOT jsonb_path_exists(
      engines,
      '$[*] ? (@.type() != "string")'
    )
    AND NOT jsonb_path_exists(
      engines,
      '$[*] ? (@ != "claude" && @ != "codex" && @ != "opencode")'
    )
  ),
  ADD CONSTRAINT weave_runtimes_workspace_id_id_key
    UNIQUE (workspace_id, id),
  ADD CONSTRAINT weave_runtimes_token_hash_key
    UNIQUE (token_hash);

CREATE FUNCTION weave_runtimes_enforce_functional_revision()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
  canonical_engines JSONB;
  functional_changed BOOLEAN;
BEGIN
  IF jsonb_typeof(NEW.engines) IS DISTINCT FROM 'array' THEN
    RAISE EXCEPTION 'runtime engines must be an array of supported engine names'
      USING
        ERRCODE = '23514',
        CONSTRAINT = 'weave_runtimes_engines_check';
  END IF;

  IF EXISTS (
    SELECT 1
    FROM jsonb_array_elements(NEW.engines) AS engine(value)
    WHERE jsonb_typeof(engine.value) IS DISTINCT FROM 'string'
       OR engine.value #>> '{}' NOT IN ('claude', 'codex', 'opencode')
  ) THEN
    RAISE EXCEPTION 'runtime engines must be an array of supported engine names'
      USING
        ERRCODE = '23514',
        CONSTRAINT = 'weave_runtimes_engines_check';
  END IF;

  SELECT COALESCE(
    jsonb_agg(to_jsonb(distinct_engine.engine) ORDER BY distinct_engine.engine),
    '[]'::jsonb
  )
  INTO canonical_engines
  FROM (
    SELECT DISTINCT engine.value #>> '{}' AS engine
    FROM jsonb_array_elements(NEW.engines) AS engine(value)
  ) AS distinct_engine;
  NEW.engines := canonical_engines;

  IF TG_OP = 'INSERT' THEN
    RETURN NEW;
  END IF;

  IF NEW.workspace_id IS DISTINCT FROM OLD.workspace_id
     OR NEW.id IS DISTINCT FROM OLD.id THEN
    RAISE EXCEPTION 'runtime identity is immutable'
      USING ERRCODE = '23514';
  END IF;

  IF OLD.deleted_at IS NOT NULL THEN
    RAISE EXCEPTION 'deleted runtime cannot be modified'
      USING ERRCODE = '23514';
  END IF;

  IF OLD.enabled = false AND NEW.enabled = true THEN
    RAISE EXCEPTION 'disabled runtime cannot be re-enabled'
      USING ERRCODE = '23514';
  END IF;

  IF OLD.revoked_at IS NOT NULL
     AND NEW.revoked_at IS DISTINCT FROM OLD.revoked_at THEN
    RAISE EXCEPTION 'runtime revocation is immutable once set'
      USING ERRCODE = '23514';
  END IF;

  functional_changed := NEW.engines IS DISTINCT FROM OLD.engines;
  IF functional_changed THEN
    IF OLD.functional_revision >= 9007199254740991
       OR NEW.functional_revision IS DISTINCT FROM OLD.functional_revision + 1 THEN
      RAISE EXCEPTION 'runtime functional changes require exactly one revision advance'
        USING ERRCODE = '23514';
    END IF;
  ELSIF NEW.functional_revision IS DISTINCT FROM OLD.functional_revision THEN
    RAISE EXCEPTION 'runtime functional revision cannot change without functional changes'
      USING ERRCODE = '23514';
  END IF;

  RETURN NEW;
END;
$$;

CREATE TRIGGER weave_runtimes_functional_revision_guard
BEFORE INSERT OR UPDATE ON weave_runtimes
FOR EACH ROW
EXECUTE FUNCTION weave_runtimes_enforce_functional_revision();

DROP INDEX idx_task_queue_claim;

-- Task 7A only prepares this index for the planned Task 7C exact
-- workspace-qualified engine claim query shape.
CREATE INDEX idx_task_queue_claim
  ON weave_task_queue (
    status,
    kind,
    workspace_id,
    runtime_id,
    identity_kind,
    priority DESC,
    created_at
  )
  WHERE status = 'queued' AND kind = 'engine_exec';

CREATE INDEX idx_task_queue_chat_claim
  ON weave_task_queue (
    status,
    kind,
    identity_kind,
    priority DESC,
    created_at
  )
  WHERE status = 'queued' AND kind <> 'engine_exec';
