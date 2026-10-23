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
	// ErrApplicationQuotaExceeded refuses an admission its application's own
	// limit has no room for, while the workspace may still have room.
	ErrApplicationQuotaExceeded    = errors.New("capability application quota exceeded")
	ErrInvocationIdentityConflict  = errors.New("invocation id is already used by another request")
	ErrInvocationLaneActive        = errors.New("another invocation is active in this lane")
	ErrWorkflowInvocationCancelled = errors.New("workflow invocation was cancelled before admission")
	ErrWorkflowSourceRequired      = errors.New("published capability does not reference a workflow")
)

type DraftStore interface {
	ListDrafts(context.Context, string) ([]capability.Definition, error)
	SaveDraft(context.Context, string, capability.Definition) error
	GetDraft(context.Context, string, string) (capability.Definition, error)
	SaveRevision(context.Context, string, capability.PublishedRevision) error
	GetRevision(context.Context, string, string, int64) (capability.PublishedRevision, error)
}

type Service struct {
	drafts DraftStore
}

func NewService(drafts DraftStore) *Service {
	return &Service{drafts: drafts}
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
	if err := capability.ValidateSchemas(published.Definition); err != nil {
		return capability.PublishedRevision{}, err
	}
	if err := s.drafts.SaveRevision(ctx, workspaceID, published); err != nil {
		return capability.PublishedRevision{}, err
	}
	return published, nil
}

// grantIdentity names the application call an access grant is checked for.
type grantIdentity struct {
	WorkspaceID   string
	ApplicationID string
	CredentialID  string
	CapabilityID  string
	Revision      int64
}

// MemoryStore is a deterministic draft store for contract and adapter tests.
type MemoryStore struct {
	mu        sync.Mutex
	drafts    map[string]capability.Definition
	revisions map[string]capability.PublishedRevision
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{drafts: map[string]capability.Definition{}, revisions: map[string]capability.PublishedRevision{}}
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

func cloneValue[T any](value T) T {
	raw, _ := json.Marshal(value)
	var copied T
	_ = json.Unmarshal(raw, &copied)
	return copied
}
