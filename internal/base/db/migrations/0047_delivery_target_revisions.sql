CREATE TABLE weave_delivery_targets (
  workspace_id TEXT NOT NULL,
  id TEXT NOT NULL,
  latest_revision BIGINT NOT NULL DEFAULT 0,
  headers_cipher TEXT NOT NULL,
  enabled BOOLEAN NOT NULL DEFAULT true,
  revoked_at TIMESTAMPTZ,
  deleted_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, id),
  CONSTRAINT weave_delivery_targets_workspace_fk
    FOREIGN KEY (workspace_id)
    REFERENCES weave_workspaces (id) ON DELETE RESTRICT,
  CONSTRAINT weave_delivery_targets_id_check
    CHECK (id <> ''),
  CONSTRAINT weave_delivery_targets_latest_revision_check
    CHECK (latest_revision BETWEEN 0 AND 9007199254740991),
  CONSTRAINT weave_delivery_targets_headers_cipher_check
    CHECK (headers_cipher <> '')
);

CREATE FUNCTION weave_delivery_target_canonicalize_headers()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
  header_name TEXT;
  folded_name TEXT;
  folded_names TEXT[] := ARRAY[]::TEXT[];
BEGIN
  IF NEW.header_names IS NULL THEN
    RAISE EXCEPTION 'delivery target header names must be a non-null array'
      USING ERRCODE = '23514';
  END IF;
  IF COALESCE(array_ndims(NEW.header_names), 1) <> 1 THEN
    RAISE EXCEPTION 'delivery target header names must be a one-dimensional array'
      USING ERRCODE = '23514';
  END IF;

  FOREACH header_name IN ARRAY NEW.header_names
  LOOP
    IF header_name IS NULL
       OR header_name COLLATE "C" !~ '^[-!#$%&''*+.^_`|~0-9A-Za-z]+$' THEN
      RAISE EXCEPTION 'delivery target header name is not a valid HTTP token'
        USING ERRCODE = '23514';
    END IF;

    folded_name := translate(
      header_name,
      'ABCDEFGHIJKLMNOPQRSTUVWXYZ',
      'abcdefghijklmnopqrstuvwxyz'
    );
    IF folded_name = ANY(folded_names) THEN
      RAISE EXCEPTION 'delivery target header names must be unique after lowercase normalization'
        USING ERRCODE = '23514';
    END IF;
    folded_names := array_append(folded_names, folded_name);
  END LOOP;

  SELECT COALESCE(
    array_agg(normalized.name ORDER BY normalized.name COLLATE "C"),
    ARRAY[]::TEXT[]
  )
  INTO NEW.header_names
  FROM unnest(folded_names) AS normalized(name);

  RETURN NEW;
END;
$$;

CREATE TABLE weave_delivery_target_revisions (
  workspace_id TEXT NOT NULL,
  target_id TEXT NOT NULL,
  revision BIGINT NOT NULL,
  kind TEXT NOT NULL,
  transport TEXT NOT NULL DEFAULT 'http',
  url TEXT NOT NULL,
  method TEXT NOT NULL DEFAULT 'POST',
  content_type TEXT NOT NULL DEFAULT 'application/json',
  timeout_seconds INTEGER NOT NULL,
  header_names TEXT[] NOT NULL DEFAULT ARRAY[]::TEXT[],
  content_hash TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, target_id, revision),
  CONSTRAINT weave_delivery_target_revisions_target_fk
    FOREIGN KEY (workspace_id, target_id)
    REFERENCES weave_delivery_targets (workspace_id, id) ON DELETE RESTRICT,
  CONSTRAINT weave_delivery_target_revisions_revision_check
    CHECK (revision BETWEEN 1 AND 9007199254740991),
  CONSTRAINT weave_delivery_target_revisions_kind_check
    CHECK (kind IN ('callback', 'target')),
  CONSTRAINT weave_delivery_target_revisions_transport_check
    CHECK (transport = 'http'),
  CONSTRAINT weave_delivery_target_revisions_url_check CHECK (
    url COLLATE "C" ~ '^https?://(\[[0-9A-Za-z:.%_-]+\]|[A-Za-z0-9._~!$&''()*+,;=%-]+)(:[0-9]+)?(/[^[:space:]?#]*)?$'
  ),
  CONSTRAINT weave_delivery_target_revisions_method_check
    CHECK (method = 'POST'),
  CONSTRAINT weave_delivery_target_revisions_content_type_check
    CHECK (content_type = 'application/json'),
  CONSTRAINT weave_delivery_target_revisions_timeout_check
    CHECK (timeout_seconds BETWEEN 1 AND 300),
  CONSTRAINT weave_delivery_target_revisions_content_hash_check
    CHECK (content_hash COLLATE "C" ~ '^[0-9a-f]{64}$')
);

CREATE TRIGGER weave_delivery_target_revisions_canonicalize_headers
BEFORE INSERT ON weave_delivery_target_revisions
FOR EACH ROW
EXECUTE FUNCTION weave_delivery_target_canonicalize_headers();

CREATE FUNCTION weave_delivery_target_revisions_reject_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'delivery target revision records are immutable'
    USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER weave_delivery_target_revisions_immutable
BEFORE UPDATE OR DELETE ON weave_delivery_target_revisions
FOR EACH ROW
EXECUTE FUNCTION weave_delivery_target_revisions_reject_mutation();

CREATE FUNCTION weave_delivery_targets_guard_lifecycle()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'delivery target heads must be soft closed'
      USING ERRCODE = '23514';
  END IF;

  IF NEW.workspace_id IS DISTINCT FROM OLD.workspace_id
     OR NEW.id IS DISTINCT FROM OLD.id THEN
    RAISE EXCEPTION 'delivery target identity is immutable'
      USING ERRCODE = '23514';
  END IF;

  IF OLD.deleted_at IS NOT NULL THEN
    RAISE EXCEPTION 'deleted delivery target cannot be modified'
      USING ERRCODE = '23514';
  END IF;

  IF OLD.enabled = false AND NEW.enabled = true THEN
    RAISE EXCEPTION 'disabled delivery target cannot be re-enabled'
      USING ERRCODE = '23514';
  END IF;

  IF OLD.revoked_at IS NOT NULL
     AND NEW.revoked_at IS DISTINCT FROM OLD.revoked_at THEN
    RAISE EXCEPTION 'delivery target revocation is immutable once set'
      USING ERRCODE = '23514';
  END IF;

  IF NEW.latest_revision IS DISTINCT FROM OLD.latest_revision THEN
    IF OLD.latest_revision >= 9007199254740991
       OR NEW.latest_revision IS DISTINCT FROM OLD.latest_revision + 1 THEN
      RAISE EXCEPTION 'delivery target revision must advance exactly once'
        USING ERRCODE = '23514';
    END IF;
  END IF;

  RETURN NEW;
END;
$$;

CREATE TRIGGER weave_delivery_targets_lifecycle_guard
BEFORE UPDATE OR DELETE ON weave_delivery_targets
FOR EACH ROW
EXECUTE FUNCTION weave_delivery_targets_guard_lifecycle();
