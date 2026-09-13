package frozen

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/jinyitao123/weave/internal/base/execution"
)

func TestCredentialReferenceRequiresExactTrustedOwner(t *testing.T) {
	personal := CredentialReference{SchemaVersion: 1, WorkspaceID: "workspace", Scope: CredentialScopeUser, UserID: "alice", Kind: CredentialProviderAPIKey, ResourceID: "provider", Slot: "api_key"}
	service := personal
	service.Scope = CredentialScopeWorkspaceService
	service.UserID = ""
	service.ServiceID = "shared-models"
	alice := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "workspace", UserID: "alice"})
	bob := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "workspace", UserID: "bob"})
	for _, test := range []struct {
		ctx     context.Context
		ref     CredentialReference
		allowed bool
	}{
		{alice, personal, true}, {bob, personal, false}, {context.Background(), personal, false}, {alice, service, false},
		{execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "workspace", ServiceID: "shared-models"}), service, true},
		{execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "workspace", ServiceID: "different"}), service, false},
		{execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "other", UserID: "alice"}), personal, false},
	} {
		if got := AuthorizeCredentialReference(test.ctx, test.ref) == nil; got != test.allowed {
			t.Fatalf("allowed=%v wanted=%v", got, test.allowed)
		}
	}
	granted := WithServiceReferenceAuthorization(alice, func(_ context.Context, subject execution.Subject, ref CredentialReference) error {
		if subject.UserID == "alice" && ref == service {
			return nil
		}
		return errors.New("not granted")
	})
	if err := AuthorizeCredentialReference(granted, service); err != nil {
		t.Fatal(err)
	}
	if err := AuthorizeCredentialReference(execution.WithSubject(granted, execution.Subject{WorkspaceID: "workspace", UserID: "bob"}), service); err == nil {
		t.Fatal("authorization callback was bypassed for another user")
	}
	for _, bad := range []CredentialReference{
		{SchemaVersion: 1, WorkspaceID: "workspace", Kind: CredentialProviderAPIKey, ResourceID: "provider", Slot: "api_key"},
		{SchemaVersion: 1, WorkspaceID: "workspace", Scope: CredentialScopeWorkspaceService, Kind: CredentialProviderAPIKey, ResourceID: "provider", Slot: "api_key"},
		{SchemaVersion: 1, WorkspaceID: "workspace", Scope: CredentialScopeUser, UserID: "alice", ServiceID: "shared-models", Kind: CredentialProviderAPIKey, ResourceID: "provider", Slot: "api_key"},
	} {
		if ValidateCredentialReference(bad) == nil {
			t.Fatal("ambiguous credential owner accepted")
		}
	}
	first, err := HashDTO(personal, PreorderCredentialReference)
	if err != nil {
		t.Fatal(err)
	}
	personal.UserID = "bob"
	second, err := HashDTO(personal, PreorderCredentialReference)
	if err != nil || first == second {
		t.Fatal("credential owner not frozen in digest")
	}
}

func TestScopedCredentialCanonicalFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/frozen-jcs-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Input     FrozenExecutionBundle `json:"input"`
		Canonical string                `json:"canonical"`
		SHA256    string                `json:"sha256"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	canonical, err := Canonicalize(fixture.Input.PrimaryModel.CredentialRef, PreorderCredentialReference)
	if err != nil {
		t.Fatal(err)
	}
	var expected struct {
		PrimaryModel struct {
			CredentialRef json.RawMessage `json:"credential_ref"`
		} `json:"primary_model"`
	}
	if err := json.Unmarshal([]byte(fixture.Canonical), &expected); err != nil {
		t.Fatal(err)
	}
	if string(canonical) != string(expected.PrimaryModel.CredentialRef) {
		t.Fatal("scoped reference canonical bytes differ")
	}
	digest, err := HashCanonicalJSON([]byte(fixture.Canonical))
	if err != nil || digest != fixture.SHA256 {
		t.Fatal("scoped reference fixture digest differs")
	}
}
