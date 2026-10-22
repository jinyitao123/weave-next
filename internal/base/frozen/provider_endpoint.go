package frozen

import (
	"fmt"
	"net/url"
	"strings"
)

// A provider endpoint is a base URL plus a chat path, and the request goes to
// exactly their concatenation. The base URL is the service origin or a gateway
// prefix and never holds the endpoint itself; the chat path is stored
// explicitly, so a frozen binding keeps the exact address it was published
// with. Nothing is guessed from model names and no custom prefix is cut away.

// ProviderEndpoint is a validated base URL and chat path.
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
	EndpointProtocolPathMismatch = "chat_path_protocol_mismatch"
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

// DefaultChatPath is the path a protocol's own service uses. It fills an
// omitted chat path when a configuration is saved and is then frozen with it.
func DefaultChatPath(protocol string) string {
	switch protocol {
	case ProtocolOpenAICompletions:
		return "/v1/chat/completions"
	case ProtocolOpenAIResponses:
		return "/v1/responses"
	case ProtocolAnthropicMessages:
		return "/v1/messages"
	}
	return ""
}

// protocolPathSuffix is the part of the path the protocol's adapter sends to
// on its own; a native adapter's path must end with it.
func protocolPathSuffix(protocol string) string {
	switch protocol {
	case ProtocolOpenAIResponses:
		return "/responses"
	case ProtocolAnthropicMessages:
		return "/v1/messages"
	}
	return ""
}

// endpointTail is what a base URL ends with when it was given the endpoint.
func endpointTail(protocol string) string {
	switch protocol {
	case ProtocolOpenAICompletions:
		return "/chat/completions"
	case ProtocolOpenAIResponses:
		return "/responses"
	case ProtocolAnthropicMessages:
		return "/messages"
	}
	return ""
}

// NormalizeProviderEndpoint validates a base URL and chat path for protocol and
// returns them in stored form: trailing slashes removed and an empty chat
// path replaced by the protocol default.
func NormalizeProviderEndpoint(protocol, baseURL, chatPath string) (ProviderEndpoint, error) {
	if DefaultChatPath(protocol) == "" {
		return ProviderEndpoint{}, fmt.Errorf("model provider protocol is required and must be supported")
	}
	base, origin, basePath, err := parseEndpointBase(baseURL)
	if err != nil {
		return ProviderEndpoint{}, err
	}
	path := strings.TrimSpace(chatPath)
	if path == "" {
		path = DefaultChatPath(protocol)
	}
	if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "?#") || strings.HasSuffix(path, "/") || strings.Contains(path, "//") || hasDotSegment(path) {
		return ProviderEndpoint{}, &EndpointError{Reason: EndpointChatPathInvalid, Detail: "chat path must start with / and hold only path segments"}
	}
	if tail := endpointTail(protocol); basePath != "" && (basePath == tail || strings.HasSuffix(basePath, tail)) {
		return ProviderEndpoint{}, &EndpointError{Reason: EndpointInBaseURL, Detail: "base URL already ends with the endpoint path",
			Suggestion: &ProviderEndpoint{BaseURL: origin, ChatPath: basePath}}
	}
	if suffix := protocolPathSuffix(protocol); suffix != "" && !strings.HasSuffix(path, suffix) {
		return ProviderEndpoint{}, &EndpointError{Reason: EndpointProtocolPathMismatch, Detail: "chat path must end with " + suffix + " for this protocol"}
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

// ValidateProviderEndpoint accepts only an endpoint already in stored form.
func ValidateProviderEndpoint(protocol, baseURL, chatPath string) error {
	if chatPath == "" {
		return &EndpointError{Reason: EndpointChatPathInvalid, Detail: "chat path is required"}
	}
	normalized, err := NormalizeProviderEndpoint(protocol, baseURL, chatPath)
	if err != nil {
		return err
	}
	if normalized.BaseURL != baseURL || normalized.ChatPath != chatPath {
		return &EndpointError{Reason: EndpointBaseURLInvalid, Detail: "endpoint is not in stored form"}
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
