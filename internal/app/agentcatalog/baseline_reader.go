package agentcatalog

import (
	"context"

	"github.com/jinyitao123/weave/internal/kernel/registry"
)

// ListTeamWorkersByTeam exposes the product roster through the Builder's
// read-only baseline port; the Builder never constructs a product repository.
func (r *AgentRegistry) ListTeamWorkersByTeam(ctx context.Context, workspaceID, teamID string) ([]registry.TeamWorker, error) {
	return NewTeamWorkerRepository(r.pool).ListByTeam(ctx, workspaceID, teamID)
}
