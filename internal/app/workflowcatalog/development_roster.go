package workflowcatalog

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/kernel/freezer"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

// The immutable candidate roster was validated and stored by the team authoring
// service. Other resource reads retain the regular registry authorization path.
type developmentRosterReader struct {
	freezer.PublicationAgentReader
	team *registry.PublicationTeamRead
}

func (r developmentRosterReader) ResolveTeamWorkersForShareTx(ctx context.Context, tx pgx.Tx, ws, id string) ([]registry.TeamWorker, error) {
	if r.team != nil && r.team.WorkspaceID == ws && r.team.TeamID == id {
		return append([]registry.TeamWorker(nil), r.team.Workers...), nil
	}
	return r.PublicationAgentReader.ResolveTeamWorkersForShareTx(ctx, tx, ws, id)
}
