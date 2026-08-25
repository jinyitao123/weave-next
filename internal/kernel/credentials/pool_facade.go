package credentials

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

// PoolCredentialResolver opens a short-lived transaction for every credential
// operation. It never retains a transaction or resolved secret material.
type PoolCredentialResolver struct {
	pool        *pgxpool.Pool
	workspaceID string
	sources     TxCredentialSources
}

func NewPoolCredentialResolver(
	pool *pgxpool.Pool,
	workspaceID string,
	sources TxCredentialSources,
) (*PoolCredentialResolver, error) {
	if pool == nil ||
		workspaceID == "" ||
		workspaceID != strings.TrimSpace(workspaceID) ||
		!sources.available() {
		return nil, normalizeFacadeError(nil)
	}
	return &PoolCredentialResolver{pool: pool, workspaceID: workspaceID, sources: sources}, nil
}

func (r *PoolCredentialResolver) Validate(
	ctx context.Context,
	ref frozen.CredentialReference,
) error {
	if r == nil || r.pool == nil {
		return normalizeFacadeError(nil)
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return normalizeFacadeError(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	facade, err := NewTxCredentialFacade(tx, r.workspaceID, r.sources)
	if err != nil {
		return err
	}
	return facade.Validate(ctx, ref)
}

func (r *PoolCredentialResolver) Resolve(
	ctx context.Context,
	request ResolveRequest,
) (SecretMaterial, error) {
	if r == nil || r.pool == nil {
		return SecretMaterial{}, normalizeFacadeError(nil)
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return SecretMaterial{}, normalizeFacadeError(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	facade, err := NewTxCredentialFacade(tx, r.workspaceID, r.sources)
	if err != nil {
		return SecretMaterial{}, err
	}
	return facade.Resolve(ctx, request)
}
