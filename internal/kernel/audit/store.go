// Package audit keeps the historical MCP audit interfaces as no-op compatibility
// shims. Product audit persistence was removed from the core runtime path.
package audit

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Clock supplies timestamps for audit records.
type Clock interface {
	Now() time.Time
}

// RealClock is the production wall clock.
type RealClock struct{}

// Now returns the current wall-clock time.
func (RealClock) Now() time.Time { return time.Now() }

// Entry is one MCP tool invocation audit record.
type Entry struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	Agent       string    `json:"agent"`
	ServerID    string    `json:"server_id,omitempty"`
	Tool        string    `json:"tool"`
	Status      string    `json:"status"`
	Detail      string    `json:"detail"`
	CreatedAt   time.Time `json:"created_at"`
}

// Store accepts audit calls without persisting them.
type Store struct {
	pool  *pgxpool.Pool
	clock Clock
}

// New creates a no-op MCP audit store.
func New(pool *pgxpool.Pool, clock Clock) *Store {
	if clock == nil {
		clock = RealClock{}
	}
	return &Store{pool: pool, clock: clock}
}

// Record accepts one MCP audit record without persisting it.
func (s *Store) Record(
	ctx context.Context,
	workspaceID, agent, tool, status, detail string,
) error {
	_, _, _, _, _, _ = ctx, workspaceID, agent, tool, status, detail
	return nil
}

// RecordServer accepts one registry-ref invocation without persisting it.
func (s *Store) RecordServer(
	ctx context.Context,
	workspaceID, agent, serverID, tool, status, detail string,
) error {
	_, _, _, _, _, _, _ = ctx, workspaceID, agent, serverID, tool, status, detail
	return nil
}

// List returns no persisted MCP audit records.
func (s *Store) List(
	ctx context.Context,
	workspaceID, agent string,
	limit, offset int,
) ([]Entry, error) {
	_, _, _, _, _ = ctx, workspaceID, agent, limit, offset
	return nil, nil
}
