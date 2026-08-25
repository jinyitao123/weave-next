package taskqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
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

// Worker claims and executes tasks while renewing their leases.
type Worker struct {
	store         *Store
	executor      ChatExecutor
	concurrency   int
	pollInterval  time.Duration
	onLegTerminal func(ctx context.Context, workspaceID, groupID string)

	mu         sync.Mutex
	cancel     context.CancelFunc
	cancellers map[string]context.CancelFunc
	pollWG     sync.WaitGroup
}

// SetOnLegTerminal installs the task-group completion hook.
func (w *Worker) SetOnLegTerminal(hook func(ctx context.Context, workspaceID, groupID string)) {
	w.onLegTerminal = hook
}

func NewWorker(store *Store, executor ChatExecutor, concurrency int) *Worker {
	if concurrency <= 0 {
		concurrency = 4
	}
	return &Worker{
		store:        store,
		executor:     executor,
		concurrency:  concurrency,
		pollInterval: 2 * time.Second,
		cancellers:   make(map[string]context.CancelFunc),
	}
}

// Start launches fixed-concurrency polling loops.
func (w *Worker) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	w.mu.Lock()
	w.cancel = cancel
	w.mu.Unlock()
	workerPrefix := uuid.NewString()
	for i := 0; i < w.concurrency; i++ {
		w.pollWG.Add(1)
		go w.pollLoop(ctx, fmt.Sprintf("%s-%d", workerPrefix, i))
	}
	slog.Info("task worker started", "concurrency", w.concurrency)
}

// Stop stops polling without waiting for in-flight task execution.
func (w *Worker) Stop() {
	w.mu.Lock()
	cancel := w.cancel
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	w.pollWG.Wait()
	slog.Info("task worker stopped")
}

// CancelTask changes durable state and cancels local execution if present.
func (w *Worker) CancelTask(ctx context.Context, workspaceID, id string) error {
	if err := w.store.Cancel(ctx, workspaceID, id); err != nil {
		return err
	}
	w.mu.Lock()
	cancel := w.cancellers[id]
	delete(w.cancellers, id)
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

func (w *Worker) pollLoop(ctx context.Context, workerID string) {
	defer w.pollWG.Done()
	for {
		task, err := w.store.Claim(ctx, workerID, ClaimFilter{
			Kind: "chat", IdentityKind: IdentityAgent,
		})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("task claim error", "worker", workerID, "error", err)
			if !waitForPoll(ctx, w.pollInterval) {
				return
			}
			continue
		}
		if task == nil {
			if !waitForPoll(ctx, w.pollInterval) {
				return
			}
			continue
		}

		done := make(chan struct{})
		go func() {
			defer close(done)
			w.executeTask(task)
		}()
		select {
		case <-ctx.Done():
			return
		case <-done:
		}
	}
}

func waitForPoll(ctx context.Context, interval time.Duration) bool {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (w *Worker) executeTask(task *Task) {
	defer func() {
		if task.TaskGroupID != "" && w.onLegTerminal != nil {
			w.onLegTerminal(context.Background(), task.WorkspaceID, task.TaskGroupID)
		}
	}()

	var req ChatExecRequest
	if err := json.Unmarshal(task.Payload, &req); err != nil {
		_ = w.store.failClaimed(context.Background(), task.ID, task.WorkerID, "invalid payload: "+err.Error())
		return
	}
	stamp, err := taskAgentExecutionStamp(task)
	if err != nil {
		_ = w.store.failClaimed(context.Background(), task.ID, task.WorkerID, "invalid task identity: "+err.Error())
		return
	}
	// Durable task columns are the only execution identity.
	req.Agent = task.Agent
	req.AgentID = task.AgentID
	req.AgentVersion = task.AgentVersion
	req.ExecutionStamp = stamp
	req.RunSnapshotID = task.RunSnapshotID
	if req.ProjectID != task.ProjectID {
		_ = w.store.failClaimed(context.Background(), task.ID, task.WorkerID, "project differs from durable task")
		return
	}
	req.ProjectID = task.ProjectID
	if !runtimeAssignmentMatchesTask(task, req.RuntimeAssignment) {
		_ = w.store.failClaimed(context.Background(), task.ID, task.WorkerID, "runtime assignment differs from durable task")
		return
	}

	// An admitted business task is bounded by its durable lease and explicit
	// cancellation, not by an arbitrary wall-clock deadline. Long-running team
	// collaboration remains recoverable through heartbeats and can still be
	// cancelled through CancelTask.
	execCtx, cancel := context.WithCancel(context.Background())
	w.mu.Lock()
	w.cancellers[task.ID] = cancel
	w.mu.Unlock()
	defer func() {
		cancel()
		w.mu.Lock()
		delete(w.cancellers, task.ID)
		w.mu.Unlock()
	}()

	type outcome struct {
		result *ChatExecResult
		err    error
	}
	outcomes := make(chan outcome, 1)
	go func() {
		result, err := w.executor.ExecuteChat(execCtx, task.WorkspaceID, req)
		outcomes <- outcome{result: result, err: err}
	}()

	heartbeatInterval := w.store.leaseTTL / 3
	if heartbeatInterval <= 0 {
		heartbeatInterval = time.Second
	}
	heartbeats := time.NewTicker(heartbeatInterval)
	defer heartbeats.Stop()

	for {
		select {
		case <-heartbeats.C:
			if err := w.store.Heartbeat(context.Background(), task.ID, task.WorkerID); err != nil {
				cancel()
				slog.Warn("task lease lost", "task_id", task.ID, "error", err)
				return
			}
		case <-execCtx.Done():
			_ = w.store.failClaimed(context.Background(), task.ID, task.WorkerID, "task cancelled or timed out")
			return
		case outcome := <-outcomes:
			if outcome.err != nil {
				var retryable retryableTaskError
				if errors.As(outcome.err, &retryable) && retryable.RetryTask() {
					if err := w.store.requeueClaimed(
						context.Background(), task.ID, task.WorkerID,
					); err != nil {
						slog.Error(
							"task retry requeue failed",
							"task_id", task.ID,
							"error", err,
						)
					}
					return
				}
				errMsg := outcome.err.Error()
				if execCtx.Err() != nil {
					errMsg = "task cancelled or timed out"
				}
				_ = w.store.failClaimed(context.Background(), task.ID, task.WorkerID, errMsg)
				return
			}
			if outcome.result == nil {
				_ = w.store.failClaimed(context.Background(), task.ID, task.WorkerID, "executor returned no result")
				return
			}
			resultJSON, err := json.Marshal(outcome.result)
			if err != nil {
				_ = w.store.failClaimed(context.Background(), task.ID, task.WorkerID, "failed to serialize result: "+err.Error())
				return
			}
			if err := w.store.completeClaimed(context.Background(), task.ID, task.WorkerID, resultJSON, outcome.result.RunID); err != nil {
				slog.Error("task complete write failed", "task_id", task.ID, "error", err)
			}
			return
		}
	}
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
