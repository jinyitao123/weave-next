package capabilities

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/storeext"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
)

// WorkflowInvocationUsage projects the root TeamRun audit without aggregating
// child audits a second time. Nil fields mean the audit does not prove that
// dimension; they must not be serialized as a measured zero.
type WorkflowInvocationUsage struct {
	PhysicalUsage      *execution.TerminalUsage `json:"physical_usage,omitempty"`
	UnreportedAttempts *int                     `json:"unreported_attempts,omitempty"`
	Complete           bool                     `json:"complete"`
}

// readWorkflowInvocationUsage reads the one authoritative usage projection:
// the V3 audit entry for the root TeamRun. Its SubtreeTotal already includes
// attributable descendants, so child/fanout audit rows and terminal markers
// are deliberately not summed here.
func readWorkflowInvocationUsage(
	ctx context.Context,
	pool *pgxpool.Pool,
	record workflowInvocationRecord,
) (WorkflowInvocationUsage, error) {
	if pool == nil {
		return WorkflowInvocationUsage{}, errors.New("workflow invocation usage pool is unavailable")
	}
	if record.WorkspaceID == "" || record.RunID == "" || record.WorkflowID == "" || record.WorkflowVersion < 1 {
		return WorkflowInvocationUsage{}, errors.New("workflow invocation usage identity is incomplete")
	}
	data, present, err := storeext.New(pool).ReadValue(ctx, "audit:"+record.WorkspaceID, record.RunID)
	if err != nil {
		return WorkflowInvocationUsage{}, err
	}
	if !present {
		return WorkflowInvocationUsage{}, nil
	}
	inspection := loomruntime.InspectTerminalRecord(true, data)
	if inspection.Classification != loomruntime.TerminalRecordValid || inspection.Entry == nil {
		return WorkflowInvocationUsage{}, nil
	}
	entry := inspection.Entry
	if entry.RunID != record.RunID || entry.Tenant != record.WorkspaceID ||
		entry.AttributionScope != loomruntime.TerminalAttributionFixedWorkflow ||
		entry.WorkflowID == nil || *entry.WorkflowID != record.WorkflowID ||
		entry.WorkflowVersion == nil || *entry.WorkflowVersion != record.WorkflowVersion {
		return WorkflowInvocationUsage{}, nil
	}

	usage := entry.SubtreeTotal
	projected := WorkflowInvocationUsage{PhysicalUsage: &usage}
	if entry.UsageComplete != nil && !*entry.UsageComplete {
		return projected, nil
	}
	zero := 0
	projected.UnreportedAttempts = &zero
	projected.Complete = true
	return projected, nil
}
