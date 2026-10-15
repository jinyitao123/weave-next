package frozen

import (
	"context"
	"errors"
	"testing"

	"github.com/jinyitao123/weave/internal/base/execution"
)

func TestServiceCredentialOwnerMismatchUsesTrustedAuthorizationHook(t *testing.T) {
	ref := CredentialReference{
		SchemaVersion: FrozenSchemaVersion,
		Scope:         CredentialScopeWorkspaceService,
		ServiceID:     "provider:shared-models",
		WorkspaceID:   "workspace-a",
		Kind:          CredentialProviderAPIKey,
		ResourceID:    "provider-a",
		Slot:          "api_key",
	}
	subject := execution.Subject{WorkspaceID: "workspace-a", ServiceID: "capability-app:app-a"}
	ctx := execution.WithSubject(context.Background(), subject)
	if err := AuthorizeCredentialReference(ctx, ref); !errors.Is(err, ErrCredentialSubjectDenied) {
		t.Fatalf("mismatched service owner without a hook was not denied: %v", err)
	}

	calls := 0
	ctx = WithServiceReferenceAuthorization(ctx, func(_ context.Context, gotSubject execution.Subject, gotRef CredentialReference) error {
		calls++
		if gotSubject != subject || gotRef != ref {
			return errors.New("unexpected service authorization request")
		}
		return nil
	})
	if err := AuthorizeCredentialReference(ctx, ref); err != nil {
		t.Fatalf("trusted hook could not authorize the exact provider reference: %v", err)
	}
	if calls != 1 {
		t.Fatalf("authorization hook calls=%d, want 1", calls)
	}
}

func TestCredentialAuthorizationHookCannotCrossWorkspaceOrUserOwner(t *testing.T) {
	serviceRef := CredentialReference{
		SchemaVersion: FrozenSchemaVersion,
		Scope:         CredentialScopeWorkspaceService,
		ServiceID:     "provider:shared-models",
		WorkspaceID:   "workspace-a",
		Kind:          CredentialProviderAPIKey,
		ResourceID:    "provider-a",
		Slot:          "api_key",
	}
	calls := 0
	permissive := func(context.Context, execution.Subject, CredentialReference) error {
		calls++
		return nil
	}

	crossWorkspace := execution.WithSubject(context.Background(), execution.Subject{
		WorkspaceID: "workspace-b", ServiceID: "capability-app:app-a",
	})
	crossWorkspace = WithServiceReferenceAuthorization(crossWorkspace, permissive)
	if err := AuthorizeCredentialReference(crossWorkspace, serviceRef); !errors.Is(err, ErrCredentialSubjectDenied) {
		t.Fatalf("cross-workspace credential was not denied: %v", err)
	}
	if calls != 0 {
		t.Fatalf("cross-workspace path invoked hook %d times, want 0", calls)
	}

	otherUserRef := CredentialReference{
		SchemaVersion: FrozenSchemaVersion,
		Scope:         CredentialScopeUser,
		UserID:        "provider-owner",
		WorkspaceID:   "workspace-a",
		Kind:          CredentialProviderAPIKey,
		ResourceID:    "provider-private",
		Slot:          "api_key",
	}
	userContext := execution.WithSubject(context.Background(), execution.Subject{
		WorkspaceID: "workspace-a", UserID: "alice",
	})
	userContext = WithServiceReferenceAuthorization(userContext, permissive)
	if err := AuthorizeCredentialReference(userContext, otherUserRef); !errors.Is(err, ErrCredentialSubjectDenied) {
		t.Fatalf("user hook borrowed another owner's credential: %v", err)
	}
	if calls != 0 {
		t.Fatalf("non-service user invoked hook %d times, want 0", calls)
	}

	applicationContext := execution.WithSubject(context.Background(), execution.Subject{
		WorkspaceID: "workspace-a", ServiceID: "capability-app:app-a",
	})
	applicationContext = WithServiceReferenceAuthorization(applicationContext, permissive)
	if err := AuthorizeCredentialReference(applicationContext, otherUserRef); !errors.Is(err, ErrCredentialSubjectDenied) {
		t.Fatalf("capability application borrowed a user-owned provider credential: %v", err)
	}
	if calls != 0 {
		t.Fatalf("application user-credential path invoked hook %d times, want 0", calls)
	}
}

func TestCredentialAuthorizationHookDenialRemainsDenied(t *testing.T) {
	ref := CredentialReference{
		SchemaVersion: FrozenSchemaVersion,
		Scope:         CredentialScopeWorkspaceService,
		ServiceID:     "provider:shared-models",
		WorkspaceID:   "workspace-a",
		Kind:          CredentialProviderAPIKey,
		ResourceID:    "provider-a",
		Slot:          "api_key",
	}
	ctx := execution.WithSubject(context.Background(), execution.Subject{
		WorkspaceID: "workspace-a", ServiceID: "capability-app:app-a",
	})
	ctx = WithServiceReferenceAuthorization(ctx, func(context.Context, execution.Subject, CredentialReference) error {
		return errors.New("grant revoked")
	})
	if err := AuthorizeCredentialReference(ctx, ref); !errors.Is(err, ErrCredentialSubjectDenied) {
		t.Fatalf("denied service reference was accepted: %v", err)
	}
}
