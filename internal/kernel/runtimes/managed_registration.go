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
	// The embedded Host identity normally survives restarts on disk. If an old
	// deployment did not persist that directory, reclaim the reserved managed
	// runtime name and rotate its token instead of making the whole Server fail
	// to start on the active-name uniqueness constraint.
	runtime, err := scanRuntime(s.pool.QueryRow(ctx, `
		INSERT INTO weave_runtimes(
			id,workspace_id,name,engines,token_hash,functional_revision,enabled,created_at,updated_at
		) VALUES($1,$2,$3,'[]',$4,1,true,$5,$5)
		ON CONFLICT (workspace_id,name) WHERE deleted_at IS NULL
		DO UPDATE SET
			token_hash=EXCLUDED.token_hash,
			enabled=true,
			revoked_at=NULL,
			updated_at=EXCLUDED.updated_at
		RETURNING `+runtimeColumns,
		id, workspaceID, name, hashToken(token), s.now()))
	if err != nil {
		return nil, err
	}
	if runtime.WorkspaceID != workspaceID || runtime.Name != name {
		return nil, errors.New("managed runtime identity does not match persisted registration")
	}
	return runtime, nil
}
