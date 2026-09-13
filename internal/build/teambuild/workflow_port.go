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
	ListByTeam(context.Context, string, string) ([]workflow.TeamWorkflow, error)
	ListVersionsByWorkflows(context.Context, string, []string) ([]workflow.TeamWorkflowVersion, error)
	VerifyBaselineWorkflowsTx(context.Context, pgx.Tx, string, []string, []workflow.TeamWorkflow, []workflow.TeamWorkflowVersion) error
}
