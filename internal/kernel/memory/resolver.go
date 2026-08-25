package memory

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/kernel/embedder"
)

// EmbedderSettings is the embedding provider configuration used to build one
// workspace's memory service (or the system fallback from EMBEDDER_URL).
type EmbedderSettings struct {
	BaseURL   string
	APIKey    string
	Model     string // defaults to "text-embedding-3-small"
	Dimension int    // defaults to 1536
}

// EmbedderSource supplies the self-configured embedder of one workspace.
// ok=false means "not configured" (not an error).
type EmbedderSource interface {
	Get(ctx context.Context, workspaceID string) (EmbedderSettings, bool, error)
}

// EmbedderResolver resolves per-workspace *Service snapshots: the workspace's
// own embedder wins, the system settings are the fallback, and a workspace
// with neither resolves to nil (memory disabled). Services are immutable
// after build; embedder CRUD invalidates the cache instead of mutating.
type EmbedderResolver struct {
	mu     sync.RWMutex
	pool   *pgxpool.Pool
	source EmbedderSource      // nil = no credentials store → system only
	system *EmbedderSettings   // EMBEDDER_URL env; nil = no system fallback
	cache  map[string]*Service // value may be nil (negative cache: workspace has no embedder)
	gen    map[string]uint64   // cache key → generation; bumped by Invalidate
	epoch  uint64              // bumped by SetSource; stales every in-flight build
}

// NewEmbedderResolver creates an EmbedderResolver over the shared PG pool.
func NewEmbedderResolver(pool *pgxpool.Pool, system *EmbedderSettings) *EmbedderResolver {
	return &EmbedderResolver{
		pool:   pool,
		system: system,
		cache:  make(map[string]*Service),
		gen:    make(map[string]uint64),
	}
}

// SetSource wires the workspace embedder source and drops all cached services.
func (r *EmbedderResolver) SetSource(src EmbedderSource) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.source = src
	r.cache = make(map[string]*Service)
	// Builds already in flight read the old source's data; bump the epoch so
	// none of them can land in the fresh cache.
	r.epoch++
}

// ForWorkspace returns the memory service for one workspace: the workspace's
// own embedder first, the system settings as fallback, and (nil, nil) when
// neither is configured. Unconfigured workspaces are negatively cached until
// Invalidate.
func (r *EmbedderResolver) ForWorkspace(ctx context.Context, workspaceID string) (*Service, error) {
	r.mu.RLock()
	source := r.source
	key := workspaceID
	if source == nil {
		// All workspaces share the system-only entry.
		key = ""
	}
	if svc, ok := r.cache[key]; ok {
		r.mu.RUnlock()
		return svc, nil
	}
	startGen := r.gen[key]
	startEpoch := r.epoch
	r.mu.RUnlock()

	// Build without holding the lock (source.Get and Migrate do IO).
	svc, err := r.build(ctx, workspaceID, source)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.source != source || r.epoch != startEpoch {
		// SetSource raced with this build; serve the service without caching
		// it under a stale key.
		return svc, nil
	}
	if r.gen[key] != startGen {
		// Invalidate ran while this build was reading the store: the service
		// may predate that CRUD, so it must never enter the cache — otherwise
		// a PUT's eager rebuild could return this stale service as "current".
		// Serve a post-invalidate build if one already landed; otherwise serve
		// this one uncached — the next request rebuilds from fresh data.
		if cached, ok := r.cache[key]; ok {
			return cached, nil
		}
		return svc, nil
	}
	if cached, ok := r.cache[key]; ok {
		// A concurrent build won; both services are equivalent.
		return cached, nil
	}
	r.cache[key] = svc
	return svc, nil
}

// Invalidate drops the cached service (positive or negative) of one workspace
// and marks in-flight builds for that workspace stale. Embedder CRUD calls it
// after a successful write.
func (r *EmbedderResolver) Invalidate(workspaceID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cache, workspaceID)
	r.gen[workspaceID]++
}

// TableDimension returns the vector dimension pinned by the existing
// loom_memory table, or 0 when the table does not exist yet. The single
// table is shared by all workspaces (namespaces isolate the rows), so the
// dimension is a deployment-wide invariant.
func (r *EmbedderResolver) TableDimension(ctx context.Context) (int, error) {
	var typmod int
	err := r.pool.QueryRow(ctx, `
		SELECT atttypmod FROM pg_attribute
		WHERE attrelid = to_regclass('loom_memory') AND attname = 'embedding'
	`).Scan(&typmod)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return typmod, nil
}

func (r *EmbedderResolver) build(ctx context.Context, workspaceID string, source EmbedderSource) (*Service, error) {
	var settings EmbedderSettings
	ok := false
	if source != nil {
		var err error
		settings, ok, err = source.Get(ctx, workspaceID)
		if err != nil {
			return nil, err
		}
	}
	if !ok {
		if r.system == nil {
			return nil, nil // negative result: memory disabled for this workspace
		}
		settings = *r.system
	}
	if settings.Model == "" {
		settings.Model = "text-embedding-3-small"
	}
	if settings.Dimension <= 0 {
		settings.Dimension = 1536
	}

	emb := embedder.New(settings.BaseURL, settings.APIKey, settings.Model, settings.Dimension)
	store := NewStore(r.pool, emb.Dimension())
	if err := store.Migrate(ctx); err != nil {
		return nil, err
	}
	// Verify the dimension AFTER the idempotent migrate, not before: the table
	// pins one dimension for the whole deployment, and CREATE TABLE IF NOT
	// EXISTS cannot change an existing table. When two dimensions race for the
	// first migration, the loser's migrate is a silent no-op — this re-read
	// makes it fail loudly instead of caching a mismatched service.
	dim, err := r.TableDimension(ctx)
	if err != nil {
		return nil, err
	}
	if dim != settings.Dimension {
		return nil, fmt.Errorf("memory: embedder dimension %d does not match existing memory table dimension %d", settings.Dimension, dim)
	}
	return NewService(store, emb), nil
}
