package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/kernel/sessionexec"
)

// Service orchestrates embedding generation and vector storage.
type Service struct {
	store                  *Store
	embedder               contract.Embedder
	deduplicationThreshold float64
}

// SessionExecutionLeaseHandle is the immutable fencing identity captured by
// asynchronous memory work before the request context returns.
type SessionExecutionLeaseHandle struct {
	Key         sessionexec.SessionKey
	LeaseEpoch  int64
	ActiveRunID string
	// OutboxCommitted gates asynchronous embedding until the final outbox
	// transaction is durable. Nil preserves legacy/direct adapter behavior.
	OutboxCommitted <-chan struct{}
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

// ProjectNamespace builds the stable workspace/Project namespace. The
// avatarID argument is retained during the T2 compatibility window for callers
// that have not dropped the old shape yet.
func ProjectNamespace(workspaceID, avatarID, projectID string) string {
	return "memory:" + workspaceID + ":" + projectID
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

// RememberIfNewFenced repeats the final epoch check immediately before Put.
// Embedding and duplicate search intentionally happen before the transaction.
func (ms *Service) RememberIfNewFenced(
	ctx context.Context,
	handle SessionExecutionLeaseHandle,
	ns string,
	content string,
	metadata map[string]any,
) (string, bool, error) {
	if handle.OutboxCommitted != nil {
		select {
		case <-handle.OutboxCommitted:
		case <-ctx.Done():
			return "", false, fmt.Errorf("memory: wait for final outbox: %w", ctx.Err())
		}
	}
	embeddings, err := ms.embedder.Embed(ctx, []string{content})
	if err != nil {
		return "", false, fmt.Errorf("memory: embed: %w", err)
	}
	if len(embeddings) == 0 || len(embeddings[0]) == 0 {
		return "", false, fmt.Errorf("memory: empty embedding returned")
	}
	existing, err := ms.store.Search(
		ctx, ns, embeddings[0], 1, ms.deduplicationThreshold,
	)
	if err != nil {
		return "", false, fmt.Errorf("memory: dedup search: %w", err)
	}
	if len(existing) > 0 {
		return "", false, nil
	}
	if metadata == nil {
		metadata = map[string]any{}
	} else {
		cloned := make(map[string]any, len(metadata)+1)
		for key, value := range metadata {
			cloned[key] = value
		}
		metadata = cloned
	}
	metadata["active_run_id"] = handle.ActiveRunID
	payload, err := json.Marshal(map[string]any{
		"namespace": ns,
		"content":   content,
		"embedding": embeddings[0],
		"metadata":  metadata,
	})
	if err != nil {
		return "", false, fmt.Errorf("memory: encode fenced put: %w", err)
	}
	tx, err := ms.store.pool.Begin(ctx)
	if err != nil {
		return "", false, fmt.Errorf("memory: begin fenced put: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	err = sessionexec.NewPGStore().PutMemoryTx(
		ctx, tx, handle.Key, handle.LeaseEpoch, payload,
	)
	if err != nil {
		if errors.Is(err, sessionexec.ErrLeaseEpochFenced) {
			if commitErr := tx.Commit(ctx); commitErr != nil {
				return "", false, fmt.Errorf(
					"memory: commit isolated fencing audit: %w", commitErr,
				)
			}
		}
		return "", false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", false, fmt.Errorf("memory: commit fenced put: %w", err)
	}
	return "", true, nil
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
