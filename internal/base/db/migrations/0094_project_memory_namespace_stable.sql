DO $$
BEGIN
  IF to_regclass('public.loom_memory') IS NOT NULL THEN
    EXECUTE $copy$
      INSERT INTO loom_memory (namespace, content, embedding, metadata, created_at, accessed_at, access_count)
      SELECT
        'memory:' || project.workspace_id || ':' || project.id,
        memory.content,
        memory.embedding,
        memory.metadata,
        memory.created_at,
        memory.accessed_at,
        memory.access_count
      FROM weave_projects AS project
      JOIN loom_memory AS memory
        ON memory.namespace = 'memory:' || project.workspace_id || ':' || project.avatar_id || ':' || project.id
      WHERE NOT EXISTS (
        SELECT 1
        FROM loom_memory AS existing
        WHERE existing.namespace = 'memory:' || project.workspace_id || ':' || project.id
          AND existing.content = memory.content
          AND existing.metadata = memory.metadata
      )
    $copy$;
  END IF;
END;
$$;
