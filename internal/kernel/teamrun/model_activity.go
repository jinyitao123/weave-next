package teamrun

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

// Model observations extend the existing run activity ledger. They describe
// local execution boundaries, never provider delivery or a second run state.
func (r *WorkflowSerialRuntime) modelObserver(run TeamRun) workflow.ModelCallObserver {
	if r.Activities == nil {
		return nil
	}
	return func(ctx context.Context, event workflow.ModelCallEvent) {
		scope, ok := ctx.Value(runtimeActivityScopeKey{}).(runtimeActivityScope)
		if !ok || scope.NodeID == "" || scope.MemberID == "" || event.CallID == "" {
			return
		}
		switch event.Phase {
		case "started", "request_written", "first_response_byte", "first_chunk", "completed", "failed", "cancelled":
		default:
			return
		}
		detail, err := json.Marshal(map[string]any{
			"model_call_id": event.CallID, "model": event.Model,
			"phase": event.Phase, "elapsed_ms": event.ElapsedMS,
			"error_class": event.ErrorClass,
		})
		if err != nil {
			return
		}
		// The model may just have been cancelled. Its final observation must
		// survive that cancellation, with a finite persistence budget.
		recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		if err := r.Activities.Record(recordCtx, ActivityEvent{
			WorkspaceID: run.WorkspaceID, RunID: run.RunID, Kind: "model_" + string(event.Phase),
			NodeID: scope.NodeID, MemberID: scope.MemberID, MemberVersion: scope.MemberVersion,
			Detail: detail, OccurredAt: time.Now().UTC(),
		}); err != nil {
			slog.Warn("team run model activity record failed", "run_id", run.RunID,
				"node_id", scope.NodeID, "phase", event.Phase, "error", err)
		}
	}
}
