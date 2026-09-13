package api

import (
	"context"
	"testing"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
)

func TestBindUserProviderOwnerUsesAuthenticatedSubject(t *testing.T) {
	ctx := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "workspace", UserID: "alice"})
	config := llmrouter.ProviderConfig{
		CredentialScope:  frozen.CredentialScopeWorkspaceService,
		CredentialUserID: "bob", CredentialServiceID: "untrusted",
	}
	if err := bindUserProviderOwner(ctx, "workspace", &config); err != nil {
		t.Fatal(err)
	}
	if config.CredentialScope != frozen.CredentialScopeUser ||
		config.CredentialUserID != "alice" || config.CredentialServiceID != "" {
		t.Fatalf("provider owner was not rebound to the authenticated user: %+v", config)
	}
}

func TestBindUserProviderOwnerRejectsServiceAndWorkspaceMismatch(t *testing.T) {
	service := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "workspace", ServiceID: "host"})
	if err := bindUserProviderOwner(service, "workspace", &llmrouter.ProviderConfig{}); err == nil {
		t.Fatal("service subject created a user credential")
	}
	alice := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "workspace", UserID: "alice"})
	if err := bindUserProviderOwner(alice, "other", &llmrouter.ProviderConfig{}); err == nil {
		t.Fatal("cross-workspace subject created a credential")
	}
}
