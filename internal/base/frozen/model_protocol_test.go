package frozen

import (
	"bytes"
	"testing"
)

func protocolTestBinding(protocol string) FrozenModelBinding {
	return FrozenModelBinding{
		SchemaVersion: 1, WorkspaceID: "ws", ProviderID: "nimble", ProviderRevision: 1,
		ModelID: "nimble:latest", BaseURL: "http://models.invalid", Protocol: protocol,
		CredentialRef: CredentialReference{SchemaVersion: 1, WorkspaceID: "ws", Kind: CredentialProviderAPIKey,
			ResourceID: "nimble", Slot: "api_key", Scope: CredentialScopeWorkspaceService, ServiceID: "workspace-provider:nimble"},
	}
}

func TestFrozenModelBindingProtocolEntersHashOnlyWhenNotDefault(t *testing.T) {
	canonical, err := Canonicalize(protocolTestBinding(""), PreorderFrozenModelBinding)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(canonical, []byte("protocol")) {
		t.Fatalf("default protocol changed pre-protocol canonical bytes: %s", canonical)
	}
	openai, err := HashDTO(protocolTestBinding(""), PreorderFrozenModelBinding)
	if err != nil {
		t.Fatal(err)
	}
	systemOne, err := HashDTO(protocolTestBinding(ModelProtocolSystemOne), PreorderFrozenModelBinding)
	if err != nil {
		t.Fatal(err)
	}
	if openai == systemOne {
		t.Fatal("protocol is not part of the frozen model binding hash")
	}
	for _, protocol := range []string{"openai", "anthropic", "SystemOne"} {
		if _, err := HashDTO(protocolTestBinding(protocol), PreorderFrozenModelBinding); err == nil {
			t.Fatalf("frozen protocol %q accepted", protocol)
		}
	}
}
