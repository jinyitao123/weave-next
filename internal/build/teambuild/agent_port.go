package teambuild

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

// AgentBaselineReader supplies product-owned version and roster facts without
// allowing Builder to construct or mutate the product directory.
type AgentBaselineReader interface {
	ResolveTeamWorkersForShareTx(context.Context, pgx.Tx, string, string) ([]registry.TeamWorker, error)
	ResolveAgentHeadVersionsTx(context.Context, pgx.Tx, string, []string) (map[string]int64, error)
	ResolveAgentVersionContentTx(context.Context, pgx.Tx, string, string, int64) (*registry.AgentVersionContent, error)
	ListTeamWorkersByTeam(context.Context, string, string) ([]registry.TeamWorker, error)
}
