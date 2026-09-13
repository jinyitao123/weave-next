package teambuild

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

// WorkflowBaselineReader reads only the product catalog needed for build authorization.
// Frozen artifacts are supplied separately by the kernel publication reader.
type WorkflowBaselineReader interface {
	Get(context.Context, string, string) (*workflow.TeamWorkflow, error)
	GetVersion(context.Context, string, string, int) (*workflow.TeamWorkflowVersion, error)
	ListByTeam(context.Context, string, string) ([]workflow.TeamWorkflow, error)
	ListVersionsByWorkflows(context.Context, string, []string) ([]workflow.TeamWorkflowVersion, error)
	ResolvePublicationDraftTx(context.Context, pgx.Tx, string, string, int) (*workflow.PublicationDraftRead, error)
}
