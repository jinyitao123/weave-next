package registry

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// FrozenAgentReader supplies exact agent versions and a locked roster from
// the product directory. A reader must not substitute mutable latest versions.
// Transactions remain caller-owned; this port cannot commit or mutate assets.
type FrozenAgentReader interface {
	ResolveAgentVersionTx(context.Context, pgx.Tx, string, string, *int64) (*AgentRecord, error)
	ResolveTeamWorkersForShareTx(context.Context, pgx.Tx, string, string) ([]TeamWorker, error)
}

// PublicationAgentReader supplies the source facts frozen by publication.
// Execution consumes the frozen artifact instead of consulting this directory.
type PublicationAgentReader interface {
	FrozenAgentReader
	ResolvePublicationTeamTx(context.Context, pgx.Tx, string, string) (*PublicationTeamRead, error)
}
