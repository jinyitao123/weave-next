// Package mcpregistry persists workspace-scoped MCP server connection metadata.
package mcpregistry

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type Transport string

const (
	TransportStreamableHTTP Transport = "streamable_http"
	TransportStdio          Transport = "stdio"
	MaskedHeader                      = "••••••••"
)

var (
	ErrUnsupportedTransport = &Error{code: CodeUnsupportedTransport}
	ErrInvalidRequest       = &Error{code: CodeInvalidRequest}

	serverSlugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
)

const (
	CodeUnsupportedTransport = "workflow_mcp_unsupported_transport"
	CodeInvalidRequest       = "workflow_mcp_invalid_request"
	CodeNotFound             = "workflow_mcp_server_not_found"
	CodeConflict             = "workflow_mcp_server_conflict"
	CodeClosed               = "workflow_mcp_server_closed"
	CodeKeyUnavailable       = "workflow_mcp_credential_unavailable"
)

type Error struct {
	code   string
	detail string
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.detail == "" {
		return e.code
	}
	return e.code + ": " + e.detail
}

func (e *Error) Code() string {
	if e == nil {
		return ""
	}
	return e.code
}

func (e *Error) Is(target error) bool {
	other, ok := target.(*Error)
	return ok && e != nil && other != nil && e.code == other.code
}

func coded(base *Error, detail string) error {
	return &Error{code: base.code, detail: detail}
}

type Server struct {
	ID                 string          `json:"id"`
	WorkspaceID        string          `json:"workspace_id"`
	Slug               string          `json:"slug"`
	DisplayName        string          `json:"display_name"`
	Transport          Transport       `json:"transport"`
	URL                string          `json:"url,omitempty"`
	Command            string          `json:"command,omitempty"`
	Args               []string        `json:"args,omitempty"`
	FunctionalRevision int64           `json:"functional_revision"`
	Enabled            bool            `json:"enabled"`
	RevokedAt          *time.Time      `json:"revoked_at,omitempty"`
	Status             string          `json:"status"`
	ProtocolVersion    string          `json:"protocol_version,omitempty"`
	LastError          string          `json:"last_error,omitempty"`
	ServerInfo         json.RawMessage `json:"server_info,omitempty"`
	LastProbedAt       *time.Time      `json:"last_probed_at,omitempty"`
	LastHandshakeAt    *time.Time      `json:"last_handshake_at,omitempty"`
	CreatedBy          string          `json:"created_by"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
	DeletedAt          *time.Time      `json:"deleted_at,omitempty"`
}

// ResolvedServer is internal-only connection material and must never be used
// as an HTTP response DTO.
type ResolvedServer struct {
	Server
	Headers map[string]string
	Env     map[string]string
}

type ServerView struct {
	Server
	Headers    map[string]string `json:"headers"`
	Env        map[string]string `json:"env"`
	ToolCount  int               `json:"tool_count"`
	AgentCount int               `json:"agent_count"`
}

// ServerMetadata is the non-secret registry projection used by discovery and
// planning paths. Reading it must not depend on decrypting connection
// credentials: a stale or rotated credential can make one server unusable,
// but must not hide unrelated providers, runtimes, or MCP availability facts.
type ServerMetadata struct {
	Server
	ToolCount  int `json:"tool_count"`
	AgentCount int `json:"agent_count"`
}

type UpsertServerRequest struct {
	Slug           string            `json:"slug"`
	DisplayName    string            `json:"display_name"`
	Transport      Transport         `json:"transport"`
	URL            string            `json:"url"`
	Command        string            `json:"command"`
	Args           []string          `json:"args,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	DeletedHeaders []string          `json:"deleted_headers,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	DeletedEnv     []string          `json:"deleted_env,omitempty"`
	Enabled        bool              `json:"enabled"`
}

type Tool struct {
	ServerID     string          `json:"server_id"`
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"input_schema"`
	Annotations  json.RawMessage `json:"annotations,omitempty"`
	ReadOnlyHint *bool           `json:"read_only_hint,omitempty"`
	DiscoveredAt time.Time       `json:"discovered_at"`
}

type ProbeResult struct {
	Server ServerView `json:"server"`
	Tools  []Tool     `json:"tools"`
}

func ValidateUpsertServerRequest(req UpsertServerRequest) error {
	if !serverSlugPattern.MatchString(req.Slug) {
		return coded(ErrInvalidRequest, "slug must be URL-safe")
	}
	if strings.TrimSpace(req.DisplayName) == "" {
		return coded(ErrInvalidRequest, "display_name is required")
	}
	switch req.Transport {
	case TransportStreamableHTTP:
		if req.Command != "" || len(req.Args) != 0 || len(req.Env) != 0 {
			return coded(ErrInvalidRequest, "HTTP transport cannot carry command, args, or environment")
		}
		parsed, err := url.Parse(req.URL)
		if err != nil || !parsed.IsAbs() || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
			return coded(ErrInvalidRequest, "url must be an absolute HTTP(S) URL with a host")
		}
		if parsed.User != nil {
			return coded(ErrInvalidRequest, "url userinfo is not allowed")
		}
		if parsed.RawQuery != "" || parsed.ForceQuery {
			return coded(ErrInvalidRequest, "url query is not allowed")
		}
		if parsed.Fragment != "" || parsed.RawFragment != "" {
			return coded(ErrInvalidRequest, "url fragment is not allowed")
		}
	case TransportStdio:
		if req.URL != "" || len(req.Headers) != 0 {
			return coded(ErrInvalidRequest, "stdio transport cannot carry a URL or headers")
		}
		if strings.TrimSpace(req.Command) == "" {
			return coded(ErrInvalidRequest, "stdio command is required")
		}
	default:
		return coded(ErrUnsupportedTransport, fmt.Sprintf("unsupported transport %q", req.Transport))
	}
	for key := range req.Headers {
		if strings.TrimSpace(key) == "" {
			return coded(ErrInvalidRequest, "header names must not be empty")
		}
	}
	for _, key := range req.DeletedHeaders {
		if strings.TrimSpace(key) == "" {
			return coded(ErrInvalidRequest, "deleted header names must not be empty")
		}
	}
	for key := range req.Env {
		if strings.TrimSpace(key) == "" {
			return coded(ErrInvalidRequest, "environment names must not be empty")
		}
	}
	for _, key := range req.DeletedEnv {
		if strings.TrimSpace(key) == "" {
			return coded(ErrInvalidRequest, "deleted environment names must not be empty")
		}
	}
	return nil
}
