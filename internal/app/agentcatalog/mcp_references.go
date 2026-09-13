package agentcatalog

import (
	"context"
	"fmt"
)

// MCPReferenceCounts is the product directory projection shown beside tool
// resources. Runtime resolution does not need this mutable membership data.
func (r *AgentRegistry) MCPReferenceCounts(ctx context.Context, workspaceID string, serverIDs []string) (map[string]int, error) {
	if r == nil || r.pool == nil {
		return nil, fmt.Errorf("agent directory is unavailable")
	}
	counts := make(map[string]int, len(serverIDs))
	if len(serverIDs) == 0 {
		return counts, nil
	}
	rows, err := r.pool.Query(ctx, `
 SELECT target.server_id, COUNT(agent.id)
 FROM (SELECT DISTINCT unnest($2::text[]) AS server_id) AS target
 LEFT JOIN weave_agents AS agent
   ON agent.workspace_id=$1 AND agent.deleted=false
  AND COALESCE(agent.spec->'mcp_servers','[]'::jsonb)
      @> jsonb_build_array(jsonb_build_object('server_id',target.server_id))
 GROUP BY target.server_id
 `, workspaceID, serverIDs)
	if err != nil {
		return nil, fmt.Errorf("read tool resource memberships: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var count int
		if err := rows.Scan(&id, &count); err != nil {
			return nil, err
		}
		counts[id] = count
	}
	return counts, rows.Err()
}
