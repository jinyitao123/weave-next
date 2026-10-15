package credentials

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

// ReferenceAuthorizationQueries is the read-only transaction port available
// only while PoolCredentialResolver is validating or resolving one exact
// frozen reference. Server authorization hooks can reuse the same connection
// without opening a nested pool transaction.
type ReferenceAuthorizationQueries interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

type referenceAuthorizationQueriesKey struct{}

func withReferenceAuthorizationQueries(ctx context.Context, queries ReferenceAuthorizationQueries) context.Context {
	return context.WithValue(ctx, referenceAuthorizationQueriesKey{}, queries)
}

// ReferenceAuthorizationQueriesFromContext returns the current resolver-owned
// read transaction when called from a credential validation or resolution.
func ReferenceAuthorizationQueriesFromContext(ctx context.Context) ReferenceAuthorizationQueries {
	queries, _ := ctx.Value(referenceAuthorizationQueriesKey{}).(ReferenceAuthorizationQueries)
	return queries
}

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
	ctx = withReferenceAuthorizationQueries(ctx, tx)

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
	ctx = withReferenceAuthorizationQueries(ctx, tx)

	facade, err := NewTxCredentialFacade(tx, r.workspaceID, r.sources)
	if err != nil {
		return SecretMaterial{}, err
	}
	return facade.Resolve(ctx, request)
}
