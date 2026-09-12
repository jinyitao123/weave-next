// Package capabilities adapts the engine-independent capability contract to
// application concerns such as workspace ownership and invocation identity.
package capabilities

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/jinyitao123/weave/internal/base/capability"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

var (
	ErrNotFound            = errors.New("capability not found")
	ErrRevisionNotFound    = errors.New("published capability revision not found")
	ErrIdempotencyConflict = errors.New("invocation request id already used with different input")
	ErrInvocationNotFound  = errors.New("invocation not found")
	ErrInvocationTerminal  = errors.New("invocation is already terminal")
)

type DraftStore interface {
	SaveDraft(context.Context, string, capability.Definition) error
	GetDraft(context.Context, string, string) (capability.Definition, error)
	SaveRevision(context.Context, string, capability.PublishedRevision) error
	GetRevision(context.Context, string, string, int64) (capability.PublishedRevision, error)
}

type InvocationStore interface {
	ClaimInvocation(context.Context, Invocation) (Invocation, bool, error)
	GetInvocation(context.Context, string, string, string) (Invocation, error)
	CancelInvocation(context.Context, string, string, string) (Invocation, error)
}

func (s *Service) GetInvocation(ctx context.Context, workspaceID, applicationID, invocationID string) (Invocation, error) {
	if s == nil || s.invocations == nil {
		return Invocation{}, errors.New("capability invocation service is not configured")
	}
	return s.invocations.GetInvocation(ctx, workspaceID, applicationID, invocationID)
}

func (s *Service) CancelInvocation(ctx context.Context, workspaceID, applicationID, invocationID string) (Invocation, error) {
	if s == nil || s.invocations == nil {
		return Invocation{}, errors.New("capability invocation service is not configured")
	}
	return s.invocations.CancelInvocation(ctx, workspaceID, applicationID, invocationID)
}

type Service struct {
	drafts      DraftStore
	invocations InvocationStore
}

func NewService(drafts DraftStore, invocations InvocationStore) *Service {
	return &Service{drafts: drafts, invocations: invocations}
}

type DraftRequest struct {
	WorkspaceID string
	Definition  capability.Definition
}

func (s *Service) SaveDraft(ctx context.Context, request DraftRequest) error {
	if s == nil || s.drafts == nil {
		return errors.New("capability draft store is not configured")
	}
	if strings.TrimSpace(request.WorkspaceID) == "" {
		return errors.New("workspace id is required")
	}
	if err := request.Definition.Validate(); err != nil {
		return err
	}
	return s.drafts.SaveDraft(ctx, request.WorkspaceID, request.Definition)
}

func (s *Service) Publish(ctx context.Context, workspaceID, capabilityID string, revision int64) (capability.PublishedRevision, error) {
	if s == nil || s.drafts == nil {
		return capability.PublishedRevision{}, errors.New("capability draft store is not configured")
	}
	draft, err := s.drafts.GetDraft(ctx, workspaceID, capabilityID)
	if err != nil {
		return capability.PublishedRevision{}, err
	}
	published, err := capability.Publish(draft, revision)
	if err != nil {
		return capability.PublishedRevision{}, err
	}
	if err := s.drafts.SaveRevision(ctx, workspaceID, published); err != nil {
		return capability.PublishedRevision{}, err
	}
	return published, nil
}

type Invocation struct {
	WorkspaceID   string          `json:"workspace_id"`
	ApplicationID string          `json:"application_id"`
	InvocationID  string          `json:"invocation_id"`
	TaskID        string          `json:"task_id,omitempty"`
	RequestID     string          `json:"request_id"`
	CapabilityID  string          `json:"capability_id"`
	Revision      int64           `json:"revision"`
	Input         json.RawMessage `json:"input"`
	Status        string          `json:"status"`
	ResultState   string          `json:"result_state"`
	Result        json.RawMessage `json:"result,omitempty"`
	Error         string          `json:"error,omitempty"`
}

type InvocationTask struct {
	TaskID       string
	WorkspaceID  string
	InvocationID string
	CapabilityID string
	Revision     int64
	Input        json.RawMessage
}

type ExecutionStore interface {
	ClaimTask(context.Context) (InvocationTask, bool, error)
	CompleteTask(context.Context, InvocationTask, json.RawMessage, error) (Invocation, error)
}

type TaskExecutor interface {
	Execute(context.Context, InvocationTask) (json.RawMessage, error)
}

// RunOne claims at most one durable capability task and writes its terminal
// invocation state. The executor owns engine-specific behavior; this package
// owns state transitions and recovery facts.
func RunOne(ctx context.Context, store ExecutionStore, executor TaskExecutor) (bool, error) {
	if store == nil || executor == nil {
		return false, errors.New("capability execution dependencies are not configured")
	}
	task, claimed, err := store.ClaimTask(ctx)
	if err != nil || !claimed {
		return claimed, err
	}
	result, executeErr := executor.Execute(ctx, task)
	_, err = store.CompleteTask(ctx, task, result, executeErr)
	return true, err
}

type InvokeRequest struct {
	WorkspaceID   string
	ApplicationID string
	InvocationID  string
	RequestID     string
	CapabilityID  string
	Revision      int64
	Input         json.RawMessage
}

func (s *Service) Invoke(ctx context.Context, request InvokeRequest) (Invocation, bool, error) {
	if s == nil || s.drafts == nil || s.invocations == nil {
		return Invocation{}, false, errors.New("capability invocation service is not configured")
	}
	if strings.TrimSpace(request.WorkspaceID) == "" || strings.TrimSpace(request.ApplicationID) == "" || strings.TrimSpace(request.RequestID) == "" {
		return Invocation{}, false, errors.New("workspace, application, and request id are required")
	}
	if request.Revision < 1 || strings.TrimSpace(request.CapabilityID) == "" {
		return Invocation{}, false, errors.New("capability id and positive revision are required")
	}
	var input map[string]any
	if len(request.Input) == 0 || json.Unmarshal(request.Input, &input) != nil || input == nil {
		return Invocation{}, false, errors.New("input must be a JSON object")
	}
	canonicalInput, err := frozen.CanonicalizeJSON(request.Input)
	if err != nil {
		return Invocation{}, false, fmt.Errorf("input must be canonicalizable JSON: %w", err)
	}
	if _, err := s.drafts.GetRevision(ctx, request.WorkspaceID, request.CapabilityID, request.Revision); err != nil {
		return Invocation{}, false, fmt.Errorf("%w: %v", ErrRevisionNotFound, err)
	}
	invocation := Invocation{
		WorkspaceID: request.WorkspaceID, ApplicationID: request.ApplicationID,
		InvocationID: request.InvocationID, RequestID: request.RequestID,
		CapabilityID: request.CapabilityID, Revision: request.Revision,
		Input: canonicalInput, Status: "queued", ResultState: "unavailable",
	}
	if invocation.InvocationID == "" {
		invocation.InvocationID = uuid.NewString()
	}
	stored, replayed, err := s.invocations.ClaimInvocation(ctx, invocation)
	if err != nil {
		return Invocation{}, false, err
	}
	return stored, replayed, nil
}

// MemoryStore is a deterministic store for contract and adapter tests. A
// PostgreSQL implementation can satisfy the same interfaces without changing
// the application service.
type MemoryStore struct {
	mu        sync.Mutex
	drafts    map[string]capability.Definition
	revisions map[string]capability.PublishedRevision
	invokes   map[string]Invocation
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{drafts: map[string]capability.Definition{}, revisions: map[string]capability.PublishedRevision{}, invokes: map[string]Invocation{}}
}

func (m *MemoryStore) SaveDraft(_ context.Context, workspaceID string, definition capability.Definition) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.drafts[workspaceID+"\x00"+definition.CapabilityID] = definition
	return nil
}

func (m *MemoryStore) GetDraft(_ context.Context, workspaceID, capabilityID string) (capability.Definition, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.drafts[workspaceID+"\x00"+capabilityID]
	if !ok {
		return capability.Definition{}, ErrNotFound
	}
	return d, nil
}

func (m *MemoryStore) SaveRevision(_ context.Context, workspaceID string, revision capability.PublishedRevision) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.revisions[workspaceID+"\x00"+revision.CapabilityID+"\x00"+fmt.Sprint(revision.Revision)] = revision
	return nil
}

func (m *MemoryStore) GetRevision(_ context.Context, workspaceID, capabilityID string, revision int64) (capability.PublishedRevision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.revisions[workspaceID+"\x00"+capabilityID+"\x00"+fmt.Sprint(revision)]
	if !ok {
		return capability.PublishedRevision{}, ErrRevisionNotFound
	}
	return r, nil
}

func (m *MemoryStore) ClaimInvocation(_ context.Context, invocation Invocation) (Invocation, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := invocation.WorkspaceID + "\x00" + invocation.ApplicationID + "\x00" + invocation.RequestID
	if existing, ok := m.invokes[key]; ok {
		if string(existing.Input) != string(invocation.Input) || existing.CapabilityID != invocation.CapabilityID || existing.Revision != invocation.Revision {
			return Invocation{}, false, ErrIdempotencyConflict
		}
		return existing, true, nil
	}
	if invocation.TaskID == "" {
		invocation.TaskID = "cap-task-memory-" + invocation.InvocationID
	}
	m.invokes[key] = invocation
	return invocation, false, nil
}

func (m *MemoryStore) GetInvocation(_ context.Context, workspaceID, applicationID, invocationID string) (Invocation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, invocation := range m.invokes {
		if invocation.WorkspaceID == workspaceID && (applicationID == "" || invocation.ApplicationID == applicationID) && invocation.InvocationID == invocationID {
			return invocation, nil
		}
	}
	return Invocation{}, ErrInvocationNotFound
}

func (m *MemoryStore) CancelInvocation(_ context.Context, workspaceID, applicationID, invocationID string) (Invocation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, invocation := range m.invokes {
		if invocation.WorkspaceID == workspaceID && invocation.ApplicationID == applicationID && invocation.InvocationID == invocationID {
			if invocation.Status == "completed" || invocation.Status == "failed" || invocation.Status == "cancelled" {
				return Invocation{}, ErrInvocationTerminal
			}
			invocation.Status = "cancelled"
			m.invokes[key] = invocation
			return invocation, nil
		}
	}
	return Invocation{}, ErrInvocationNotFound
}

func (m *MemoryStore) ClaimTask(_ context.Context) (InvocationTask, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, invocation := range m.invokes {
		if invocation.Status != "queued" {
			continue
		}
		invocation.Status = "running"
		m.invokes[key] = invocation
		return InvocationTask{TaskID: invocation.TaskID, WorkspaceID: invocation.WorkspaceID, InvocationID: invocation.InvocationID, CapabilityID: invocation.CapabilityID, Revision: invocation.Revision, Input: invocation.Input}, true, nil
	}
	return InvocationTask{}, false, nil
}

func (m *MemoryStore) CompleteTask(_ context.Context, task InvocationTask, result json.RawMessage, executeErr error) (Invocation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, invocation := range m.invokes {
		if invocation.TaskID != task.TaskID {
			continue
		}
		if executeErr != nil {
			invocation.Status, invocation.ResultState, invocation.Error = "failed", "unavailable", executeErr.Error()
		} else {
			invocation.Status, invocation.ResultState, invocation.Result = "completed", "available", result
		}
		m.invokes[key] = invocation
		return invocation, nil
	}
	return Invocation{}, ErrInvocationNotFound
}
