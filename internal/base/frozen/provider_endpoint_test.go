package frozen

import (
	"errors"
	"testing"
)

func TestProviderEndpointKeepsTheAddressItWasGiven(t *testing.T) {
	for _, tc := range []struct {
		name, protocol, base, path, url string
	}{
		{"origin and default path", "openai", "https://api.example.test", "/v1/chat/completions", "https://api.example.test/v1/chat/completions"},
		{"trailing slash", "openai", "https://api.example.test/", "/v1/chat/completions", "https://api.example.test/v1/chat/completions"},
		{"sdk style base with its own path", "openai", "https://api.example.test/v1", "/chat/completions", "https://api.example.test/v1/chat/completions"},
		{"gateway prefix is kept", "openai", "https://gw.example.test/openai/team-a", "/v1/chat/completions", "https://gw.example.test/openai/team-a/v1/chat/completions"},
		{"custom path", "openai", "https://open.example.test", "/api/paas/v4/chat/completions", "https://open.example.test/api/paas/v4/chat/completions"},
		{"systemone", ModelProtocolSystemOne, "https://decide.example.test", "/v1/systemone", "https://decide.example.test/v1/systemone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			endpoint, err := NormalizeProviderEndpoint(tc.protocol, tc.base, tc.path)
			if err != nil {
				t.Fatal(err)
			}
			if endpoint.URL() != tc.url {
				t.Fatalf("url = %s, want %s", endpoint.URL(), tc.url)
			}
			if err := ValidateProviderEndpoint(tc.protocol, endpoint.BaseURL, endpoint.ChatPath); err != nil {
				t.Fatalf("stored form refused: %v", err)
			}
		})
	}
}

func TestProviderEndpointWithoutAPathKeepsTheLegacyAddress(t *testing.T) {
	for _, tc := range []struct{ protocol, base, url string }{
		{"openai", "https://api.example.test", "https://api.example.test/v1/chat/completions"},
		{"openai", "https://api.example.test/v1", "https://api.example.test/v1/chat/completions"},
		{"", "https://api.example.test/v1/", "https://api.example.test/v1/chat/completions"},
		{ModelProtocolSystemOne, "https://decide.example.test/v1", "https://decide.example.test/v1/systemone"},
	} {
		if got := ResolveProviderEndpoint(tc.protocol, tc.base, "").URL(); got != tc.url {
			t.Errorf("%s %s: url = %s, want %s", tc.protocol, tc.base, got, tc.url)
		}
	}
	if err := ValidateProviderEndpoint("openai", "https://api.example.test/v1", ""); err != nil {
		t.Fatalf("legacy endpoint refused: %v", err)
	}
}

func TestProviderEndpointRefusesAmbiguousAddressesWithASuggestion(t *testing.T) {
	for _, tc := range []struct {
		name, base, path, reason, suggested string
	}{
		{"sdk style base with the default path", "https://api.example.test/v1", "/v1/chat/completions", EndpointDuplicatePathSegment, "https://api.example.test/v1/chat/completions"},
		{"full endpoint as base", "https://api.example.test/v1/chat/completions", "/v1/chat/completions", EndpointInBaseURL, "https://api.example.test/v1/chat/completions"},
		{"full gateway endpoint as base", "https://gw.example.test/p/v1/chat/completions/", "/v1/chat/completions", EndpointInBaseURL, "https://gw.example.test/p/v1/chat/completions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NormalizeProviderEndpoint("openai", tc.base, tc.path)
			var refused *EndpointError
			if !errors.As(err, &refused) || refused.Reason != tc.reason {
				t.Fatalf("err = %v, want %s", err, tc.reason)
			}
			if refused.Suggestion == nil || refused.Suggestion.URL() != tc.suggested {
				t.Fatalf("suggestion = %+v, want %s", refused.Suggestion, tc.suggested)
			}
			// The suggestion is itself an accepted endpoint for the same address.
			if accepted, err := NormalizeProviderEndpoint("openai", refused.Suggestion.BaseURL, refused.Suggestion.ChatPath); err != nil || accepted.URL() != tc.suggested {
				t.Fatalf("suggestion is not accepted: %+v, %v", accepted, err)
			}
		})
	}
	for _, tc := range []struct{ name, base, path, reason string }{
		{"no scheme", "api.example.test", "/v1/chat/completions", EndpointBaseURLInvalid},
		{"query", "https://api.example.test?key=1", "/v1/chat/completions", EndpointBaseURLInvalid},
		{"credentials", "https://user:secret@api.example.test", "/v1/chat/completions", EndpointBaseURLInvalid},
		{"empty path", "https://api.example.test", "", EndpointChatPathInvalid},
		{"relative path", "https://api.example.test", "v1/chat/completions", EndpointChatPathInvalid},
		{"path with query", "https://api.example.test", "/v1/chat/completions?x=1", EndpointChatPathInvalid},
		{"repeated segment in path", "https://api.example.test", "/v1/v1/chat/completions", EndpointDuplicatePathSegment},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NormalizeProviderEndpoint("openai", tc.base, tc.path)
			var refused *EndpointError
			if !errors.As(err, &refused) || refused.Reason != tc.reason {
				t.Fatalf("err = %v, want %s", err, tc.reason)
			}
		})
	}
	if err := ValidateProviderEndpoint("openai", "https://api.example.test/", "/v1/chat/completions"); err == nil {
		t.Fatal("a stored endpoint not in stored form was accepted")
	}
}

func TestFrozenModelBindingHashCoversTheChatPathOnlyWhenRecorded(t *testing.T) {
	binding := FrozenModelBinding{
		SchemaVersion: FrozenSchemaVersion, WorkspaceID: "ws", ProviderID: "p", ProviderRevision: 1, ModelID: "m",
		BaseURL:       "https://api.example.test",
		CredentialRef: CredentialReference{SchemaVersion: FrozenSchemaVersion, WorkspaceID: "ws", Scope: CredentialScopeWorkspaceService, ServiceID: "svc", Kind: CredentialProviderAPIKey, ResourceID: "p", Slot: "api_key"},
	}
	legacy, err := HashDTO(binding, PreorderFrozenModelBinding)
	if err != nil {
		t.Fatal(err)
	}
	// The hash of a binding without a chat path is the one bindings had before
	// chat paths existed: the hash input encodes no chat_path key.
	before := struct {
		SchemaVersion    int                 `json:"schema_version"`
		WorkspaceID      string              `json:"workspace_id"`
		ProviderID       string              `json:"provider_id"`
		ProviderRevision int64               `json:"provider_revision"`
		ModelID          string              `json:"model_id"`
		BaseURL          string              `json:"base_url"`
		JSONObjectMode   bool                `json:"json_object_mode"`
		CredentialRef    CredentialReference `json:"credential_ref"`
	}{binding.SchemaVersion, binding.WorkspaceID, binding.ProviderID, binding.ProviderRevision, binding.ModelID, binding.BaseURL, binding.JSONObjectMode, binding.CredentialRef}
	raw, err := canonicalizeTyped(before)
	if err != nil {
		t.Fatal(err)
	}
	want, err := HashCanonicalJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if legacy != want {
		t.Fatalf("legacy binding hash changed: %s != %s", legacy, want)
	}
	binding.ChatPath = "/v1/chat/completions"
	first, err := HashDTO(binding, PreorderFrozenModelBinding)
	if err != nil {
		t.Fatal(err)
	}
	binding.ChatPath = "/api/v4/chat/completions"
	second, err := HashDTO(binding, PreorderFrozenModelBinding)
	if err != nil {
		t.Fatal(err)
	}
	if first == legacy || first == second {
		t.Fatal("a recorded chat path is not part of the binding hash")
	}
}
