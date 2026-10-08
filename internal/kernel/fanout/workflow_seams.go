package fanout

import (
	"context"
	"encoding/json"
	"reflect"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
)

func NewWorkflowError(code ErrorCode, format string, args ...any) error {
	return workflowError(code, format, args...)
}

type LateSynthesisTaskBuilder interface {
	BuildLateSynthesisTask(context.Context, pgx.Tx, GroupCompletion) (*taskqueue.Task, error)
}

type DurableLateSynthesisScheduler struct {
	Transactions coordinatorTransactions
	Store        *Store
	Tasks        workflowTaskStore
	Builder      LateSynthesisTaskBuilder
}

func (s DurableLateSynthesisScheduler) ScheduleFromCompletion(
	ctx context.Context,
	completion GroupCompletion,
) (LateSynthesisResult, error) {
	if completion.Mode != string(FreeCollabSynthesisMode) {
		return LateSynthesisResult{}, workflowError(
			ErrorInvalidRequest, "workflow resume completion cannot schedule synthesis",
		)
	}
	if s.Transactions == nil || s.Store == nil || s.Tasks == nil || s.Builder == nil {
		return LateSynthesisResult{}, workflowError(
			ErrorStoreUnavailable, "late synthesis scheduler dependencies are unavailable",
		)
	}
	if completion.WorkspaceID == "" || completion.GroupID == "" ||
		completion.GroupCompletionID == "" || completion.Generation == "" ||
		!json.Valid(completion.JoinResult) {
		return LateSynthesisResult{}, workflowError(
			ErrorInvalidRequest, "late synthesis completion identity is invalid",
		)
	}

	tx, err := s.Transactions.Begin(ctx)
	if err != nil {
		return LateSynthesisResult{}, wrapStoreUnavailable("begin late synthesis schedule", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	plan, err := s.Store.GetWorkflowResumePlanTx(
		ctx, tx, completion.WorkspaceID, completion.GroupID,
	)
	if err != nil {
		return LateSynthesisResult{}, err
	}
	if plan.Mode != FreeCollabSynthesisMode ||
		plan.GroupCompletionID != completion.GroupCompletionID ||
		plan.Generation != completion.Generation ||
		!equivalentJSON(plan.JoinResult, completion.JoinResult) {
		return LateSynthesisResult{}, workflowError(
			ErrorResumeConflict, "late synthesis durable completion differs",
		)
	}
	result := LateSynthesisResult{
		CompletionID:   completion.GroupCompletionID,
		SessionEventID: completion.GroupCompletionID,
	}
	if plan.Status == WorkflowGroupClosed {
		result.Status = "already_scheduled"
		if err := tx.Commit(ctx); err != nil {
			return LateSynthesisResult{}, wrapStoreUnavailable("commit late synthesis replay", err)
		}
		return result, nil
	}
	if plan.Status != WorkflowGroupDecided {
		return LateSynthesisResult{}, workflowError(
			ErrorInvalidRequest, "late synthesis group is not decided",
		)
	}
	task, err := s.Builder.BuildLateSynthesisTask(ctx, tx, completion)
	if err != nil {
		return LateSynthesisResult{}, err
	}
	if task == nil || task.ID != completion.GroupCompletionID ||
		task.WorkspaceID != completion.WorkspaceID ||
		task.ContextKey != completion.GroupCompletionID ||
		task.Source != "fanout_completion" || task.TaskGroupID != "" {
		return LateSynthesisResult{}, workflowError(
			ErrorInvalidRequest, "late synthesis task identity is invalid",
		)
	}
	if err := s.Tasks.EnqueueTx(ctx, tx, task); err != nil {
		return LateSynthesisResult{}, wrapStoreUnavailable("enqueue late synthesis", err)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE weave_fanout_group
		SET status='closed',updated_at=clock_timestamp()
		WHERE workspace_id=$1 AND group_id=$2
			AND mode='free_collab_synthesis' AND status='decided'
			AND group_completion_id=$3
	`, completion.WorkspaceID, completion.GroupID, completion.GroupCompletionID)
	if err != nil {
		return LateSynthesisResult{}, wrapStoreUnavailable("close scheduled synthesis group", err)
	}
	if tag.RowsAffected() != 1 {
		return LateSynthesisResult{}, workflowError(
			ErrorResumeConflict, "late synthesis group lost decided ownership",
		)
	}
	if err := tx.Commit(ctx); err != nil {
		return LateSynthesisResult{}, wrapStoreUnavailable("commit late synthesis schedule", err)
	}
	result.Status = "scheduled"
	return result, nil
}

func equivalentJSON(left, right json.RawMessage) bool {
	var leftValue, rightValue any
	if json.Unmarshal(left, &leftValue) != nil || json.Unmarshal(right, &rightValue) != nil {
		return false
	}
	return reflect.DeepEqual(leftValue, rightValue)
}

var _ LateSynthesisScheduler = DurableLateSynthesisScheduler{}
