package credentials

import (
	"context"

	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
)

// AuthorizeReference is checked before any secret read, including direct
// source calls that do not pass through a runtime facade. A source called by
// the facade that just authorized the same reference in the same transaction
// is not authorized a second time.
func AuthorizeReference(ctx context.Context, ref frozen.CredentialReference) error {
	if authorizedByFacade(ctx, ref) {
		return nil
	}
	if err := frozen.AuthorizeCredentialReference(ctx, ref); err != nil {
		return coded(CodeCredentialUnavailable, "credential subject is not authorized")
	}
	return nil
}

func providerConfigReference(workspaceID string, cfg llmrouter.ProviderConfig) frozen.CredentialReference {
	return frozen.CredentialReference{SchemaVersion: frozen.FrozenSchemaVersion, WorkspaceID: workspaceID,
		Scope: cfg.CredentialScope, UserID: cfg.CredentialUserID, ServiceID: cfg.CredentialServiceID,
		Kind: frozen.CredentialProviderAPIKey, ResourceID: cfg.ID, Slot: "api_key"}
}

func headReference(head ProviderHead) frozen.CredentialReference {
	return frozen.CredentialReference{SchemaVersion: frozen.FrozenSchemaVersion, WorkspaceID: head.WorkspaceID,
		Scope: head.CredentialScope, UserID: head.CredentialUserID, ServiceID: head.CredentialServiceID,
		Kind: frozen.CredentialProviderAPIKey, ResourceID: head.ID, Slot: "api_key"}
}

func WithServiceReferenceAuthorization(ctx context.Context, authorize frozen.ServiceReferenceAuthorization) context.Context {
	return frozen.WithServiceReferenceAuthorization(ctx, authorize)
}

// facadeAuthorization marks the reference a TxCredentialFacade authorized and
// the transaction it did so in. Only this package can set it.
type facadeAuthorization struct {
	ref     frozen.CredentialReference
	queries ReferenceAuthorizationQueries
}

type facadeAuthorizationKey struct{}

func withFacadeAuthorization(ctx context.Context, ref frozen.CredentialReference) context.Context {
	queries := ReferenceAuthorizationQueriesFromContext(ctx)
	if queries == nil {
		return ctx
	}
	return context.WithValue(ctx, facadeAuthorizationKey{}, facadeAuthorization{ref: ref, queries: queries})
}

func authorizedByFacade(ctx context.Context, ref frozen.CredentialReference) bool {
	marked, ok := ctx.Value(facadeAuthorizationKey{}).(facadeAuthorization)
	if !ok || marked.ref != ref {
		return false
	}
	queries := ReferenceAuthorizationQueriesFromContext(ctx)
	return queries != nil && queries == marked.queries
}
