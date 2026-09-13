package llmrouter

import (
	"context"
	"errors"
	"sync"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

// ProviderSource must return only credentials this trusted subject may read.
// The resolver independently verifies every returned scope before routing.
type ProviderSource interface {
	List(context.Context, string) ([]ProviderConfig, error)
}

type resolverKey struct{ workspaceID, subjectDigest string }

// Resolver partitions immutable provider clients by the platform subject.
// Shared service routes are never cached; their authorization is checked again
// on every Chat/Stream, including calls through previously returned snapshots.
type Resolver struct {
	mu     sync.RWMutex
	system *Router
	source ProviderSource
	cache  map[resolverKey]*subjectRouter
	gen    map[string]uint64
	epoch  uint64
}

func NewResolver(system *Router) *Resolver {
	return &Resolver{system: system, cache: make(map[resolverKey]*subjectRouter), gen: make(map[string]uint64)}
}
func (r *Resolver) SetSource(src ProviderSource) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.source = src
	r.cache = make(map[resolverKey]*subjectRouter)
	r.epoch++
}

func (r *Resolver) ForWorkspace(ctx context.Context, workspaceID string) (contract.LLM, error) {
	subject, err := execution.RequireSubject(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	key := resolverKey{workspaceID, subject.Digest()}
	// An authorization callback can change the visible shared routes. Its state
	// is not a cache version, so none of those snapshots may enter the cache.
	cacheable := subject.ServiceID == "" && !frozen.HasServiceReferenceAuthorization(ctx)
	r.mu.RLock()
	source := r.source
	generation, epoch := r.gen[workspaceID], r.epoch
	if cacheable {
		if cached := r.cache[key]; cached != nil {
			r.mu.RUnlock()
			return cached, nil
		}
	}
	r.mu.RUnlock()
	snap, shared, err := r.build(ctx, subject, source)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.epoch != epoch || r.gen[workspaceID] != generation {
		return nil, errors.New("model credentials changed during routing; resolve again")
	}
	snap.current = func() bool {
		r.mu.RLock()
		defer r.mu.RUnlock()
		return r.epoch == epoch && r.gen[workspaceID] == generation
	}

	if cacheable && !shared {
		if cached := r.cache[key]; cached != nil {
			return cached, nil
		}
		r.cache[key] = snap
	}
	return snap, nil
}

func (r *Resolver) Invalidate(workspaceID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key := range r.cache {
		if key.workspaceID == workspaceID {
			delete(r.cache, key)
		}
	}
	r.gen[workspaceID]++
}
func (r *Resolver) CanResolve(ctx context.Context, workspaceID, model string) (bool, error) {
	llm, err := r.ForWorkspace(ctx, workspaceID)
	if err != nil {
		return false, err
	}
	snap := llm.(*subjectRouter)
	_, err = snap.router.resolve(model)
	return err == nil, nil
}

func (r *Resolver) build(ctx context.Context, subject execution.Subject, source ProviderSource) (*subjectRouter, bool, error) {
	configs := []ProviderConfig{}
	fallback := ""
	if r.system != nil {
		r.system.mu.RLock()
		fallback = r.system.fallback
		for id, cfg := range r.system.providers {
			copy := *cfg
			copy.ID = "system/" + id
			copy.Models = append([]string(nil), cfg.Models...)
			configs = append(configs, copy)
		}
		r.system.mu.RUnlock()
	}
	if source != nil {
		owned, err := source.List(ctx, subject.WorkspaceID)
		if err != nil {
			return nil, false, err
		}
		configs = append(configs, owned...)
	}
	snap := &subjectRouter{router: New(fallback), subject: subject, refs: make(map[string]frozen.CredentialReference)}
	shared := false
	// Personal routes override only their named models after authorized service
	// routes. API keys and provider clients are never shared between subjects.
	for _, scope := range []frozen.CredentialScope{frozen.CredentialScopeWorkspaceService, frozen.CredentialScopeUser} {
		for _, cfg := range configs {
			if cfg.CredentialScope != scope {
				continue
			}
			ref := providerReference(subject.WorkspaceID, cfg)
			if frozen.AuthorizeCredentialReference(ctx, ref) != nil {
				continue
			}
			if scope == frozen.CredentialScopeWorkspaceService {
				shared = true
			}
			snap.router.RegisterProvider(cfg)
			snap.refs[cfg.ID] = ref
		}
	}
	return snap, shared, nil
}

func providerReference(workspaceID string, cfg ProviderConfig) frozen.CredentialReference {
	return frozen.CredentialReference{
		SchemaVersion: frozen.FrozenSchemaVersion, WorkspaceID: workspaceID, Kind: frozen.CredentialProviderAPIKey, ResourceID: cfg.ID, Slot: "api_key",
		Scope: cfg.CredentialScope, UserID: cfg.CredentialUserID, ServiceID: cfg.CredentialServiceID,
	}
}

type subjectRouter struct {
	current func() bool
	router  *Router
	subject execution.Subject
	refs    map[string]frozen.CredentialReference
}

func (s *subjectRouter) authorize(ctx context.Context, model string) error {
	if s.current == nil || !s.current() {
		return errors.New("model credential snapshot is no longer current")
	}

	subject, err := execution.RequireSubject(ctx, s.subject.WorkspaceID)
	if err != nil {
		return err
	}
	if subject != s.subject {
		return execution.ErrSubjectMismatch
	}
	if model == "" {
		model = s.router.fallback
	}
	s.router.mu.RLock()
	providerID := s.router.models[model]
	s.router.mu.RUnlock()
	if ref, ok := s.refs[providerID]; ok {
		return frozen.AuthorizeCredentialReference(ctx, ref)
	}
	return errors.New("no authorized model provider")
}
func (s *subjectRouter) Chat(ctx context.Context, req contract.ChatRequest) (*contract.ChatResponse, error) {
	if err := s.authorize(ctx, req.Model); err != nil {
		return nil, err
	}
	return s.router.Chat(ctx, req)
}
func (s *subjectRouter) Stream(ctx context.Context, req contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	if err := s.authorize(ctx, req.Model); err != nil {
		return nil, err
	}
	return s.router.Stream(ctx, req)
}
