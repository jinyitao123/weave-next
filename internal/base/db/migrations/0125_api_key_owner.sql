ALTER TABLE weave_api_keys
  ADD COLUMN IF NOT EXISTS owner_user_id TEXT REFERENCES weave_users(id);

WITH RECURSIVE creator_chain AS (
  SELECT key.id AS origin_key_id, key.tenant_id, key.created_by, 0 AS depth
  FROM weave_api_keys AS key
  UNION ALL
  SELECT chain.origin_key_id, chain.tenant_id, parent.created_by, chain.depth + 1
  FROM creator_chain AS chain
  JOIN weave_api_keys AS parent
    ON chain.created_by = 'apikey:' || parent.id
   AND parent.tenant_id = chain.tenant_id
  WHERE chain.depth < 32
), resolved_owner AS (
  SELECT DISTINCT ON (chain.origin_key_id)
    chain.origin_key_id, owner.id AS owner_user_id
  FROM creator_chain AS chain
  JOIN weave_users AS owner
    ON owner.id = chain.created_by
   AND owner.tenant_id = chain.tenant_id
  ORDER BY chain.origin_key_id, chain.depth
)
UPDATE weave_api_keys AS key
SET owner_user_id = resolved.owner_user_id
FROM resolved_owner AS resolved
WHERE key.id = resolved.origin_key_id
  AND key.owner_user_id IS NULL;

CREATE INDEX IF NOT EXISTS idx_weave_api_keys_owner
  ON weave_api_keys(tenant_id, owner_user_id);
