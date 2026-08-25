package llmrouter

import (
	"context"
	"sync"

	"github.com/jinyitao123/loom/contract"
)

// ProviderSource supplies the self-configured providers of one workspace.
// *credentials.Store satisfies this interface.
type ProviderSource interface {
	List(ctx context.Context, workspaceID string) ([]ProviderConfig, error)
}

// Resolver resolves per-workspace contract.LLM snapshots.
//
// A snapshot is a Router built from the system Router (env providers +
// DEFAULT_MODEL fallback, re-keyed under the "system/" namespace) plus the
// workspace's own providers; a workspace-declared model overrides the system
// entry for that model only. Snapshots are immutable by convention: after
// build no write method (RegisterProvider/RemoveProvider/Register) is ever
// called on them — provider CRUD invalidates the cache instead.
type Resolver struct {
	mu     sync.RWMutex
	system *Router                 // env providers + DEFAULT_MODEL fallback; never mutated after wiring
	source ProviderSource          // nil = no credentials store → system only
	cache  map[string]contract.LLM // workspaceID → snapshot ("" = shared system-only snapshot)
	gen    map[string]uint64       // cache key → generation; bumped by Invalidate
	epoch  uint64                  // bumped by SetSource; stales every in-flight build
}

// NewResolver creates a Resolver over the process-wide system Router.
func NewResolver(system *Router) *Resolver {
	return &Resolver{
		system: system,
		cache:  make(map[string]contract.LLM),
		gen:    make(map[string]uint64),
	}
}

// SetSource wires the workspace provider source and drops all cached snapshots.
func (r *Resolver) SetSource(src ProviderSource) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.source = src
	r.cache = make(map[string]contract.LLM)
	// Builds already in flight read the old source's data; bump the epoch so
	// none of them can land in the fresh cache.
	r.epoch++
}

// ForWorkspace returns the LLM snapshot for one workspace. Without any
// workspace configuration it still returns a non-nil system-only snapshot;
// it only fails when the provider source itself fails.
func (r *Resolver) ForWorkspace(ctx context.Context, workspaceID string) (contract.LLM, error) {
	r.mu.RLock()
	source := r.source
	key := workspaceID
	if source == nil {
		// All workspaces share the system-only snapshot.
		key = ""
	}
	if snap, ok := r.cache[key]; ok {
		r.mu.RUnlock()
		return snap, nil
	}
	startGen := r.gen[key]
	startEpoch := r.epoch
	r.mu.RUnlock()

	// Build without holding the lock (source.List does IO).
	snap, err := r.build(ctx, workspaceID, source)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.source != source || r.epoch != startEpoch {
		// SetSource raced with this build; serve the snapshot without caching
		// it under a stale key.
		return snap, nil
	}
	if r.gen[key] != startGen {
		// Invalidate ran while this build was reading the store: the snapshot
		// may predate that CRUD (e.g. it can still hold a deleted provider's
		// decrypted key), so it must never enter the cache. Serve a
		// post-invalidate snapshot if one already landed; otherwise serve this
		// one uncached — the next request rebuilds from fresh data.
		if cached, ok := r.cache[key]; ok {
			return cached, nil
		}
		return snap, nil
	}
	if cached, ok := r.cache[key]; ok {
		// A concurrent build won; both snapshots are equivalent.
		return cached, nil
	}
	r.cache[key] = snap
	return snap, nil
}

// Invalidate drops the cached snapshot of one workspace and marks in-flight
// builds for that workspace stale. Provider CRUD calls it after a successful
// write.
func (r *Resolver) Invalidate(workspaceID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cache, workspaceID)
	r.gen[workspaceID]++
}

// CanResolve reports whether the workspace snapshot can dispatch the model
// (workspace provider, system provider, or — for "" — the default fallback).
// Diagnostics only: the startup upgrade scan uses it to name agents whose
// models were configured in another workspace on pre-scoping deployments.
func (r *Resolver) CanResolve(ctx context.Context, workspaceID, model string) (bool, error) {
	snap, err := r.ForWorkspace(ctx, workspaceID)
	if err != nil {
		return false, err
	}
	router, ok := snap.(*Router)
	if !ok {
		return true, nil
	}
	_, err = router.resolve(model)
	return err == nil, nil
}

func (r *Resolver) build(ctx context.Context, workspaceID string, source ProviderSource) (contract.LLM, error) {
	snap := r.system.cloneAsSystem()
	if source != nil {
		cfgs, err := source.List(ctx, workspaceID)
		if err != nil {
			return nil, err
		}
		for _, cfg := range cfgs {
			// Registering after the system clone lets a workspace model win
			// the models-map entry while system providers keep their own
			// namespaced provider/client entries.
			snap.RegisterProvider(cfg)
		}
	}
	return snap, nil
}
