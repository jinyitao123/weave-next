package mcphost

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/kernel/fanout"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
)

// BuiltinTaskStatusServerURL is the stable server identity used by builtin
// task-status tools.
const BuiltinTaskStatusServerURL = "builtin://task-status"

// TaskStatusWriteTools lists declared-write task status tools rejected by the
// write gate.
var TaskStatusWriteTools = []string{"cancel_task", "retry_failed_leg"}

const (
	taskGroupStatusEnum = "active, resolving, resolved"
	taskLegStatusEnum   = "queued, dispatched, running, completed, failed, cancelled, superseded, cut, timed_out"
)

// myTasksListLimit caps the groups scanned by list_my_tasks, matching the task
// group API default page size.
const myTasksListLimit = 20

// TaskGroupReader reads fan-out group records and pages. *fanout.Store
// satisfies it.
type TaskGroupReader interface {
	Snapshot(ctx context.Context, workspaceID, groupID string) (fanout.Snapshot, error)
	ListGroups(ctx context.Context, filter fanout.GroupListFilter) ([]fanout.GroupListItem, int, error)
}

// TaskLegReader reads the queue tasks of one group.
type TaskLegReader interface {
	ListByGroup(ctx context.Context, workspaceID, groupID string) ([]taskqueue.Task, error)
}

// TaskLegWriter applies gated write operations to group legs. *taskqueue.Store
// satisfies it.
type TaskLegWriter interface {
	TaskLegReader
	CancelGroupLegs(ctx context.Context, workspaceID, groupID string) (int, error)
	RequeueFailedLeg(ctx context.Context, workspaceID, groupID, agent string) (*taskqueue.Task, error)
}

// TaskStatusDispatcher exposes structured task-group status as builtin tools so
// agents read real queue state instead of guessing from conversation text. The
// three read tools are also safe for depth-1 sub-agents; the two write tools
// are declared writes and must be wrapped in a WriteGateDispatcher at the top
// level (never handed to sub-agents).
type TaskStatusDispatcher struct {
	groups      TaskGroupReader
	legs        TaskLegReader
	writes      TaskLegWriter
	workspaceID string
	agent       string
	userID      string
	readOnly    bool
}

// NewTaskStatusDispatcher creates the full five-tool dispatcher. Wrap it in a
// WriteGateDispatcher with TaskStatusWriteTools before registering it.
func NewTaskStatusDispatcher(
	groups TaskGroupReader,
	legs TaskLegWriter,
	workspaceID, agent, userID string,
) *TaskStatusDispatcher {
	return &TaskStatusDispatcher{
		groups: groups, legs: legs, writes: legs,
		workspaceID: workspaceID, agent: agent, userID: userID,
	}
}

// NewReadOnlyTaskStatusDispatcher creates the sub-agent variant exposing only
// the three read tools; write calls are rejected at dispatch as well.
func NewReadOnlyTaskStatusDispatcher(
	groups TaskGroupReader,
	legs TaskLegReader,
	workspaceID, agent, userID string,
) *TaskStatusDispatcher {
	return &TaskStatusDispatcher{
		groups: groups, legs: legs, readOnly: true,
		workspaceID: workspaceID, agent: agent, userID: userID,
	}
}

var taskGroupInputSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"group_id": {"type": "string", "description": "Task group ID (tg_...) as returned by dispatch_parallel or list_my_tasks"}
	},
	"required": ["group_id"]
}`)

var workerResultInputSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"group_id": {"type": "string", "description": "Task group ID (tg_...)"},
		"worker": {"type": "string", "description": "Name of the worker agent whose leg to inspect"}
	},
	"required": ["group_id", "worker"]
}`)

var emptyInputSchema = json.RawMessage(`{"type": "object", "properties": {}}`)

// ListTools returns the task status tools. Read-only mode (sub-agents) exposes
// only the three read tools.
func (d *TaskStatusDispatcher) ListTools(context.Context) ([]contract.ToolDef, error) {
	reads := []contract.ToolDef{
		{
			Name: "get_task_group",
			Description: fmt.Sprintf(
				"Get the structured status of one task group. Returns JSON: "+
					"{group_id, status, avatar_agent, quorum, group_outcome, created_at, deadline_at, resolved_at, "+
					"legs: [{agent, status, run_id, started_at, completed_at, error}]}. "+
					"Group status is always one of: %s. Leg status is always one of: %s. "+
					"Report statuses with these exact values only — never invent other status words.",
				taskGroupStatusEnum, taskLegStatusEnum,
			),
			InputSchema: taskGroupInputSchema,
			ReadOnly:    true,
		},
		{
			Name: "list_my_tasks",
			Description: fmt.Sprintf(
				"List task groups related to you: groups you dispatched as avatar or where you run a leg. "+
					"Returns JSON: {groups: [{group_id, status, avatar_agent, created_at, resolved_at, "+
					"legs: [{agent, status}]}], total}. "+
					"Group status is always one of: %s. Leg status is always one of: %s.",
				taskGroupStatusEnum, taskLegStatusEnum,
			),
			InputSchema: emptyInputSchema,
			ReadOnly:    true,
		},
		{
			Name: "get_worker_result",
			Description: fmt.Sprintf(
				"Get the result or error of one worker's leg in a task group. Returns JSON: "+
					"{group_id, worker, status, run_id, result, error}. "+
					"Status is always one of: %s. result is present only when status is completed.",
				taskLegStatusEnum,
			),
			InputSchema: workerResultInputSchema,
			ReadOnly:    true,
		},
	}
	if d.readOnly {
		return reads, nil
	}
	return append(reads, contract.ToolDef{
		Name: "cancel_task",
		Description: "Cancel every still-cancellable leg (queued, dispatched or running) of one task group. " +
			"This is a write action and is rejected by the product write gate. " +
			"Returns JSON: {group_id, cancelled}.",
		InputSchema: taskGroupInputSchema,
	}, contract.ToolDef{
		Name: "retry_failed_leg",
		Description: "Requeue one worker's failed leg in an active task group so it runs again. " +
			"This is a write action and is rejected by the product write gate. " +
			"Returns JSON: {group_id, worker, task_id, status}.",
		InputSchema: workerResultInputSchema,
	}), nil
}

// Dispatch executes one task status tool call and returns structured JSON.
func (d *TaskStatusDispatcher) Dispatch(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	switch call.Name {
	case "get_task_group":
		return d.getTaskGroup(ctx, call)
	case "list_my_tasks":
		return d.listMyTasks(ctx, call)
	case "get_worker_result":
		return d.getWorkerResult(ctx, call)
	case "cancel_task":
		return d.cancelTask(ctx, call)
	case "retry_failed_leg":
		return d.retryFailedLeg(ctx, call)
	default:
		return taskStatusError(call.ID, fmt.Sprintf("unknown tool %q", call.Name)), nil
	}
}

type taskGroupLegJSON struct {
	Agent       string     `json:"agent"`
	Status      string     `json:"status"`
	RunID       string     `json:"run_id,omitempty"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	Error       string     `json:"error,omitempty"`
}

type taskGroupStatusJSON struct {
	GroupID      string             `json:"group_id"`
	Status       string             `json:"status"`
	AvatarAgent  string             `json:"avatar_agent"`
	Quorum       int                `json:"quorum"`
	GroupOutcome string             `json:"group_outcome,omitempty"`
	CreatedAt    time.Time          `json:"created_at"`
	DeadlineAt   *time.Time         `json:"deadline_at,omitempty"`
	ResolvedAt   *time.Time         `json:"resolved_at,omitempty"`
	Legs         []taskGroupLegJSON `json:"legs"`
}

func (d *TaskStatusDispatcher) getTaskGroup(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	groupID, err := taskStatusGroupID(call)
	if err != nil {
		return taskStatusError(call.ID, err.Error()), nil
	}
	if d.groups == nil || d.legs == nil {
		return taskStatusError(call.ID, "task status tools are unavailable"), nil
	}
	snapshot, err := d.groups.Snapshot(ctx, d.workspaceID, groupID)
	if err != nil {
		return taskStatusError(call.ID, err.Error()), nil
	}
	legs, err := d.legs.ListByGroup(ctx, d.workspaceID, groupID)
	if err != nil {
		return taskStatusError(call.ID, err.Error()), nil
	}
	out := taskGroupStatusJSON{
		GroupID:      snapshot.Group.ID,
		Status:       snapshot.Group.Status,
		AvatarAgent:  snapshot.Group.AvatarAgent,
		Quorum:       snapshot.Group.Quorum,
		GroupOutcome: snapshot.Group.GroupOutcome,
		CreatedAt:    snapshot.Group.CreatedAt,
		DeadlineAt:   snapshot.Group.DeadlineAt,
		ResolvedAt:   snapshot.Group.ResolvedAt,
		Legs:         make([]taskGroupLegJSON, 0, len(legs)),
	}
	for _, leg := range legs {
		out.Legs = append(out.Legs, taskGroupLegJSON{
			Agent: leg.Agent, Status: leg.Status, RunID: leg.RunID,
			StartedAt: leg.StartedAt, CompletedAt: leg.CompletedAt, Error: leg.Error,
		})
	}
	return taskStatusJSON(call.ID, out)
}

type myTaskLegJSON struct {
	Agent  string `json:"agent"`
	Status string `json:"status"`
}

type myTaskGroupJSON struct {
	GroupID     string          `json:"group_id"`
	Status      string          `json:"status"`
	AvatarAgent string          `json:"avatar_agent"`
	CreatedAt   time.Time       `json:"created_at"`
	ResolvedAt  *time.Time      `json:"resolved_at,omitempty"`
	Legs        []myTaskLegJSON `json:"legs"`
}

type myTasksJSON struct {
	Groups []myTaskGroupJSON `json:"groups"`
	Total  int               `json:"total"`
}

func (d *TaskStatusDispatcher) listMyTasks(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	if d.groups == nil {
		return taskStatusError(call.ID, "task status tools are unavailable"), nil
	}
	groups, _, err := d.groups.ListGroups(ctx, fanout.GroupListFilter{
		WorkspaceID: d.workspaceID,
		Statuses:    []string{fanout.StatusActive, fanout.StatusResolving, fanout.StatusResolved},
		Limit:       myTasksListLimit,
	})
	if err != nil {
		return taskStatusError(call.ID, err.Error()), nil
	}
	out := myTasksJSON{Groups: make([]myTaskGroupJSON, 0)}
	for _, group := range groups {
		if !d.relatedToGroup(group) {
			continue
		}
		item := myTaskGroupJSON{
			GroupID:     group.ID,
			Status:      group.Status,
			AvatarAgent: group.AvatarAgent,
			CreatedAt:   group.CreatedAt,
			ResolvedAt:  group.ResolvedAt,
			Legs:        make([]myTaskLegJSON, 0, len(group.Legs)),
		}
		for _, leg := range group.Legs {
			item.Legs = append(item.Legs, myTaskLegJSON{Agent: leg.Agent, Status: leg.Status})
		}
		out.Groups = append(out.Groups, item)
	}
	out.Total = len(out.Groups)
	return taskStatusJSON(call.ID, out)
}

// relatedToGroup reports whether the group belongs to this agent (as avatar or
// leg worker) and, when a user scope is set, to that user.
func (d *TaskStatusDispatcher) relatedToGroup(group fanout.GroupListItem) bool {
	if d.userID != "" && group.UserID != d.userID {
		return false
	}
	if group.AvatarAgent == d.agent {
		return true
	}
	for _, leg := range group.Legs {
		if leg.Agent == d.agent {
			return true
		}
	}
	return false
}

type workerResultJSON struct {
	GroupID string          `json:"group_id"`
	Worker  string          `json:"worker"`
	Status  string          `json:"status"`
	RunID   string          `json:"run_id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   string          `json:"error,omitempty"`
}

func (d *TaskStatusDispatcher) getWorkerResult(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	groupID, worker, err := taskStatusGroupWorker(call)
	if err != nil {
		return taskStatusError(call.ID, err.Error()), nil
	}
	if d.legs == nil {
		return taskStatusError(call.ID, "task status tools are unavailable"), nil
	}
	legs, err := d.legs.ListByGroup(ctx, d.workspaceID, groupID)
	if err != nil {
		return taskStatusError(call.ID, err.Error()), nil
	}
	// Legs arrive ordered by creation; the newest matching leg wins when one
	// worker was dispatched more than once in the same group.
	var match *taskqueue.Task
	for i := range legs {
		if legs[i].Agent == worker {
			match = &legs[i]
		}
	}
	if match == nil {
		return taskStatusError(call.ID, fmt.Sprintf("no leg for worker %q in task group %q", worker, groupID)), nil
	}
	return taskStatusJSON(call.ID, workerResultJSON{
		GroupID: groupID, Worker: worker, Status: match.Status,
		RunID: match.RunID, Result: match.Result, Error: match.Error,
	})
}

func (d *TaskStatusDispatcher) cancelTask(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	if d.readOnly || d.writes == nil {
		return taskStatusError(call.ID, fmt.Sprintf("tool %q is not available to sub-agents (read-only task status access)", call.Name)), nil
	}
	groupID, err := taskStatusGroupID(call)
	if err != nil {
		return taskStatusError(call.ID, err.Error()), nil
	}
	if _, err := d.groups.Snapshot(ctx, d.workspaceID, groupID); err != nil {
		return taskStatusError(call.ID, err.Error()), nil
	}
	cancelled, err := d.writes.CancelGroupLegs(ctx, d.workspaceID, groupID)
	if err != nil {
		return taskStatusError(call.ID, err.Error()), nil
	}
	return taskStatusJSON(call.ID, map[string]any{
		"group_id":  groupID,
		"cancelled": cancelled,
	})
}

func (d *TaskStatusDispatcher) retryFailedLeg(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	if d.readOnly || d.writes == nil {
		return taskStatusError(call.ID, fmt.Sprintf("tool %q is not available to sub-agents (read-only task status access)", call.Name)), nil
	}
	groupID, worker, err := taskStatusGroupWorker(call)
	if err != nil {
		return taskStatusError(call.ID, err.Error()), nil
	}
	snapshot, err := d.groups.Snapshot(ctx, d.workspaceID, groupID)
	if err != nil {
		return taskStatusError(call.ID, err.Error()), nil
	}
	if snapshot.Group.Status != fanout.StatusActive {
		return taskStatusError(call.ID, fmt.Sprintf(
			"task group %q is %s; only legs of active groups can be retried", groupID, snapshot.Group.Status,
		)), nil
	}
	task, err := d.writes.RequeueFailedLeg(ctx, d.workspaceID, groupID, worker)
	if err != nil {
		return taskStatusError(call.ID, err.Error()), nil
	}
	return taskStatusJSON(call.ID, map[string]any{
		"group_id": groupID,
		"worker":   worker,
		"task_id":  task.ID,
		"status":   task.Status,
	})
}

func taskStatusGroupID(call contract.ToolCall) (string, error) {
	var input struct {
		GroupID string `json:"group_id"`
	}
	if err := json.Unmarshal([]byte(call.Args), &input); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	if input.GroupID == "" {
		return "", fmt.Errorf("group_id is required")
	}
	return input.GroupID, nil
}

func taskStatusGroupWorker(call contract.ToolCall) (string, string, error) {
	var input struct {
		GroupID string `json:"group_id"`
		Worker  string `json:"worker"`
	}
	if err := json.Unmarshal([]byte(call.Args), &input); err != nil {
		return "", "", fmt.Errorf("invalid input: %w", err)
	}
	if input.GroupID == "" || input.Worker == "" {
		return "", "", fmt.Errorf("both 'group_id' and 'worker' fields are required")
	}
	return input.GroupID, input.Worker, nil
}

func taskStatusJSON(callID string, payload any) (*contract.ToolResult, error) {
	content, err := json.Marshal(payload)
	if err != nil {
		return taskStatusError(callID, "failed to encode task status: "+err.Error()), nil
	}
	return &contract.ToolResult{CallID: callID, Content: string(content)}, nil
}

func taskStatusError(callID, content string) *contract.ToolResult {
	return &contract.ToolResult{CallID: callID, Content: content, IsError: true}
}

var _ contract.ToolDispatcher = (*TaskStatusDispatcher)(nil)
