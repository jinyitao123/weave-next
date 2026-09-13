ALTER TABLE weave_capability_invocation_tasks
 ADD COLUMN claim_token TEXT,
 ADD COLUMN deadline_at TIMESTAMPTZ;

CREATE FUNCTION weave_capability_revision_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
 RAISE EXCEPTION 'published capability revisions are immutable' USING ERRCODE='23514';
END;
$$;
CREATE TRIGGER weave_capability_revision_immutable
BEFORE UPDATE OR DELETE ON weave_capability_revisions
FOR EACH ROW EXECUTE FUNCTION weave_capability_revision_immutable();
