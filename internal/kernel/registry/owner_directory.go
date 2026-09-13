package registry

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// OwnerQuery preserves the current PostgreSQL transaction when the application
// verifies a live account and its workspace membership.
type OwnerQuery interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}
type WorkspaceMemberVerifier func(context.Context, OwnerQuery, string, string) (bool, error)

var ErrOwnerDirectoryUnavailable = errors.New("agent owner directory is unavailable")

type Option func(*AgentRegistry)

func WithWorkspaceMemberVerifier(verify WorkspaceMemberVerifier) Option {
	return func(r *AgentRegistry) { r.memberVerifier = verify }
}

func (r *AgentRegistry) workspaceMemberExists(ctx context.Context, q OwnerQuery, workspaceID, userID string) (bool, error) {
	if r.memberVerifier == nil {
		return false, ErrOwnerDirectoryUnavailable
	}
	return r.memberVerifier(ctx, q, workspaceID, userID)
}
