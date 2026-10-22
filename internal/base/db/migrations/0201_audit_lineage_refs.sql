-- Terminal lineage repair needs one run's lineage, but finding it meant
-- reading every terminal record in the tenant's audit namespace on each run
-- termination. These tables index, per audit record, every run id it names,
-- so a repair reads its lineage closure instead.
--
-- The index over-approximates on purpose: a record lists each string under
-- any key that matches run_id, parent_run_id or aggregation_parent_run_id
-- ignoring case and delimiters, because the Go decoder matches field names
-- case-insensitively; and a value PostgreSQL cannot read as JSON is listed as
-- unparsed and joins every closure. Extra records cannot change an assembled
-- lineage, so the closure read stays equivalent to the full scan.
CREATE TABLE weave_audit_lineage_refs (
  namespace TEXT NOT NULL,
  key TEXT NOT NULL,
  ref TEXT NOT NULL,
  PRIMARY KEY (namespace, key, ref)
);
CREATE INDEX weave_audit_lineage_refs_ref_idx ON weave_audit_lineage_refs (namespace, ref);
CREATE TABLE weave_audit_lineage_unparsed (
  namespace TEXT NOT NULL,
  key TEXT NOT NULL,
  PRIMARY KEY (namespace, key)
);

-- NULL means the value is not JSON PostgreSQL can read.
CREATE FUNCTION weave_audit_lineage_ref_values(value bytea) RETURNS text[]
LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE
  doc jsonb;
  refs text[];
BEGIN
  BEGIN
    doc := convert_from(value, 'UTF8')::jsonb;
  EXCEPTION WHEN others THEN
    RETURN NULL;
  END;
  IF jsonb_typeof(doc) <> 'object' THEN
    RETURN '{}';
  END IF;
  SELECT coalesce(array_agg(DISTINCT field.value #>> '{}'), '{}') INTO refs
    FROM jsonb_each(doc) AS field
   WHERE jsonb_typeof(field.value) = 'string'
     AND translate(lower(field.key), '_-', '') IN ('runid', 'parentrunid', 'aggregationparentrunid');
  RETURN refs;
END $$;

CREATE FUNCTION weave_audit_lineage_index() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  refs text[];
BEGIN
  IF TG_OP = 'TRUNCATE' THEN
    TRUNCATE weave_audit_lineage_refs, weave_audit_lineage_unparsed;
    RETURN NULL;
  END IF;
  IF TG_OP IN ('UPDATE', 'DELETE') AND left(OLD.namespace, 6) = 'audit:' THEN
    DELETE FROM weave_audit_lineage_refs WHERE namespace = OLD.namespace AND key = OLD.key;
    DELETE FROM weave_audit_lineage_unparsed WHERE namespace = OLD.namespace AND key = OLD.key;
  END IF;
  IF TG_OP IN ('INSERT', 'UPDATE') AND left(NEW.namespace, 6) = 'audit:' THEN
    DELETE FROM weave_audit_lineage_refs WHERE namespace = NEW.namespace AND key = NEW.key;
    DELETE FROM weave_audit_lineage_unparsed WHERE namespace = NEW.namespace AND key = NEW.key;
    refs := weave_audit_lineage_ref_values(NEW.value);
    IF refs IS NULL THEN
      INSERT INTO weave_audit_lineage_unparsed (namespace, key) VALUES (NEW.namespace, NEW.key);
    ELSE
      INSERT INTO weave_audit_lineage_refs (namespace, key, ref)
      SELECT NEW.namespace, NEW.key, ref FROM unnest(refs) AS ref;
    END IF;
  END IF;
  RETURN NULL;
END $$;

-- The trigger exists before the backfill, and creating it locks out writers
-- until this migration commits, so no audit write is missed in between.
CREATE TRIGGER weave_audit_lineage_index
  AFTER INSERT OR UPDATE OR DELETE ON loom_store
  FOR EACH ROW EXECUTE FUNCTION weave_audit_lineage_index();
CREATE TRIGGER weave_audit_lineage_truncate
  AFTER TRUNCATE ON loom_store
  FOR EACH STATEMENT EXECUTE FUNCTION weave_audit_lineage_index();

INSERT INTO weave_audit_lineage_refs (namespace, key, ref)
SELECT stored.namespace, stored.key, ref
  FROM loom_store AS stored, unnest(weave_audit_lineage_ref_values(stored.value)) AS ref
 WHERE left(stored.namespace, 6) = 'audit:';
INSERT INTO weave_audit_lineage_unparsed (namespace, key)
SELECT namespace, key FROM loom_store
 WHERE left(namespace, 6) = 'audit:' AND weave_audit_lineage_ref_values(value) IS NULL;
