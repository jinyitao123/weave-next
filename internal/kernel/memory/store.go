// Package memory provides vector-based memory storage and management for Weave.
// This is a platform-level concern — the Loom kernel provides only the Store
// interface and RetrieveStep; all memory management, auto-remember, and decay
// logic lives here in Weave.
package memory

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Record represents a stored memory with its embedding.
type Record struct {
	ID          string         `json:"id"`
	Namespace   string         `json:"namespace"`
	Content     string         `json:"content"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	Score       float64        `json:"score,omitempty"` // cosine similarity (only on search results)
	CreatedAt   time.Time      `json:"created_at"`
	AccessedAt  time.Time      `json:"accessed_at"`
	AccessCount int            `json:"access_count"`
}

// Store provides vector-based memory storage backed by pgvector.
type Store struct {
	pool      *pgxpool.Pool
	dimension int
}

// NewStore creates a Store sharing the connection pool from PGStore.
func NewStore(pool *pgxpool.Pool, dimension int) *Store {
	if dimension <= 0 {
		dimension = 1536
	}
	return &Store{pool: pool, dimension: dimension}
}

// Migrate creates the loom_memory table with pgvector extension.
func (ms *Store) Migrate(ctx context.Context) error {
	// Enable pgvector extension.
	if _, err := ms.pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		return fmt.Errorf("memory: enable vector extension: %w", err)
	}

	ddl := fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS loom_memory (
			id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			namespace    TEXT NOT NULL,
			content      TEXT NOT NULL,
			embedding    vector(%d) NOT NULL,
			metadata     JSONB NOT NULL DEFAULT '{}',
			created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			accessed_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			access_count INT NOT NULL DEFAULT 0
		)
	`, ms.dimension)
	if _, err := ms.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("memory: create table: %w", err)
	}

	// Add columns if upgrading from an older schema.
	for _, col := range []struct{ name, def string }{
		{"accessed_at", "TIMESTAMPTZ NOT NULL DEFAULT NOW()"},
		{"access_count", "INT NOT NULL DEFAULT 0"},
	} {
		_, _ = ms.pool.Exec(ctx, fmt.Sprintf(
			`ALTER TABLE loom_memory ADD COLUMN IF NOT EXISTS %s %s`, col.name, col.def))
	}

	// Create HNSW index for fast cosine similarity search.
	_, err := ms.pool.Exec(ctx, `
		CREATE INDEX IF NOT EXISTS idx_loom_memory_embedding
		ON loom_memory USING hnsw (embedding vector_cosine_ops)
	`)
	if err != nil {
		return fmt.Errorf("memory: create index: %w", err)
	}

	// Index on namespace for filtering.
	_, err = ms.pool.Exec(ctx, `
		CREATE INDEX IF NOT EXISTS idx_loom_memory_namespace
		ON loom_memory (namespace)
	`)
	if err != nil {
		return fmt.Errorf("memory: create namespace index: %w", err)
	}

	return nil
}

// Put inserts a memory with its embedding vector.
func (ms *Store) Put(ctx context.Context, ns, content string, embedding []float32, metadata map[string]any) (string, error) {
	if metadata == nil {
		metadata = map[string]any{}
	}

	var id string
	err := ms.pool.QueryRow(ctx, `
		INSERT INTO loom_memory (namespace, content, embedding, metadata)
		VALUES ($1, $2, $3, $4)
		RETURNING id
	`, ns, content, vectorString(embedding), metadata).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("memory: store: %w", err)
	}
	return id, nil
}

// Search returns top-K most similar memories by cosine similarity.
func (ms *Store) Search(ctx context.Context, ns string, queryEmbedding []float32, topK int, threshold float64) ([]Record, error) {
	if topK <= 0 {
		topK = 5
	}
	if threshold <= 0 {
		threshold = 0.5
	}

	rows, err := ms.pool.Query(ctx, `
		SELECT id, namespace, content, metadata, created_at, accessed_at, access_count,
		       1 - (embedding <=> $2) AS score
		FROM loom_memory
		WHERE namespace = $1
		  AND 1 - (embedding <=> $2) >= $4
		ORDER BY embedding <=> $2
		LIMIT $3
	`, ns, vectorString(queryEmbedding), topK, threshold)
	if err != nil {
		return nil, fmt.Errorf("memory: search: %w", err)
	}
	defer rows.Close()

	records, err := scanRecords(rows)
	if err != nil {
		return nil, err
	}

	// Bump accessed_at and access_count for returned results.
	if len(records) > 0 {
		ids := make([]string, len(records))
		for i, r := range records {
			ids[i] = r.ID
		}
		_, _ = ms.pool.Exec(ctx, `
			UPDATE loom_memory
			SET accessed_at = NOW(), access_count = access_count + 1
			WHERE id = ANY($1)
		`, ids)
	}

	return records, nil
}

// List returns all memories in a namespace (paginated).
func (ms *Store) List(ctx context.Context, ns string, limit, offset int) ([]Record, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	rows, err := ms.pool.Query(ctx, `
		SELECT id, namespace, content, metadata, created_at, accessed_at, access_count, 0 AS score
		FROM loom_memory
		WHERE namespace = $1
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`, ns, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("memory: list: %w", err)
	}
	defer rows.Close()

	return scanRecords(rows)
}

// Delete removes a memory by ID within a namespace.
func (ms *Store) Delete(ctx context.Context, ns, id string) error {
	_, err := ms.pool.Exec(ctx, `
		DELETE FROM loom_memory WHERE namespace = $1 AND id = $2
	`, ns, id)
	if err != nil {
		return fmt.Errorf("memory: delete: %w", err)
	}
	return nil
}

// Count returns the number of memories in a namespace.
func (ms *Store) Count(ctx context.Context, ns string) (int, error) {
	var count int
	err := ms.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM loom_memory WHERE namespace = $1
	`, ns).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("memory: count: %w", err)
	}
	return count, nil
}

// CleanupStale removes memories that haven't been accessed since the given
// cutoff time and have been accessed fewer than minAccess times.
func (ms *Store) CleanupStale(ctx context.Context, ns string, cutoff time.Time, minAccess int) (int, error) {
	tag, err := ms.pool.Exec(ctx, `
		DELETE FROM loom_memory
		WHERE namespace = $1
		  AND accessed_at < $2
		  AND access_count < $3
	`, ns, cutoff, minAccess)
	if err != nil {
		return 0, fmt.Errorf("memory: cleanup: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// scanRecords reads Records from query rows.
func scanRecords(rows pgx.Rows) ([]Record, error) {
	var records []Record
	for rows.Next() {
		var r Record
		if err := rows.Scan(&r.ID, &r.Namespace, &r.Content, &r.Metadata, &r.CreatedAt, &r.AccessedAt, &r.AccessCount, &r.Score); err != nil {
			return nil, fmt.Errorf("memory: scan: %w", err)
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

// vectorString converts a float32 slice to pgvector literal format: "[0.1,0.2,0.3]".
func vectorString(v []float32) string {
	if len(v) == 0 {
		return "[]"
	}
	var b strings.Builder
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%g", f)
	}
	b.WriteByte(']')
	return b.String()
}
