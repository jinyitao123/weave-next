package teamevaluations

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type IdempotencyRecord struct {
	WorkspaceID string
	Key         uuid.UUID
	Fingerprint string
	BuildRunID  string
	TeamID      string
	CreatedBy   string
}

type IdempotencyStore interface {
	Claim(context.Context, IdempotencyRecord) (IdempotencyRecord, error)
}

type PGIdempotencyStore struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

func NewPGIdempotencyStore(pool *pgxpool.Pool) *PGIdempotencyStore {
	return &PGIdempotencyStore{pool: pool, now: time.Now}
}

func (s *PGIdempotencyStore) Claim(ctx context.Context, requested IdempotencyRecord) (IdempotencyRecord, error) {
	if s == nil || s.pool == nil {
		return IdempotencyRecord{}, ErrUnavailable
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO weave_team_evaluation_requests (
			workspace_id, idempotency_key, request_fingerprint,
			build_run_id, team_id, created_by, created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (workspace_id, idempotency_key) DO NOTHING
	`, requested.WorkspaceID, requested.Key, requested.Fingerprint,
		requested.BuildRunID, requested.TeamID, requested.CreatedBy, s.now().UTC())
	if err != nil {
		return IdempotencyRecord{}, fmt.Errorf("claim team evaluation request: %w", err)
	}
	var found IdempotencyRecord
	err = s.pool.QueryRow(ctx, `
		SELECT workspace_id, idempotency_key, request_fingerprint,
			build_run_id, team_id, created_by
		FROM weave_team_evaluation_requests
		WHERE workspace_id=$1 AND idempotency_key=$2
	`, requested.WorkspaceID, requested.Key).Scan(
		&found.WorkspaceID, &found.Key, &found.Fingerprint,
		&found.BuildRunID, &found.TeamID, &found.CreatedBy,
	)
	if err != nil {
		return IdempotencyRecord{}, fmt.Errorf("read team evaluation request claim: %w", err)
	}
	return found, nil
}
