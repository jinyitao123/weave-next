package taskqueue

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jinyitao123/weave/internal/base/execution"
)

// ChatExecutor executes the payload carried by an asynchronous chat task.
type ChatExecutor interface {
	ExecuteChat(ctx context.Context, tenant string, req ChatExecRequest) (*ChatExecResult, error)
}

type retryableTaskError interface {
	RetryTask() bool
}

type Attachment struct {
	ID       string `json:"id"`
	Filename string `json:"filename"`
	Path     string `json:"path"`
}

type RuntimeCapabilityFact struct {
	RuntimeID         string   `json:"runtime_id"`
	Name              string   `json:"name"`
	Engines           []string `json:"engines"`
	RuntimeRevision   int64    `json:"runtime_revision"`
	Enabled           bool     `json:"enabled"`
	Online            bool     `json:"online"`
	Eligible          bool     `json:"eligible"`
	UnavailableReason string   `json:"unavailable_reason,omitempty"`
}

type RuntimeAssignment struct {
	RuntimeID       string                  `json:"runtime_id,omitempty"`
	RuntimeRevision int64                   `json:"runtime_revision,omitempty"`
	Mode            string                  `json:"mode"`
	ReasonCode      string                  `json:"reason_code"`
	Engine          string                  `json:"engine"`
	CapabilityFacts []RuntimeCapabilityFact `json:"capability_facts"`
}

type ChatExecRequest struct {
	Agent             string                         `json:"agent"`
	AgentID           string                         `json:"agent_id,omitempty"`
	AgentVersion      int                            `json:"agent_version,omitempty"`
	ExecutionStamp    *execution.AgentExecutionStamp `json:"-"`
	RunSnapshotID     string                         `json:"-"`
	ProjectID         string                         `json:"project_id,omitempty"`
	RuntimeAssignment *RuntimeAssignment             `json:"runtime_assignment,omitempty"`
	ClientRequestID   string                         `json:"client_request_id,omitempty"`
	SessionID         string                         `json:"session_id"`
	ConversationID    string                         `json:"conversation_id,omitempty"`
	Message           string                         `json:"message"`
	Profile           string                         `json:"profile"`
	Effort            string                         `json:"effort"`
	UserID            string                         `json:"user_id"`
	Context           map[string]any                 `json:"context,omitempty"`
	NoDispatch        bool                           `json:"no_dispatch,omitempty"`
	Attachments       []Attachment                   `json:"attachments,omitempty"`
}

type ChatExecResult struct {
	Output            string             `json:"output"`
	StopReason        string             `json:"stop_reason"`
	SessionID         string             `json:"session_id"`
	RunID             string             `json:"run_id"`
	ProjectID         string             `json:"project_id,omitempty"`
	ConversationID    string             `json:"conversation_id,omitempty"`
	RuntimeAssignment *RuntimeAssignment `json:"runtime_assignment,omitempty"`
}

// ChatHandler translates durable agent identity into the chat execution contract.
type ChatHandler struct{ Executor ChatExecutor }

func (h ChatHandler) ExecuteTask(ctx context.Context, task Task) (TaskResult, error) {
	var req ChatExecRequest
	if err := json.Unmarshal(task.Payload, &req); err != nil {
		return TaskResult{}, fmt.Errorf("invalid payload: %w", err)
	}
	stamp, err := taskAgentExecutionStamp(&task)
	if err != nil {
		return TaskResult{}, fmt.Errorf("invalid task identity: %w", err)
	}
	req.Agent, req.AgentID, req.AgentVersion = task.Agent, task.AgentID, task.AgentVersion
	req.ExecutionStamp, req.RunSnapshotID = stamp, task.RunSnapshotID
	if req.ProjectID != task.ProjectID {
		return TaskResult{}, fmt.Errorf("project differs from durable task")
	}
	if !runtimeAssignmentMatchesTask(&task, req.RuntimeAssignment) {
		return TaskResult{}, fmt.Errorf("runtime assignment differs from durable task")
	}
	result, err := h.Executor.ExecuteChat(ctx, task.WorkspaceID, req)
	if err != nil {
		return TaskResult{}, err
	}
	if result == nil {
		return TaskResult{}, fmt.Errorf("executor returned no result")
	}
	encoded, err := json.Marshal(result)
	return TaskResult{Result: encoded, RunID: result.RunID}, err
}

func runtimeAssignmentMatchesTask(task *Task, assignment *RuntimeAssignment) bool {
	if task == nil {
		return false
	}
	if len(task.RuntimeAssignment) == 0 {
		return assignment == nil && task.RuntimeID == ""
	}
	var durable RuntimeAssignment
	if err := json.Unmarshal(task.RuntimeAssignment, &durable); err != nil {
		return false
	}
	if assignment == nil || durable.RuntimeID != task.RuntimeID {
		return false
	}
	payload, err := json.Marshal(assignment)
	if err != nil {
		return false
	}
	durablePayload, err := json.Marshal(durable)
	return err == nil && string(payload) == string(durablePayload)
}

func taskAgentExecutionStamp(task *Task) (*execution.AgentExecutionStamp, error) {
	if task == nil {
		return nil, fmt.Errorf("task is required")
	}
	if task.IdentityKind != IdentityAgent {
		return nil, fmt.Errorf("identity kind %q is not an agent", task.IdentityKind)
	}
	if task.Agent == "" {
		return nil, fmt.Errorf("agent name is required")
	}
	if task.WorkflowID != "" || task.WorkflowVersion != 0 {
		return nil, fmt.Errorf("agent task contains workflow identity")
	}

	switch task.IdentitySchemaVersion {
	case 1:
		if task.RunSnapshotID != "" {
			return nil, fmt.Errorf("schema-one agent task contains run snapshot identity")
		}
		if task.ExecutionScope != "" {
			return nil, fmt.Errorf("schema-one task contains execution scope")
		}
		if task.AgentID == "" && task.AgentVersion == 0 {
			return nil, nil
		}
		if task.AgentID == "" || task.AgentVersion < 1 {
			return nil, fmt.Errorf("schema-one agent ID and positive version must be paired")
		}
		return &execution.AgentExecutionStamp{
			AgentID:        task.AgentID,
			AgentVersion:   task.AgentVersion,
			ExecutionScope: execution.ScopeLegacyOrchestrator,
			LegacyScope:    true,
		}, nil
	case 2:
		if task.AgentID == "" || task.AgentVersion < 1 || !task.ExecutionScope.Valid() {
			return nil, fmt.Errorf("invalid schema-two agent stamp")
		}
		teamScope := task.ExecutionScope == execution.ScopeTeamFreeCollab ||
			task.ExecutionScope == execution.ScopeTeamWorkerLeaf
		if task.RunSnapshotID != "" && !teamScope {
			return nil, fmt.Errorf("standalone agent task contains run snapshot identity")
		}
		return &execution.AgentExecutionStamp{
			AgentID:        task.AgentID,
			AgentVersion:   task.AgentVersion,
			ExecutionScope: task.ExecutionScope,
			RunSnapshotID:  task.RunSnapshotID,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported identity schema version %d", task.IdentitySchemaVersion)
	}
}
