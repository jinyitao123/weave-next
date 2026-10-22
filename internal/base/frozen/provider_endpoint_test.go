package frozen

import (
	"errors"
	"testing"
)

func TestProviderEndpointKeepsTheAddressItWasGiven(t *testing.T) {
	for _, tc := range []struct {
		name, protocol, base, path, url string
	}{
		{"origin and default path", ProtocolOpenAICompletions, "https://api.example.test", "", "https://api.example.test/v1/chat/completions"},
		{"trailing slash", ProtocolOpenAICompletions, "https://api.example.test/", "", "https://api.example.test/v1/chat/completions"},
		{"sdk style base with its own path", ProtocolOpenAICompletions, "https://api.example.test/v1", "/chat/completions", "https://api.example.test/v1/chat/completions"},
		{"gateway prefix is kept", ProtocolOpenAICompletions, "https://gw.example.test/openai/team-a", "", "https://gw.example.test/openai/team-a/v1/chat/completions"},
		{"custom path", ProtocolOpenAICompletions, "https://open.example.test", "/api/paas/v4/chat/completions", "https://open.example.test/api/paas/v4/chat/completions"},
		{"responses default", ProtocolOpenAIResponses, "https://api.example.test", "", "https://api.example.test/v1/responses"},
		{"messages default", ProtocolAnthropicMessages, "https://api.example.test", "", "https://api.example.test/v1/messages"},
		{"messages behind a prefix", ProtocolAnthropicMessages, "https://gw.example.test/anthropic", "", "https://gw.example.test/anthropic/v1/messages"},
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

func TestProviderEndpointRefusesAmbiguousAddressesWithASuggestion(t *testing.T) {
	for _, tc := range []struct {
		name, protocol, base, path, reason, suggested string
	}{
		{"sdk style base with the default path", ProtocolOpenAICompletions, "https://api.example.test/v1", "", EndpointDuplicatePathSegment, "https://api.example.test/v1/chat/completions"},
		{"full endpoint as base", ProtocolOpenAICompletions, "https://api.example.test/v1/chat/completions", "", EndpointInBaseURL, "https://api.example.test/v1/chat/completions"},
		{"full gateway endpoint as base", ProtocolOpenAICompletions, "https://gw.example.test/p/v1/chat/completions/", "", EndpointInBaseURL, "https://gw.example.test/p/v1/chat/completions"},
		{"messages base ending in v1", ProtocolAnthropicMessages, "https://api.example.test/v1", "", EndpointDuplicatePathSegment, "https://api.example.test/v1/messages"},
		{"responses endpoint as base", ProtocolOpenAIResponses, "https://api.example.test/v1/responses", "", EndpointInBaseURL, "https://api.example.test/v1/responses"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NormalizeProviderEndpoint(tc.protocol, tc.base, tc.path)
			var refused *EndpointError
			if !errors.As(err, &refused) || refused.Reason != tc.reason {
				t.Fatalf("err = %v, want %s", err, tc.reason)
			}
			if refused.Suggestion == nil || refused.Suggestion.URL() != tc.suggested {
				t.Fatalf("suggestion = %+v, want %s", refused.Suggestion, tc.suggested)
			}
			// The suggestion is itself an accepted endpoint for the same address.
			if accepted, err := NormalizeProviderEndpoint(tc.protocol, refused.Suggestion.BaseURL, refused.Suggestion.ChatPath); err != nil || accepted.URL() != tc.suggested {
				t.Fatalf("suggestion is not accepted: %+v, %v", accepted, err)
			}
		})
	}
	for _, tc := range []struct {
		name, protocol, base, path, reason string
	}{
		{"no scheme", ProtocolOpenAICompletions, "api.example.test", "", EndpointBaseURLInvalid},
		{"query", ProtocolOpenAICompletions, "https://api.example.test?key=1", "", EndpointBaseURLInvalid},
		{"credentials", ProtocolOpenAICompletions, "https://user:secret@api.example.test", "", EndpointBaseURLInvalid},
		{"relative path", ProtocolOpenAICompletions, "https://api.example.test", "v1/chat/completions", EndpointChatPathInvalid},
		{"path with query", ProtocolOpenAICompletions, "https://api.example.test", "/v1/chat/completions?x=1", EndpointChatPathInvalid},
		{"repeated segment in path", ProtocolOpenAICompletions, "https://api.example.test", "/v1/v1/chat/completions", EndpointDuplicatePathSegment},
		{"responses path of another protocol", ProtocolOpenAIResponses, "https://api.example.test", "/v1/chat/completions", EndpointProtocolPathMismatch},
		{"messages path without v1", ProtocolAnthropicMessages, "https://api.example.test", "/messages", EndpointProtocolPathMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NormalizeProviderEndpoint(tc.protocol, tc.base, tc.path)
			var refused *EndpointError
			if !errors.As(err, &refused) || refused.Reason != tc.reason {
				t.Fatalf("err = %v, want %s", err, tc.reason)
			}
		})
	}
	if err := ValidateProviderEndpoint(ProtocolOpenAICompletions, "https://api.example.test", ""); err == nil {
		t.Fatal("a stored endpoint without a chat path was accepted")
	}
	if err := ValidateProviderEndpoint(ProtocolOpenAICompletions, "https://api.example.test/", "/v1/chat/completions"); err == nil {
		t.Fatal("a stored endpoint not in stored form was accepted")
	}
}

func TestFrozenModelBindingHashCoversTheChatPath(t *testing.T) {
	binding := FrozenModelBinding{
		SchemaVersion: FrozenSchemaVersion, WorkspaceID: "ws", ProviderID: "p", ProviderRevision: 1, ModelID: "m",
		BaseURL: "https://api.example.test", ChatPath: "/v1/chat/completions", Protocol: ProtocolOpenAICompletions,
		CredentialRef: CredentialReference{SchemaVersion: FrozenSchemaVersion, WorkspaceID: "ws", Scope: CredentialScopeWorkspaceService, ServiceID: "svc", Kind: CredentialProviderAPIKey, ResourceID: "p", Slot: "api_key"},
	}
	first, err := HashDTO(binding, PreorderFrozenModelBinding)
	if err != nil {
		t.Fatal(err)
	}
	binding.ChatPath = "/api/v4/chat/completions"
	second, err := HashDTO(binding, PreorderFrozenModelBinding)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("changing the chat path kept the binding hash")
	}
	binding.ChatPath = ""
	if _, err := HashDTO(binding, PreorderFrozenModelBinding); err == nil {
		t.Fatal("a binding without a chat path was frozen")
	}
}
