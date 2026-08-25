CREATE OR REPLACE FUNCTION weave_runtimes_enforce_functional_revision()
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

  functional_changed :=
    NEW.engines IS DISTINCT FROM OLD.engines
    OR NEW.engine_capabilities IS DISTINCT FROM OLD.engine_capabilities
    OR NEW.total_slots IS DISTINCT FROM OLD.total_slots
    OR NEW.pool_id IS DISTINCT FROM OLD.pool_id;
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
