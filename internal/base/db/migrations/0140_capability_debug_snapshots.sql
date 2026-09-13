ALTER TABLE weave_capability_invocations
 ADD COLUMN run_kind TEXT NOT NULL DEFAULT 'published' CHECK(run_kind IN ('published','debug')),
 ADD COLUMN definition_hash TEXT NOT NULL DEFAULT '';
ALTER TABLE weave_capability_invocation_tasks
 ADD COLUMN run_kind TEXT NOT NULL DEFAULT 'published' CHECK(run_kind IN ('published','debug'));
ALTER TABLE weave_capability_invocations DROP CONSTRAINT weave_capability_invocations_revision_check;
ALTER TABLE weave_capability_invocations ADD CONSTRAINT weave_capability_invocations_revision_check
 CHECK ((run_kind='published' AND revision>0) OR (run_kind='debug' AND revision=0));
ALTER TABLE weave_capability_invocation_tasks DROP CONSTRAINT weave_capability_invocation_tasks_revision_check;
ALTER TABLE weave_capability_invocation_tasks ADD CONSTRAINT weave_capability_invocation_tasks_revision_check
 CHECK ((run_kind='published' AND revision>0) OR (run_kind='debug' AND revision=0));

UPDATE weave_capability_invocations i SET definition_hash=r.definition_hash
FROM weave_capability_revisions r
WHERE i.workspace_id=r.workspace_id AND i.capability_id=r.capability_id AND i.revision=r.revision;

CREATE TABLE weave_capability_debug_snapshots (
 workspace_id TEXT NOT NULL,
 invocation_id TEXT NOT NULL,
 definition_hash TEXT NOT NULL,
 definition JSONB NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 PRIMARY KEY(workspace_id,invocation_id),
 FOREIGN KEY(workspace_id,invocation_id) REFERENCES weave_capability_invocations(workspace_id,invocation_id)
);
CREATE FUNCTION weave_capability_debug_snapshot_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 RAISE EXCEPTION 'debug definition snapshots are immutable' USING ERRCODE='23514';
END;
$$;
CREATE TRIGGER weave_capability_debug_snapshot_immutable
BEFORE UPDATE OR DELETE ON weave_capability_debug_snapshots
FOR EACH ROW EXECUTE FUNCTION weave_capability_debug_snapshot_immutable();
