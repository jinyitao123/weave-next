package api

import (
	"context"

	"github.com/jinyitao123/weave/internal/app/ownermem"
)

// OwnerMemoryStore is the workspace-, agent-, and user-scoped profile store
// used by the profile administration API.
type OwnerMemoryStore interface {
	Get(ctx context.Context, workspaceID, agentID, userID string) (ownermem.Profile, error)
	Upsert(ctx context.Context, workspaceID, agentID, userID string, slots map[string]string, expectedVersion int) error
}
