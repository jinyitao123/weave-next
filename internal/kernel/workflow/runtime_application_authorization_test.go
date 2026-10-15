package workflow

import (
	"context"
	"errors"
	"testing"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

func TestPublishedCredentialAuthorizationComposesAndNarrowsApplicationGrant(t *testing.T) {
	allowed := applicationProviderReference("provider-a")
	subject := execution.Subject{WorkspaceID: "workspace-a", ServiceID: "capability-app:app-a"}
	ctx := execution.WithSubject(context.Background(), subject)
	downstreamCalls := 0
	ctx = frozen.WithServiceReferenceAuthorization(ctx, func(_ context.Context, gotSubject execution.Subject, gotRef frozen.CredentialReference) error {
		downstreamCalls++
		if gotSubject != subject || gotRef != allowed {
			return errors.New("unexpected live grant lookup")
		}
		return nil
	})
	ctx = withPublishedServiceCredentialAuthorization(ctx, applicationCredentialPayload(allowed))

	if err := frozen.AuthorizeCredentialReference(ctx, allowed); err != nil {
		t.Fatalf("declared provider reference was rejected: %v", err)
	}
	if downstreamCalls != 1 {
		t.Fatalf("downstream grant hook calls=%d, want 1", downstreamCalls)
	}

	unlisted := allowed
	unlisted.ResourceID = "provider-b"
	if err := frozen.AuthorizeCredentialReference(ctx, unlisted); !errors.Is(err, frozen.ErrCredentialSubjectDenied) {
		t.Fatalf("unlisted provider reference was accepted: %v", err)
	}
	if downstreamCalls != 1 {
		t.Fatalf("unlisted reference reached downstream hook; calls=%d", downstreamCalls)
	}

	version := int64(2)
	differentVersion := allowed
	differentVersion.CredentialVersion = &version
	composed := frozen.ServiceReferenceAuthorizationFromContext(ctx)
	if err := composed(ctx, subject, differentVersion); !errors.Is(err, execution.ErrSubjectMismatch) {
		t.Fatalf("different credential version passed the frozen-reference boundary: %v", err)
	}
	if downstreamCalls != 1 {
		t.Fatalf("different-version reference reached downstream hook; calls=%d", downstreamCalls)
	}
	if err := frozen.AuthorizeCredentialReference(ctx, differentVersion); !errors.Is(err, frozen.ErrCredentialSubjectDenied) {
		t.Fatalf("invalid versioned reference was not denied by the credential contract: %v", err)
	}
}

func TestPublishedCredentialAuthorizationDoesNotSwallowHookDenial(t *testing.T) {
	allowed := applicationProviderReference("provider-a")
	subject := execution.Subject{WorkspaceID: "workspace-a", ServiceID: "capability-app:app-a"}
	ctx := execution.WithSubject(context.Background(), subject)
	grantDenied := errors.New("live application grant revoked")
	ctx = frozen.WithServiceReferenceAuthorization(ctx, func(context.Context, execution.Subject, frozen.CredentialReference) error {
		return grantDenied
	})
	ctx = withPublishedServiceCredentialAuthorization(ctx, applicationCredentialPayload(allowed))

	composed := frozen.ServiceReferenceAuthorizationFromContext(ctx)
	if err := composed(ctx, subject, allowed); !errors.Is(err, grantDenied) {
		t.Fatalf("published authorization swallowed downstream denial: %v", err)
	}
	if err := frozen.AuthorizeCredentialReference(ctx, allowed); !errors.Is(err, frozen.ErrCredentialSubjectDenied) {
		t.Fatalf("revoked reference remained authorized: %v", err)
	}
}

func applicationProviderReference(provider string) frozen.CredentialReference {
	return frozen.CredentialReference{
		SchemaVersion: frozen.FrozenSchemaVersion,
		Scope:         frozen.CredentialScopeWorkspaceService,
		ServiceID:     "provider:" + provider,
		WorkspaceID:   "workspace-a",
		Kind:          frozen.CredentialProviderAPIKey,
		ResourceID:    provider,
		Slot:          "api_key",
	}
}

func applicationCredentialPayload(ref frozen.CredentialReference) frozen.ArtifactPayloadV1 {
	return frozen.ArtifactPayloadV1{
		Bundles: []frozen.FrozenExecutionBundle{{Credentials: []frozen.CredentialReference{ref}}},
	}
}
