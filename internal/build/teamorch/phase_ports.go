package teamorch

import (
	"context"
	"errors"

	"github.com/jinyitao123/weave/internal/build/teambuild"
)

// ErrBlueprintPatchPlannerBudgetExhausted is returned before starting the
// planner when no budget balance remains, or after charging the planner when
// that final charge reaches/exceeds a frozen limit. The controller maps both
// cases to the stable budget_exhausted terminal class and never appends a
// Blueprint revision.
var ErrBlueprintPatchPlannerBudgetExhausted = errors.New("blueprint patch planner budget exhausted")

// ErrCompilerPublishBudgetExhausted reports a G5 rejection that has already
// committed publishing -> blocked atomically. Callers must preserve the
// budget failure class instead of wrapping it as retryable infrastructure.
var ErrCompilerPublishBudgetExhausted = errors.New("compiler publish budget exhausted")

// BuildRunPort is the builder's durable decision boundary. Atomic commands
// keep evaluation reports and round decisions together without exposing transactions.
type BuildRunPort interface {
	CompilerOperationStore
	ListRounds(context.Context, string, string) ([]teambuild.Round, error)
	GetRoundReport(context.Context, string, string, int) (teambuild.RoundReport, error)
	SaveRoundReport(context.Context, string, string, teambuild.EvaluationReport) (teambuild.RoundReport, error)
	RecordEvaluatedRound(context.Context, string, string, teambuild.EvaluationReport, teambuild.RoundResult) (teambuild.Round, error)
	TransitionStatus(context.Context, string, string, string, string, string, string) (teambuild.TeamBuildRun, error)
	EvaluateBudget(context.Context, string, string, int, teambuild.Budget, teambuild.Budget) (teambuild.BudgetDecision, error)
	AppendCompilerRevisionFromPatch(context.Context, string, string, teambuild.CompilerRevisionAppend) (teambuild.BlueprintRevision, error)
}
