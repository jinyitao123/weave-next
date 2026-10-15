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

func TestPublishedApplicationGrantComposesWithExactFrozenReference(t *testing.T) {
	allowed := frozen.CredentialReference{SchemaVersion: frozen.FrozenSchemaVersion,
		Scope: frozen.CredentialScopeWorkspaceService, ServiceID: "provider:provider", WorkspaceID: "workspace",
		Kind: frozen.CredentialProviderAPIKey, ResourceID: "provider", Slot: "api_key"}
	app := execution.Subject{WorkspaceID: "workspace", ServiceID: "capability-app:app-a"}
	ctx := execution.WithSubject(context.Background(), app)
	ctx = frozen.WithServiceReferenceAuthorization(ctx, func(_ context.Context, subject execution.Subject, ref frozen.CredentialReference) error {
		if subject == app && ref == allowed {
			return nil
		}
		return errors.New("application grant denied")
	})
	ctx = withPublishedServiceCredentialAuthorization(ctx, frozen.ArtifactPayloadV1{
		Bundles: []frozen.FrozenExecutionBundle{{Credentials: []frozen.CredentialReference{allowed}}},
	})
	if err := frozen.AuthorizeCredentialReference(ctx, allowed); err != nil {
		t.Fatalf("exact provider reference under application grant rejected: %v", err)
	}
	userOwned := allowed
	userOwned.Scope, userOwned.ServiceID, userOwned.UserID = frozen.CredentialScopeUser, "", "provider-owner"
	if err := frozen.AuthorizeCredentialReference(ctx, userOwned); !errors.Is(err, frozen.ErrCredentialSubjectDenied) {
		t.Fatalf("application subject borrowed a user-owned provider credential: %v", err)
	}
	other := allowed
	other.ResourceID = "other-provider"
	if err := frozen.AuthorizeCredentialReference(ctx, other); !errors.Is(err, frozen.ErrCredentialSubjectDenied) {
		t.Fatalf("provider reference outside frozen bundle error = %v", err)
	}
	otherApp := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "workspace", ServiceID: "capability-app:app-b"})
	otherApp = withPublishedServiceCredentialAuthorization(otherApp, frozen.ArtifactPayloadV1{
		Bundles: []frozen.FrozenExecutionBundle{{Credentials: []frozen.CredentialReference{allowed}}},
	})
	if err := frozen.AuthorizeCredentialReference(otherApp, allowed); !errors.Is(err, frozen.ErrCredentialSubjectDenied) {
		t.Fatalf("provider reference crossed application subjects: %v", err)
	}
}

func TestApplicationSubjectNeedsExactPublishedReferenceAndLiveGrant(t *testing.T) {
	allowed := frozen.CredentialReference{
		SchemaVersion: frozen.FrozenSchemaVersion, Scope: frozen.CredentialScopeWorkspaceService,
		ServiceID: "provider:provider", WorkspaceID: "workspace", Kind: frozen.CredentialProviderAPIKey,
		ResourceID: "provider", Slot: "api_key",
	}
	appSubject := execution.Subject{WorkspaceID: "workspace", ServiceID: "capability-app:app-a"}
	ctx := execution.WithSubject(context.Background(), appSubject)
	ctx = frozen.WithServiceReferenceAuthorization(ctx, func(_ context.Context, subject execution.Subject, ref frozen.CredentialReference) error {
		if subject == appSubject && ref == allowed {
			return nil
		}
		return errors.New("grant denied")
	})
	ctx = withPublishedServiceCredentialAuthorization(ctx, frozen.ArtifactPayloadV1{
		Bundles: []frozen.FrozenExecutionBundle{{Credentials: []frozen.CredentialReference{allowed}}},
	})
	if err := frozen.AuthorizeCredentialReference(ctx, allowed); err != nil {
		t.Fatalf("exact application-granted provider reference was rejected: %v", err)
	}
	other := allowed
	other.ResourceID = "other-provider"
	if err := frozen.AuthorizeCredentialReference(ctx, other); !errors.Is(err, frozen.ErrCredentialSubjectDenied) {
		t.Fatalf("application reference escaped the frozen bundle: %v", err)
	}
	revoked := execution.WithSubject(context.Background(), appSubject)
	revoked = frozen.WithServiceReferenceAuthorization(revoked, func(context.Context, execution.Subject, frozen.CredentialReference) error {
		return errors.New("grant revoked")
	})
	revoked = withPublishedServiceCredentialAuthorization(revoked, frozen.ArtifactPayloadV1{
		Bundles: []frozen.FrozenExecutionBundle{{Credentials: []frozen.CredentialReference{allowed}}},
	})
	if err := frozen.AuthorizeCredentialReference(revoked, allowed); !errors.Is(err, frozen.ErrCredentialSubjectDenied) {
		t.Fatalf("revoked application grant remained usable: %v", err)
	}
	otherApp := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "workspace", ServiceID: "capability-app:app-b"})
	otherApp = withPublishedServiceCredentialAuthorization(otherApp, frozen.ArtifactPayloadV1{
		Bundles: []frozen.FrozenExecutionBundle{{Credentials: []frozen.CredentialReference{allowed}}},
	})
	if err := frozen.AuthorizeCredentialReference(otherApp, allowed); !errors.Is(err, frozen.ErrCredentialSubjectDenied) {
		t.Fatalf("another application resolved the provider reference without a grant: %v", err)
	}
}
