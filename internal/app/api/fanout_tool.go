package api

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/app/conversation"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/fanout"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/org"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

const (
	defaultFanoutGroupDeadline = 10 * time.Minute
	defaultFanoutLegDeadline   = 10 * time.Minute
)

type fanoutRegistry interface {
	ListManaged(ctx context.Context, tenant, fromName string) ([]registry.ManagedAgent, error)
}

type teamWorkerDispatchRulesRegistry interface {
	ResolveTeamWorkerDispatchRules(
		ctx context.Context,
		workspaceID, leadName string,
	) (registry.TeamWorkerDispatchRules, bool, bool, error)
}

type teamDispatchRulesResolver interface {
	ResolveTeamDispatchRules(ctx context.Context, workspaceID, avatarAgent string) (org.TeamDispatchRules, bool, error)
}

type fanoutToolClock interface {
	Now() time.Time
}

// FanoutToolDispatcher creates durable parallel task groups without waiting for
// any leg to execute.
type FanoutToolDispatcher struct {
	registry       fanoutRegistry
	rules          teamDispatchRulesResolver
	fanout         *fanout.Store
	tasks          *taskqueue.Store
	conversations  *conversation.Store
	tenant         string
	selfName       string
	userID         string
	conversationID string
	clock          fanoutToolClock
	groupTTL       time.Duration
	legTTL         time.Duration
	dispatched     atomic.Int64

	// seen dedupes identical batches within this dispatcher's lifetime (one
	// chat turn): a model retrying the same dispatch gets the original group
	// back instead of fanning out duplicate workers.
	seenMu sync.Mutex
	seen   map[string]string
}

// DispatchedCount returns the number of successful parallel dispatches.
func (d *FanoutToolDispatcher) DispatchedCount() int64 {
	return d.dispatched.Load()
}

// NewFanoutToolDispatcher creates the avatar-side parallel dispatch tool.
func NewFanoutToolDispatcher(
	reg fanoutRegistry,
	fanoutStore *fanout.Store,
	taskStore *taskqueue.Store,
	conversations *conversation.Store,
	tenant, selfName, userID, conversationID string,
	clock fanoutToolClock,
) *FanoutToolDispatcher {
	if clock == nil {
		clock = fanout.RealClock{}
	}
	return &FanoutToolDispatcher{
		registry:       reg,
		fanout:         fanoutStore,
		tasks:          taskStore,
		conversations:  conversations,
		tenant:         tenant,
		selfName:       selfName,
		userID:         userID,
		conversationID: conversationID,
		clock:          clock,
		groupTTL:       defaultFanoutGroupDeadline,
		legTTL:         defaultFanoutLegDeadline,
		seen:           make(map[string]string),
	}
}

var fanoutInputSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"tasks": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"worker": {"type": "string"},
					"message": {"type": "string"}
				},
				"required": ["worker", "message"]
			}
		},
		"quorum": {
			"type": "integer",
			"description": "Number of successfully completed worker tasks required to trigger an early report; failed, cancelled, cut, or timed-out tasks do not count."
		}
	},
	"required": ["tasks"]
}`)

// ListTools exposes parallel dispatch only when the avatar manages workers.
func (d *FanoutToolDispatcher) ListTools(ctx context.Context) ([]contract.ToolDef, error) {
	if d.registry == nil {
		return nil, nil
	}
	managed, err := d.registry.ListManaged(ctx, d.tenant, d.selfName)
	if err != nil {
		return nil, nil
	}

	var catalog strings.Builder
	count := 0
	for _, worker := range managed {
		if worker.Name == d.selfName || worker.Kind != "dispatch" {
			continue
		}
		fmt.Fprintf(&catalog, "- %s\n", worker.Name)
		count++
	}
	if count == 0 {
		return nil, nil
	}

	return []contract.ToolDef{{
		Name: "dispatch_parallel",
		Description: fmt.Sprintf(
			"Assign independent tasks to multiple managed workers and return immediately. "+
				"Put every task of one assignment into a single call (one batch), then reply to the user right away — "+
				"worker results are delivered back into this conversation automatically when they finish. "+
				"Never re-dispatch the same tasks or call this in a loop while waiting. "+
				"Use quorum when enough successfully completed results should trigger an early report; unsuccessful results never count toward quorum.\n\nManaged workers (%d):\n%s",
			count, catalog.String(),
		),
		InputSchema: fanoutInputSchema,
	}}, nil
}

type fanoutDispatchInput struct {
	Tasks  []fanoutDispatchTask `json:"tasks"`
	Quorum int                  `json:"quorum,omitempty"`
}

type fanoutDispatchTask struct {
	Worker  string `json:"worker"`
	Message string `json:"message"`
}

type fanoutLegPayload struct {
	Agent   string `json:"agent"`
	From    string `json:"from"`
	Message string `json:"message"`
}

func encodeFanoutLegPayload(worker, from, message string) ([]byte, error) {
	return json.Marshal(fanoutLegPayload{
		Agent: worker, From: from, Message: message,
	})
}

// Dispatch validates the entire batch before creating one group and its queued
// legs, then returns the persisted group ID immediately.
func (d *FanoutToolDispatcher) Dispatch(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	if call.Name != "dispatch_parallel" {
		return fanoutToolError(call.ID, fmt.Sprintf("unknown tool %q", call.Name)), nil
	}

	var input fanoutDispatchInput
	if err := json.Unmarshal([]byte(call.Args), &input); err != nil {
		return fanoutToolError(call.ID, "invalid input: "+err.Error()), nil
	}
	if len(input.Tasks) == 0 {
		return fanoutToolError(call.ID, "tasks must not be empty"), nil
	}
	for _, task := range input.Tasks {
		if task.Worker == "" {
			return fanoutToolError(call.ID, "worker is required"), nil
		}
		if task.Worker == d.selfName {
			return fanoutToolError(call.ID, "cannot dispatch to self"), nil
		}
	}

	managed, err := d.registry.ListManaged(ctx, d.tenant, d.selfName)
	if err != nil {
		return fanoutToolError(call.ID, fmt.Sprintf("not authorized to dispatch to %q", input.Tasks[0].Worker)), nil
	}
	managedByName := make(map[string]registry.ManagedAgent, len(managed))
	managedAgentIDs := make(map[string]struct{}, len(managed))
	for _, worker := range managed {
		if worker.Name == "" || worker.ID == "" || worker.Version < 1 || worker.WorkspaceID != d.tenant {
			return fanoutToolError(call.ID, fmt.Sprintf("not authorized to dispatch to %q", input.Tasks[0].Worker)), nil
		}
		if _, duplicate := managedAgentIDs[worker.ID]; duplicate {
			return fanoutToolError(call.ID, fmt.Sprintf("not authorized to dispatch to %q", input.Tasks[0].Worker)), nil
		}
		if _, duplicate := managedByName[worker.Name]; duplicate {
			return fanoutToolError(call.ID, fmt.Sprintf("not authorized to dispatch to %q", input.Tasks[0].Worker)), nil
		}
		managedAgentIDs[worker.ID] = struct{}{}
		managedByName[worker.Name] = worker
	}
	for _, task := range input.Tasks {
		worker, ok := managedByName[task.Worker]
		if !ok || worker.Kind != "dispatch" {
			return fanoutToolError(call.ID, fmt.Sprintf("not authorized to dispatch to %q", task.Worker)), nil
		}
	}

	if d.fanout == nil || d.tasks == nil {
		return fanoutToolError(call.ID, "parallel dispatch is unavailable"), nil
	}

	effectiveQuorum := input.Quorum
	groupTTL := d.groupTTL
	legTTL := d.legTTL
	teamContext := false
	if teamRulesRegistry, ok := d.registry.(teamWorkerDispatchRulesRegistry); ok {
		rules, inTeam, configured, err := teamRulesRegistry.ResolveTeamWorkerDispatchRules(
			ctx, d.tenant, d.selfName,
		)
		if err != nil {
			return fanoutToolError(call.ID, "failed to resolve team dispatch rules: "+err.Error()), nil
		}
		teamContext = inTeam
		if configured {
			effectiveQuorum = rules.Quorum
			groupTTL = time.Duration(rules.GroupDeadlineSec) * time.Second
			legTTL = time.Duration(rules.LegTimeoutSec) * time.Second
		}
	}
	if !teamContext && d.rules != nil {
		rules, configured, err := d.rules.ResolveTeamDispatchRules(ctx, d.tenant, d.selfName)
		if err != nil {
			return fanoutToolError(call.ID, "failed to resolve team dispatch rules: "+err.Error()), nil
		}
		if configured {
			effectiveQuorum = rules.Quorum
			groupTTL = time.Duration(rules.GroupDeadlineSec) * time.Second
			legTTL = time.Duration(rules.LegTimeoutSec) * time.Second
		}
	}
	executionScope := execution.ScopeLegacyOrchestrator
	if teamContext {
		executionScope = execution.ScopeTeamWorkerLeaf
	}

	// Same batch (tasks + quorum) re-dispatched within this turn returns the
	// original group: retry loops must not multiply real worker runs.
	batchKey := fanoutBatchKey(input.Tasks, effectiveQuorum)
	d.seenMu.Lock()
	if groupID, ok := d.seen[batchKey]; ok {
		d.seenMu.Unlock()
		return &contract.ToolResult{
			CallID: call.ID,
			Content: fmt.Sprintf(
				"These tasks are already dispatched (group %s). Results flow back into this conversation when workers finish — do not dispatch again; reply to the user now.",
				groupID,
			),
		}, nil
	}
	d.seenMu.Unlock()
	now := d.clock.Now()
	groupDeadline := now.Add(groupTTL)
	legDeadline := now.Add(legTTL)
	group, err := d.fanout.CreateGroup(ctx, fanout.Group{
		WorkspaceID:     d.tenant,
		AvatarAgent:     d.selfName,
		UserID:          d.userID,
		ConversationID:  d.cardConversationID(),
		OriginalRequest: fanoutTaskOverview(input.Tasks),
		Quorum:          effectiveQuorum,
		DeadlineAt:      &groupDeadline,
	})
	if err != nil {
		return fanoutToolError(call.ID, "failed to create task group: "+err.Error()), nil
	}

	// Reserve the batch under its group id the moment the group exists — before
	// enqueuing legs or the card. If a later step fails and the model retries
	// the identical batch, the retry is suppressed instead of creating a second
	// group and duplicating real worker runs.
	d.seenMu.Lock()
	d.seen[batchKey] = group.ID
	d.seenMu.Unlock()

	for _, item := range input.Tasks {
		worker := managedByName[item.Worker]
		payload, err := encodeFanoutLegPayload(item.Worker, d.selfName, item.Message)
		if err != nil {
			return fanoutToolError(call.ID, "failed to encode task payload: "+err.Error()), nil
		}
		if err := d.tasks.Enqueue(ctx, &taskqueue.Task{
			ID:                    "task-" + uuid.NewString(),
			WorkspaceID:           d.tenant,
			ProjectID:             group.ProjectID,
			Agent:                 worker.Name,
			AgentID:               worker.ID,
			AgentVersion:          worker.Version,
			IdentityKind:          taskqueue.IdentityAgent,
			IdentitySchemaVersion: 2,
			ExecutionScope:        executionScope,
			Source:                "dispatch",
			Status:                taskqueue.StatusQueued,
			TaskGroupID:           group.ID,
			SubtaskDeadlineAt:     &legDeadline,
			Payload:               payload,
		}); err != nil {
			return fanoutToolError(call.ID, "failed to enqueue parallel task: "+err.Error()), nil
		}
	}

	if d.conversations != nil && d.conversationID != "" {
		legs := make([]fanout.LegSnapshot, 0, len(input.Tasks))
		for _, item := range input.Tasks {
			legs = append(legs, fanout.LegSnapshot{Agent: item.Worker, Status: taskqueue.StatusQueued})
		}
		content, metadata := fanout.RenderCard(group, legs, 0, false)
		message, err := d.conversations.AppendMessage(ctx, conversation.Message{
			ConversationID: d.conversationID,
			WorkspaceID:    d.tenant,
			Role:           "event",
			Content:        content,
			Metadata:       metadata,
		})
		if err != nil {
			return fanoutToolError(call.ID, "failed to append dispatch card: "+err.Error()), nil
		}
		if err := d.fanout.SetCardMessage(ctx, d.tenant, group.ID, message.ID); err != nil {
			return fanoutToolError(call.ID, "failed to attach dispatch card: "+err.Error()), nil
		}
	}

	d.dispatched.Add(1)
	return &contract.ToolResult{
		CallID: call.ID,
		Content: fmt.Sprintf(
			"Dispatched %d workers in parallel (group %s). Their results will be synthesized back into this conversation automatically — do not dispatch again or wait in a loop; reply to the user now.",
			len(input.Tasks), group.ID,
		),
	}, nil
}

// fanoutBatchKey canonicalizes a dispatch batch (order-insensitive tasks plus
// quorum) for dedupe. Quorum is part of the key so a same-tasks re-dispatch that
// corrects the quorum is treated as a distinct request, not silently swallowed.
func fanoutBatchKey(tasks []fanoutDispatchTask, quorum int) string {
	parts := make([]string, 0, len(tasks))
	for _, task := range tasks {
		parts = append(parts, task.Worker+"\x00"+task.Message)
	}
	sort.Strings(parts)
	return strconv.Itoa(quorum) + "\x02" + strings.Join(parts, "\x01")
}

func (d *FanoutToolDispatcher) cardConversationID() string {
	if d.conversations == nil {
		return ""
	}
	return d.conversationID
}

func fanoutContainsAgent(agents []registry.ManagedAgent, name string) bool {
	for _, agent := range agents {
		if agent.Name == name {
			return true
		}
	}
	return false
}

func fanoutContainsAgentKind(agents []registry.ManagedAgent, name, kind string) bool {
	for _, agent := range agents {
		if agent.Name == name && agent.Kind == kind {
			return true
		}
	}
	return false
}

func fanoutTaskOverview(tasks []fanoutDispatchTask) string {
	var overview strings.Builder
	for i, task := range tasks {
		if i > 0 {
			overview.WriteByte('\n')
		}
		fmt.Fprintf(&overview, "%s: %s", task.Worker, task.Message)
	}
	return overview.String()
}

func fanoutToolError(callID, content string) *contract.ToolResult {
	return &contract.ToolResult{CallID: callID, Content: content, IsError: true}
}

var _ contract.ToolDispatcher = (*FanoutToolDispatcher)(nil)
