package llmrouter

import (
	"context"
	"errors"
	"testing"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

type claimProviderSource struct{ configs []ProviderConfig }

func (s *claimProviderSource) List(context.Context, string) ([]ProviderConfig, error) {
	return append([]ProviderConfig(nil), s.configs...), nil
}

func TestClaimProviderResolvesFreshUserOrExplicitServiceCredentials(t *testing.T) {
	source := &claimProviderSource{configs: []ProviderConfig{
		{ID: "alice", BaseURL: "https://provider.invalid/v1", APIKey: "alice-key", Models: []string{"model"}, CredentialScope: frozen.CredentialScopeUser, CredentialUserID: "alice"},
		{ID: "bob", BaseURL: "https://provider.invalid/v1", APIKey: "bob-key", Models: []string{"model"}, CredentialScope: frozen.CredentialScopeUser, CredentialUserID: "bob"},
		{ID: "shared", BaseURL: "https://provider.invalid/v1", APIKey: "service-key", Models: []string{"shared-model"}, CredentialScope: frozen.CredentialScopeWorkspaceService, CredentialServiceID: "shared-models"},
	}}
	resolver := NewResolver(New("model"))
	resolver.SetSource(source)
	ctxFor := func(user string) context.Context {
		return execution.WithSubject(t.Context(), execution.Subject{WorkspaceID: "workspace", UserID: user})
	}
	alice, bob := ctxFor("alice"), ctxFor("bob")
	if _, err := resolver.ForWorkspace(alice, "workspace"); err != nil {
		t.Fatal(err)
	} // populate the ordinary route cache
	for _, tc := range []struct {
		ctx  context.Context
		want string
	}{{alice, "alice-key"}, {bob, "bob-key"}} {
		cfg, err := resolver.ResolveProviderConfiguration(tc.ctx, "workspace", "model")
		if err != nil || cfg.APIKey != tc.want {
			t.Fatalf("wrong claim credential: %q %v", cfg.APIKey, err)
		}
	}
	source.configs[0].APIKey = "rotated-alice"
	cfg, err := resolver.ResolveProviderConfiguration(alice, "workspace", "model")
	if err != nil || cfg.APIKey != "rotated-alice" {
		t.Fatal("physical claim reused cached credential")
	}
	cfg.Models[0] = "tampered"
	if source.configs[0].Models[0] != "model" {
		t.Fatal("claim mutated stored model route")
	}
	for _, ctx := range []context.Context{context.Background(), execution.WithSubject(t.Context(), execution.Subject{WorkspaceID: "foreign", UserID: "alice"}), ctxFor("charlie")} {
		if _, err := resolver.ResolveProviderConfiguration(ctx, "workspace", "model"); err == nil {
			t.Fatal("claim accepted missing, foreign, or unconfigured actor")
		}
	}
	if _, err := resolver.ResolveProviderConfiguration(alice, "workspace", "shared-model"); err == nil {
		t.Fatal("implicit service fallback")
	}
	granted := frozen.WithServiceReferenceAuthorization(alice, func(_ context.Context, subject execution.Subject, ref frozen.CredentialReference) error {
		if subject.UserID == "alice" && ref.ServiceID == "shared-models" && ref.ResourceID == "shared" {
			return nil
		}
		return errors.New("denied")
	})
	cfg, err = resolver.ResolveProviderConfiguration(granted, "workspace", "shared-model")
	if err != nil || cfg.APIKey != "service-key" {
		t.Fatalf("explicit shared credential unavailable: %v", err)
	}
	if _, err := resolver.ResolveProviderConfiguration(alice, "workspace", "shared-model"); err == nil {
		t.Fatal("previous grant leaked to new claim")
	}
	source.configs = source.configs[1:]
	if _, err := resolver.ResolveProviderConfiguration(alice, "workspace", "model"); err == nil {
		t.Fatal("deleted personal provider silently fell back")
	}
}
