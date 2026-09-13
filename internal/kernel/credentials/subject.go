package credentials

import (
	"context"

	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
)

// AuthorizeReference is checked before any secret read, including direct
// source calls that do not pass through a runtime facade.
func AuthorizeReference(ctx context.Context, ref frozen.CredentialReference) error {
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
