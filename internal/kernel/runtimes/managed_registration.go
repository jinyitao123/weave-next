package runtimes

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
)

// NewRegistrationIdentity lets the Server durably save a Host identity before
// registering it, so a crash never silently replaces the Host or its spool.
func NewRegistrationIdentity() (id, token string, err error) {
	token, err = generateToken()
	return uuid.NewString(), token, err
}

func (s *Store) EnsureManagedRegistration(ctx context.Context, workspaceID, id, name, token string) (*Runtime, error) {
	if workspaceID == "" || id == "" || !strings.HasPrefix(token, tokenPrefix) || len(token) < 40 {
		return nil, errors.New("invalid managed runtime registration")
	}
	now := s.now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	// A container replacement can lose its local host identity while the old
	// registration remains in Postgres. Retire that stale managed slot before
	// installing the replacement; historical tasks keep their runtime ID.
	if _, err = tx.Exec(ctx, `UPDATE weave_runtimes
 SET enabled=false,revoked_at=COALESCE(revoked_at,$4),deleted_at=COALESCE(deleted_at,$4),updated_at=$4
 WHERE workspace_id=$1 AND name=$2 AND id<>$3 AND deleted_at IS NULL`, workspaceID, name, id, now); err != nil {
		return nil, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO weave_runtimes(id,workspace_id,name,engines,token_hash,functional_revision,enabled,created_at,updated_at)
 VALUES($1,$2,$3,'[]',$4,1,true,$5,$5) ON CONFLICT(id) DO NOTHING`, id, workspaceID, name, hashToken(token), now)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	runtime, err := s.ValidateToken(ctx, token)
	if err != nil {
		return nil, err
	}
	if runtime.ID != id || runtime.WorkspaceID != workspaceID {
		return nil, errors.New("managed runtime identity does not match persisted registration")
	}
	return runtime, nil
}
