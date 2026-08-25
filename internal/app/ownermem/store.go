// Package ownermem persists bounded, structured profiles about conversation
// participants. It is independent from the vector memory package.
package ownermem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrVersionConflict reports a failed optimistic-lock update.
var ErrVersionConflict = errors.New("owner memory profile version conflict")

// Clock supplies timestamps for profile updates.
type Clock interface {
	Now() time.Time
}

// RealClock is the production wall clock.
type RealClock struct{}

// Now returns the current wall-clock time.
func (RealClock) Now() time.Time { return time.Now() }

// Profile is the bounded memory attached to one workspace/agent/user tuple.
type Profile struct {
	WorkspaceID string            `json:"workspace_id"`
	AgentID     string            `json:"agent_id"`
	UserID      string            `json:"user_id"`
	Slots       map[string]string `json:"slots"`
	Version     int               `json:"version"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// Store persists owner memory profiles.
type Store struct {
	pool  *pgxpool.Pool
	clock Clock
}

// New creates a profile store with an injected clock.
func New(pool *pgxpool.Pool, clock Clock) *Store {
	if clock == nil {
		clock = RealClock{}
	}
	return &Store{pool: pool, clock: clock}
}

// Get returns the profile for the exact workspace/agent/user tuple. A missing
// row is represented by an empty version-zero profile.
func (s *Store) Get(ctx context.Context, workspaceID, agentID, userID string) (Profile, error) {
	profile := Profile{
		WorkspaceID: workspaceID,
		AgentID:     agentID,
		UserID:      userID,
		Slots:       map[string]string{},
	}
	var slotsJSON []byte
	err := s.pool.QueryRow(ctx, `
		SELECT slots, version, updated_at
		FROM agent_memory_profile
		WHERE workspace_id=$1 AND agent_id=$2 AND user_id=$3
	`, workspaceID, agentID, userID).Scan(&slotsJSON, &profile.Version, &profile.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return profile, nil
	}
	if err != nil {
		return Profile{}, fmt.Errorf("get owner memory profile: %w", err)
	}
	if err := json.Unmarshal(slotsJSON, &profile.Slots); err != nil {
		return Profile{}, fmt.Errorf("decode owner memory profile slots: %w", err)
	}
	if profile.Slots == nil {
		profile.Slots = map[string]string{}
	}
	return profile, nil
}

// Upsert applies a full-profile read-modify-write guarded by expectedVersion.
// Callers must re-read and retry after ErrVersionConflict.
func (s *Store) Upsert(
	ctx context.Context,
	workspaceID, agentID, userID string,
	slots map[string]string,
	expectedVersion int,
) error {
	data, err := json.Marshal(slots)
	if err != nil {
		return fmt.Errorf("encode owner memory profile slots: %w", err)
	}
	result, err := s.pool.Exec(ctx, `
		INSERT INTO agent_memory_profile (
			workspace_id, agent_id, user_id, slots, version, updated_at
		)
		SELECT $1, $2, $3, $4, 1, $6
		WHERE $5=0 OR EXISTS (
			SELECT 1 FROM agent_memory_profile
			WHERE workspace_id=$1 AND agent_id=$2 AND user_id=$3
		)
		ON CONFLICT (workspace_id, agent_id, user_id) DO UPDATE SET
			slots=EXCLUDED.slots,
			version=agent_memory_profile.version+1,
			updated_at=EXCLUDED.updated_at
		WHERE agent_memory_profile.version=$5
	`, workspaceID, agentID, userID, data, expectedVersion, s.clock.Now())
	if err != nil {
		return fmt.Errorf("upsert owner memory profile: %w", err)
	}
	if result.RowsAffected() == 0 {
		return ErrVersionConflict
	}
	return nil
}
