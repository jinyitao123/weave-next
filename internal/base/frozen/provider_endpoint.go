package frozen

import (
	"fmt"
	"net/url"
	"strings"
)

// A provider endpoint is a base URL plus a chat path, and a request goes to
// exactly their concatenation. The chat path is stored explicitly, so a frozen
// binding keeps the address it was published with; nothing is guessed from
// model names and no custom prefix is cut away.
//
// An endpoint recorded before chat paths existed has an empty path. Its
// address keeps the rule its client always applied: a trailing /v1 is dropped
// from the base and the protocol's default path is appended.

// ProviderEndpoint is a base URL and chat path in stored form.
type ProviderEndpoint struct {
	BaseURL  string `json:"base_url"`
	ChatPath string `json:"chat_path"`
}

// URL is the address every request of this endpoint is sent to.
func (e ProviderEndpoint) URL() string { return e.BaseURL + e.ChatPath }

// Reasons an endpoint is refused.
const (
	EndpointBaseURLInvalid       = "base_url_invalid"
	EndpointChatPathInvalid      = "chat_path_invalid"
	EndpointInBaseURL            = "endpoint_in_base_url"
	EndpointDuplicatePathSegment = "duplicate_path_segment"
)

// EndpointError explains a refused endpoint. Suggestion, when present, is the
// split that sends requests to the address the caller most likely meant; it is
// offered for confirmation, never applied silently.
type EndpointError struct {
	Reason     string
	Detail     string
	Suggestion *ProviderEndpoint
}

func (e *EndpointError) Error() string { return e.Reason + ": " + e.Detail }

// DefaultChatPath is the path a protocol's own service uses. Protocol is the
// frozen value: empty or "openai" for chat completions, ModelProtocolSystemOne
// for the System One decision protocol.
func DefaultChatPath(protocol string) string {
	if protocol == ModelProtocolSystemOne {
		return "/v1/systemone"
	}
	return "/v1/chat/completions"
}

// endpointTail is what a base URL ends with when it was given the endpoint.
func endpointTail(protocol string) string {
	if protocol == ModelProtocolSystemOne {
		return "/systemone"
	}
	return "/chat/completions"
}

// LegacyProviderEndpoint is the address an endpoint without a recorded chat
// path has always reached.
func LegacyProviderEndpoint(protocol, baseURL string) ProviderEndpoint {
	return ProviderEndpoint{BaseURL: strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1"), ChatPath: DefaultChatPath(protocol)}
}

// ResolveProviderEndpoint is the address requests go to: the recorded split,
// or the legacy rule when no chat path was recorded.
func ResolveProviderEndpoint(protocol, baseURL, chatPath string) ProviderEndpoint {
	if chatPath == "" {
		return LegacyProviderEndpoint(protocol, baseURL)
	}
	return ProviderEndpoint{BaseURL: baseURL, ChatPath: chatPath}
}

// NormalizeProviderEndpoint validates an explicitly given base URL and chat
// path and returns them in stored form, trailing slashes removed. An empty
// chat path is refused: callers that have none keep the legacy address.
func NormalizeProviderEndpoint(protocol, baseURL, chatPath string) (ProviderEndpoint, error) {
	base, origin, basePath, err := parseEndpointBase(baseURL)
	if err != nil {
		return ProviderEndpoint{}, err
	}
	path := strings.TrimSpace(chatPath)
	if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "?#") || strings.HasSuffix(path, "/") || strings.Contains(path, "//") || hasDotSegment(path) {
		return ProviderEndpoint{}, &EndpointError{Reason: EndpointChatPathInvalid, Detail: "chat path must start with / and hold only path segments"}
	}
	if tail := endpointTail(protocol); basePath != "" && strings.HasSuffix(basePath, tail) {
		return ProviderEndpoint{}, &EndpointError{Reason: EndpointInBaseURL, Detail: "base URL already ends with the endpoint path",
			Suggestion: &ProviderEndpoint{BaseURL: origin, ChatPath: basePath}}
	}
	baseSegments, pathSegments := pathSegments(basePath), pathSegments(path)
	if len(baseSegments) > 0 && baseSegments[len(baseSegments)-1] == pathSegments[0] {
		trimmed := strings.Join(baseSegments[:len(baseSegments)-1], "/")
		if trimmed != "" {
			trimmed = "/" + trimmed
		}
		return ProviderEndpoint{}, &EndpointError{Reason: EndpointDuplicatePathSegment, Detail: "base URL ends with the chat path's first segment " + pathSegments[0],
			Suggestion: &ProviderEndpoint{BaseURL: origin + trimmed, ChatPath: path}}
	}
	for index := 1; index < len(pathSegments); index++ {
		if pathSegments[index] == pathSegments[index-1] {
			return ProviderEndpoint{}, &EndpointError{Reason: EndpointDuplicatePathSegment, Detail: "chat path repeats the segment " + pathSegments[index]}
		}
	}
	return ProviderEndpoint{BaseURL: base, ChatPath: path}, nil
}

// ValidateProviderEndpoint accepts a recorded chat path only in stored form.
// An empty chat path is the legacy form and is always accepted.
func ValidateProviderEndpoint(protocol, baseURL, chatPath string) error {
	if chatPath == "" {
		return nil
	}
	normalized, err := NormalizeProviderEndpoint(protocol, baseURL, chatPath)
	if err != nil {
		return err
	}
	if normalized.BaseURL != baseURL || normalized.ChatPath != chatPath {
		return &EndpointError{Reason: EndpointBaseURLInvalid, Detail: fmt.Sprintf("endpoint %q is not in stored form", baseURL+chatPath)}
	}
	return nil
}

func parseEndpointBase(raw string) (base, origin, path string, err error) {
	refuse := func(detail string) (string, string, string, error) {
		return "", "", "", &EndpointError{Reason: EndpointBaseURLInvalid, Detail: detail}
	}
	parsed, parseErr := url.Parse(strings.TrimSpace(raw))
	if parseErr != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return refuse("base URL must be an absolute http or https URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Opaque != "" {
		return refuse("base URL must not carry credentials, a query or a fragment")
	}
	path = strings.TrimRight(parsed.EscapedPath(), "/")
	if strings.Contains(path, "//") || hasDotSegment(path) {
		return refuse("base URL path holds empty or dot segments")
	}
	origin = parsed.Scheme + "://" + parsed.Host
	return origin + path, origin, path, nil
}

func pathSegments(path string) []string {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "/")
}

func hasDotSegment(path string) bool {
	for _, segment := range pathSegments(path) {
		if segment == "." || segment == ".." {
			return true
		}
	}
	return false
}
