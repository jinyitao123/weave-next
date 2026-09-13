package freezer

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

// FrozenAgentReader supplies exact agent versions and a locked roster from
// the product directory. A reader must not substitute mutable latest versions.
// Transactions remain caller-owned; this port cannot commit or mutate assets.
type FrozenAgentReader interface {
	ResolveAgentVersionTx(context.Context, pgx.Tx, string, string, *int64) (*registry.AgentRecord, error)
	ResolveTeamWorkersForShareTx(context.Context, pgx.Tx, string, string) ([]registry.TeamWorker, error)
}

// PublicationAgentReader supplies the source facts frozen by publication.
// Execution consumes the frozen artifact instead of consulting this directory.
type PublicationAgentReader interface {
	FrozenAgentReader
	ResolvePublicationTeamTx(context.Context, pgx.Tx, string, string) (*registry.PublicationTeamRead, error)
}
