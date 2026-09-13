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
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/capability"
)

var (
	ErrNotFound            = errors.New("capability not found")
	ErrRevisionNotFound    = errors.New("published capability revision not found")
	ErrIdempotencyConflict = errors.New("invocation request id already used with different input")
	ErrInvocationNotFound  = errors.New("invocation not found")
	ErrInvocationTerminal  = errors.New("invocation is already terminal")
	ErrClaimLost           = errors.New("capability execution claim lost")
	ErrRevisionConflict    = errors.New("published revision already exists with different content")
	ErrQuotaExceeded       = errors.New("capability quota exceeded")
)

type DraftStore interface {
	ListDrafts(context.Context, string) ([]capability.Definition, error)
	SaveDraft(context.Context, string, capability.Definition) error
	GetDraft(context.Context, string, string) (capability.Definition, error)
	SaveRevision(context.Context, string, capability.PublishedRevision) error
	GetRevision(context.Context, string, string, int64) (capability.PublishedRevision, error)
}

type InvocationStore interface {
	ClaimDebugInvocation(context.Context, Invocation, capability.DefinitionSnapshot) (Invocation, bool, error)
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

func (s *Service) GetDraft(ctx context.Context, workspaceID, id string) (capability.Definition, error) {
	if s == nil || s.drafts == nil {
		return capability.Definition{}, errors.New("draft store unavailable")
	}
	return s.drafts.GetDraft(ctx, workspaceID, id)
}
func (s *Service) ListDrafts(ctx context.Context, workspaceID string) ([]capability.Definition, error) {
	if s == nil || s.drafts == nil {
		return nil, errors.New("draft store unavailable")
	}
	return s.drafts.ListDrafts(ctx, workspaceID)
}
func (m *MemoryStore) ListDrafts(_ context.Context, workspaceID string) ([]capability.Definition, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := []capability.Definition{}
	for key, d := range m.drafts {
		if strings.HasPrefix(key, workspaceID+"\x00") {
			result = append(result, cloneValue(d))
		}
	}
	return result, nil
}

func (s *Service) SaveDraft(ctx context.Context, request DraftRequest) error {
	if s == nil || s.drafts == nil {
		return errors.New("capability draft store is not configured")
	}
	if strings.TrimSpace(request.WorkspaceID) == "" {
		return fmt.Errorf("%w: workspace id is required", capability.ErrInvalidDefinition)
	}
	if err := request.Definition.ValidateDraft(); err != nil {
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
	if _, err := capability.Compile(published); err != nil {
		return capability.PublishedRevision{}, err
	}
	if err := s.drafts.SaveRevision(ctx, workspaceID, published); err != nil {
		return capability.PublishedRevision{}, err
	}
	return published, nil
}

type Invocation struct {
	CapabilityName     string                    `json:"-"`
	StepNames          map[string]string         `json:"-"`
	ResultSteps        []string                  `json:"-"`
	CredentialID       string                    `json:"-"`
	CallerKind         string                    `json:"caller_kind"`
	RunKind            string                    `json:"run_kind"`
	DefinitionHash     string                    `json:"definition_hash"`
	WorkspaceID        string                    `json:"workspace_id"`
	ApplicationID      string                    `json:"application_id"`
	InvocationID       string                    `json:"invocation_id"`
	TaskID             string                    `json:"task_id,omitempty"`
	RequestID          string                    `json:"request_id"`
	CapabilityID       string                    `json:"capability_id"`
	Revision           int64                     `json:"revision"`
	Input              json.RawMessage           `json:"input"`
	Status             string                    `json:"status"`
	ResultState        string                    `json:"result_state"`
	Result             json.RawMessage           `json:"result,omitempty"`
	Error              string                    `json:"error,omitempty"`
	ActorUserID        string                    `json:"actor_user_id,omitempty"`
	RuntimeID          string                    `json:"runtime_id,omitempty"`
	UsedSteps          int                       `json:"used_steps"`
	MaxSteps           int                       `json:"max_steps"`
	PhysicalUsage      execution.TerminalUsage   `json:"physical_usage"`
	UnreportedAttempts int                       `json:"unreported_attempts"`
	Checkpoint         capability.ExecutionState `json:"-"`
}

type InvocationTask struct {
	RunKind      string
	WorkerID     string
	ClaimEpoch   int64
	TaskID       string
	WorkspaceID  string
	InvocationID string
	CapabilityID string
	Revision     int64
	Input        json.RawMessage
	Plan         capability.Plan
	State        capability.ExecutionState
	ActorUserID  string
	UsedSteps    int
	MaxSteps     int
	ToolBindings []frozen.FrozenMCPBinding
}

type TaskExecutor interface {
	Execute(context.Context, InvocationTask) (json.RawMessage, error)
}

func executeSafely(ctx context.Context, executor TaskExecutor, task InvocationTask) (result json.RawMessage, err error) {
	defer func() {
		if recover() != nil {
			result = nil
			err = errors.New("capability executor panicked")
		}
	}()
	return executor.Execute(ctx, task)
}

type InvokeRequest struct {
	CredentialID  string
	WorkspaceID   string
	ApplicationID string
	InvocationID  string
	RequestID     string
	CapabilityID  string
	Revision      int64
	Input         json.RawMessage
	ActorUserID   string
	MaxSteps      int
}

func (s *Service) Invoke(ctx context.Context, request InvokeRequest) (Invocation, bool, error) {
	if s == nil || s.drafts == nil || s.invocations == nil {
		return Invocation{}, false, errors.New("capability invocation service is not configured")
	}
	if strings.TrimSpace(request.WorkspaceID) == "" || strings.TrimSpace(request.ApplicationID) == "" || strings.TrimSpace(request.RequestID) == "" {
		return Invocation{}, false, fmt.Errorf("%w: workspace, application, and request id are required", capability.ErrInvalidDefinition)
	}
	if request.Revision < 1 || strings.TrimSpace(request.CapabilityID) == "" {
		return Invocation{}, false, fmt.Errorf("%w: capability id and positive revision are required", capability.ErrInvalidDefinition)
	}
	var input map[string]any
	if len(request.Input) == 0 || json.Unmarshal(request.Input, &input) != nil || input == nil {
		return Invocation{}, false, fmt.Errorf("%w: input must be a JSON object", capability.ErrInvalidDefinition)
	}
	canonicalInput, err := frozen.CanonicalizeJSON(request.Input)
	if err != nil {
		return Invocation{}, false, fmt.Errorf("%w: input must be canonicalizable JSON", capability.ErrInvalidDefinition)
	}
	published, err := s.drafts.GetRevision(ctx, request.WorkspaceID, request.CapabilityID, request.Revision)
	if err != nil {
		return Invocation{}, false, err
	}
	if _, err := capability.Compile(published); err != nil {
		return Invocation{}, false, err
	}
	if err := capability.ValidateValue(published.Definition.InputSchema, canonicalInput); err != nil {
		return Invocation{}, false, fmt.Errorf("%w: input schema: %v", capability.ErrInvalidDefinition, err)
	}
	invocation := Invocation{
		CredentialID: request.CredentialID, CallerKind: "developer",
		RunKind: "published", DefinitionHash: published.DefinitionHash,
		WorkspaceID: request.WorkspaceID, ApplicationID: request.ApplicationID,
		InvocationID: request.InvocationID, RequestID: request.RequestID,
		CapabilityID: request.CapabilityID, Revision: request.Revision,
		Input: canonicalInput, Status: "queued", ResultState: "unavailable", ActorUserID: request.ActorUserID, MaxSteps: request.MaxSteps,
	}
	if invocation.MaxSteps <= 0 {
		invocation.MaxSteps = 100
	}
	if request.CredentialID != "" {
		invocation.CallerKind = "application"
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
	debug     map[string]capability.DefinitionSnapshot
	mu        sync.Mutex
	drafts    map[string]capability.Definition
	revisions map[string]capability.PublishedRevision
	invokes   map[string]Invocation
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{debug: map[string]capability.DefinitionSnapshot{}, drafts: map[string]capability.Definition{}, revisions: map[string]capability.PublishedRevision{}, invokes: map[string]Invocation{}}
}

func (m *MemoryStore) SaveDraft(_ context.Context, workspaceID string, definition capability.Definition) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.drafts[workspaceID+"\x00"+definition.CapabilityID] = cloneValue(definition)
	return nil
}

func (m *MemoryStore) GetDraft(_ context.Context, workspaceID, capabilityID string) (capability.Definition, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.drafts[workspaceID+"\x00"+capabilityID]
	if !ok {
		return capability.Definition{}, ErrNotFound
	}
	return cloneValue(d), nil
}

func (m *MemoryStore) SaveRevision(_ context.Context, workspaceID string, revision capability.PublishedRevision) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := workspaceID + "\x00" + revision.CapabilityID + "\x00" + fmt.Sprint(revision.Revision)
	if existing, ok := m.revisions[key]; ok && existing.DefinitionHash != revision.DefinitionHash {
		return ErrRevisionConflict
	}
	m.revisions[key] = cloneValue(revision)
	return nil
}

func (m *MemoryStore) GetRevision(_ context.Context, workspaceID, capabilityID string, revision int64) (capability.PublishedRevision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.revisions[workspaceID+"\x00"+capabilityID+"\x00"+fmt.Sprint(revision)]
	if !ok {
		return capability.PublishedRevision{}, ErrRevisionNotFound
	}
	return cloneValue(r), nil
}

func (m *MemoryStore) ClaimInvocation(_ context.Context, invocation Invocation) (Invocation, bool, error) {
	if invocation.CredentialID != "" || invocation.CallerKind == "application" {
		return Invocation{}, false, ErrAccessDenied
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := invocation.WorkspaceID + "\x00" + invocation.ApplicationID + "\x00" + invocation.RequestID
	if existing, ok := m.invokes[key]; ok {
		if string(existing.Input) != string(invocation.Input) || existing.CapabilityID != invocation.CapabilityID || existing.Revision != invocation.Revision || existing.RunKind != invocation.RunKind || existing.DefinitionHash != invocation.DefinitionHash {
			return Invocation{}, false, ErrIdempotencyConflict
		}
		return cloneValue(existing), true, nil
	}
	if invocation.TaskID == "" {
		invocation.TaskID = "cap-task-memory-" + invocation.InvocationID
	}
	m.invokes[key] = cloneValue(invocation)
	return cloneValue(invocation), false, nil
}

func (m *MemoryStore) GetInvocation(_ context.Context, workspaceID, applicationID, invocationID string) (Invocation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, invocation := range m.invokes {
		if applicationID != "" && invocation.WorkspaceID == workspaceID && invocation.ApplicationID == applicationID && invocation.InvocationID == invocationID {
			invocation = cloneValue(invocation)
			definition := m.revisions[workspaceID+"\x00"+invocation.CapabilityID+"\x00"+fmt.Sprint(invocation.Revision)].Definition
			if invocation.RunKind == "debug" {
				definition = m.debug[workspaceID+"\x00"+invocationID].Definition
			}
			presentInvocation(&invocation, definition)
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
			if invocation.Status == "cancelled" || invocation.Status == "cancel_requested" {
				return cloneValue(invocation), nil
			}
			if invocation.Status == "completed" || invocation.Status == "failed" {
				return Invocation{}, ErrInvocationTerminal
			}
			if invocation.Status == "running" {
				invocation.Status = "cancel_requested"
			} else {
				invocation.Status = "cancelled"
			}
			m.invokes[key] = invocation
			return invocation, nil
		}
	}
	return Invocation{}, ErrInvocationNotFound
}

func cloneValue[T any](value T) T {
	raw, _ := json.Marshal(value)
	var copied T
	_ = json.Unmarshal(raw, &copied)
	return copied
}
