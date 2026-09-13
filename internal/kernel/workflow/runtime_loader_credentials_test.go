package workflow

import (
	"context"
	"errors"
	"testing"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

func TestPublishedSnapshotAuthorizesOnlyItsSharedCredentialReferences(t *testing.T) {
	allowed := frozen.CredentialReference{
		SchemaVersion: frozen.FrozenSchemaVersion,
		Scope:         frozen.CredentialScopeWorkspaceService, ServiceID: "provider:shared",
		WorkspaceID: "workspace", Kind: frozen.CredentialProviderAPIKey,
		ResourceID: "shared", Slot: "api_key",
	}
	ctx := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "workspace", UserID: "alice"})
	ctx = withPublishedServiceCredentialAuthorization(ctx, frozen.ArtifactPayloadV1{
		Bundles: []frozen.FrozenExecutionBundle{{Credentials: []frozen.CredentialReference{allowed}}},
	})

	if err := frozen.AuthorizeCredentialReference(ctx, allowed); err != nil {
		t.Fatalf("published reference rejected: %v", err)
	}
	other := allowed
	other.ResourceID = "other"
	if err := frozen.AuthorizeCredentialReference(ctx, other); !errors.Is(err, frozen.ErrCredentialSubjectDenied) {
		t.Fatalf("unpublished reference error = %v", err)
	}
	otherWorkspace := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "other", UserID: "alice"})
	otherWorkspace = withPublishedServiceCredentialAuthorization(otherWorkspace, frozen.ArtifactPayloadV1{
		Bundles: []frozen.FrozenExecutionBundle{{Credentials: []frozen.CredentialReference{allowed}}},
	})
	if err := frozen.AuthorizeCredentialReference(otherWorkspace, allowed); !errors.Is(err, frozen.ErrCredentialSubjectDenied) {
		t.Fatalf("cross-workspace reference error = %v", err)
	}
}
