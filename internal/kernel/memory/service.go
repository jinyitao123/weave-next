package memory

import (
	"context"
	"fmt"
	"time"

	"github.com/jinyitao123/loom/contract"
)

// Service orchestrates embedding generation and vector storage.
type Service struct {
	store                  *Store
	embedder               contract.Embedder
	deduplicationThreshold float64
}

// NewService creates a Service with default deduplication threshold.
func NewService(store *Store, embedder contract.Embedder) *Service {
	return &Service{store: store, embedder: embedder, deduplicationThreshold: DefaultDeduplicationThreshold}
}

// Embedder returns the underlying embedder for reuse by other services (e.g., semantic skill matching).
func (ms *Service) Embedder() contract.Embedder {
	return ms.embedder
}

// Namespace builds the canonical namespace for agent memory (tenant-level).
func Namespace(tenant, agent string) string {
	return "memory:" + tenant + ":" + agent
}

// ScopedNamespace builds a memory namespace according to the isolation scope.
//
//	scope="tenant"  (default) → memory:{tenant}:{agent}
//	scope="user"              → memory:{tenant}:{userID}:{agent}
//	scope="session"           → memory:{tenant}:{agent}:{sessionID}
//
// Falls back to tenant scope when the required ID is empty.
func ScopedNamespace(tenant, userID, agent, sessionID, scope string) string {
	switch scope {
	case "user":
		if userID == "" {
			return Namespace(tenant, agent)
		}
		return "memory:" + tenant + ":" + userID + ":" + agent
	case "session":
		if sessionID == "" {
			return Namespace(tenant, agent)
		}
		return "memory:" + tenant + ":" + agent + ":" + sessionID
	default:
		return Namespace(tenant, agent)
	}
}

// Remember embeds the text and stores it as a memory.
func (ms *Service) Remember(ctx context.Context, ns, content string, metadata map[string]any) (string, error) {
	embeddings, err := ms.embedder.Embed(ctx, []string{content})
	if err != nil {
		return "", fmt.Errorf("memory: embed: %w", err)
	}
	if len(embeddings) == 0 || len(embeddings[0]) == 0 {
		return "", fmt.Errorf("memory: empty embedding returned")
	}

	id, err := ms.store.Put(ctx, ns, content, embeddings[0], metadata)
	if err != nil {
		return "", fmt.Errorf("memory: store: %w", err)
	}
	return id, nil
}

// DeduplicationThreshold is the cosine similarity above which a new fact is
// considered a duplicate of an existing memory and will not be stored.
const DefaultDeduplicationThreshold = 0.80

// RememberIfNew embeds the text, checks for near-duplicates, and only stores
// if no existing memory exceeds DeduplicationThreshold similarity.
func (ms *Service) RememberIfNew(ctx context.Context, ns, content string, metadata map[string]any) (string, bool, error) {
	embeddings, err := ms.embedder.Embed(ctx, []string{content})
	if err != nil {
		return "", false, fmt.Errorf("memory: embed: %w", err)
	}
	if len(embeddings) == 0 || len(embeddings[0]) == 0 {
		return "", false, fmt.Errorf("memory: empty embedding returned")
	}

	// Check for near-duplicate: search with high threshold, top-1.
	existing, err := ms.store.Search(ctx, ns, embeddings[0], 1, ms.deduplicationThreshold)
	if err != nil {
		return "", false, fmt.Errorf("memory: dedup search: %w", err)
	}
	if len(existing) > 0 {
		return "", false, nil
	}

	id, err := ms.store.Put(ctx, ns, content, embeddings[0], metadata)
	if err != nil {
		return "", false, fmt.Errorf("memory: store: %w", err)
	}
	return id, true, nil
}

// Recall embeds the query and retrieves top-K similar memories.
func (ms *Service) Recall(ctx context.Context, ns, query string, topK int) ([]Record, error) {
	embeddings, err := ms.embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, fmt.Errorf("memory: embed query: %w", err)
	}
	if len(embeddings) == 0 || len(embeddings[0]) == 0 {
		return nil, fmt.Errorf("memory: empty embedding for query")
	}

	records, err := ms.store.Search(ctx, ns, embeddings[0], topK, 0.5)
	if err != nil {
		return nil, fmt.Errorf("memory: search: %w", err)
	}
	return records, nil
}

// ForgetOne deletes a specific memory.
func (ms *Service) ForgetOne(ctx context.Context, ns, id string) error {
	return ms.store.Delete(ctx, ns, id)
}

// ListAll returns all memories in a namespace.
func (ms *Service) ListAll(ctx context.Context, ns string, limit, offset int) ([]Record, error) {
	return ms.store.List(ctx, ns, limit, offset)
}

// Count returns the number of memories in a namespace.
func (ms *Service) Count(ctx context.Context, ns string) (int, error) {
	return ms.store.Count(ctx, ns)
}

// CleanupStale removes memories not accessed within maxAge and accessed fewer
// than minAccess times.
func (ms *Service) CleanupStale(ctx context.Context, ns string, maxAge time.Duration, minAccess int) (int, error) {
	cutoff := time.Now().Add(-maxAge)
	return ms.store.CleanupStale(ctx, ns, cutoff, minAccess)
}
