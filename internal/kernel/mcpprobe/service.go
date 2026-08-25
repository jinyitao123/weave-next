// Package mcpprobe performs strict MCP Streamable HTTP discovery and persists
// the most recent successful tool catalog.
package mcpprobe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jinyitao123/weave/internal/kernel/mcphost"
	"github.com/jinyitao123/weave/internal/kernel/mcpregistry"
)

const (
	defaultProbeTimeout = 5 * time.Second
	maxLastErrorRunes   = 1024
)

var (
	ErrProbeFailed    = errors.New("MCP server probe failed")
	ErrServerDisabled = mcpregistry.ErrClosed
)

type registryStore interface {
	Resolve(context.Context, string, string) (mcpregistry.ResolvedServer, error)
	RecordProbeSuccess(
		context.Context,
		string,
		string,
		string,
		json.RawMessage,
		[]mcpregistry.Tool,
	) (mcpregistry.ProbeResult, error)
	RecordProbeFailure(context.Context, string, string, string) (mcpregistry.ServerView, error)
}

// Service performs a strict upstream handshake before updating registry state.
type Service struct {
	store   registryStore
	timeout time.Duration
}

// Option configures a probe service.
type Option func(*Service)

// New creates a strict MCP probe service.
func New(store registryStore, opts ...Option) *Service {
	service := &Service{store: store, timeout: defaultProbeTimeout}
	for _, opt := range opts {
		opt(service)
	}
	return service
}

// Probe resolves the workspace-owned server, performs a strict MCP handshake,
// and persists either a complete successful catalog or a sanitized failure.
func (s *Service) Probe(ctx context.Context, workspaceID, serverID string) (mcpregistry.ProbeResult, error) {
	server, err := s.store.Resolve(ctx, workspaceID, serverID)
	if err != nil {
		return mcpregistry.ProbeResult{}, err
	}
	if !server.Enabled {
		return mcpregistry.ProbeResult{}, ErrServerDisabled
	}
	if server.Transport != mcpregistry.TransportStreamableHTTP {
		return mcpregistry.ProbeResult{}, fmt.Errorf("%w: %s", mcpregistry.ErrUnsupportedTransport, server.Transport)
	}

	probeCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	host := mcphost.NewHTTPHost(
		server.URL,
		mcphost.WithHeaders(server.Headers),
		mcphost.WithTimeout(s.timeout),
	)
	metadata, wireTools, err := host.StrictProbe(probeCtx)
	if err != nil {
		lastError := sanitizeProbeError(err, server)
		if _, recordErr := s.store.RecordProbeFailure(ctx, workspaceID, serverID, lastError); recordErr != nil {
			return mcpregistry.ProbeResult{}, fmt.Errorf("record MCP probe failure: %w", recordErr)
		}
		return mcpregistry.ProbeResult{}, fmt.Errorf("%w: %s", ErrProbeFailed, lastError)
	}

	tools := make([]mcpregistry.Tool, 0, len(wireTools))
	for _, wire := range wireTools {
		var readOnlyHint *bool
		if wire.ReadOnlyHint != nil {
			value := *wire.ReadOnlyHint
			readOnlyHint = &value
		}
		tools = append(tools, mcpregistry.Tool{
			ServerID:     serverID,
			Name:         wire.Name,
			Description:  wire.Description,
			InputSchema:  append(json.RawMessage(nil), wire.InputSchema...),
			Annotations:  append(json.RawMessage(nil), wire.Annotations...),
			ReadOnlyHint: readOnlyHint,
		})
	}
	return s.store.RecordProbeSuccess(
		ctx,
		workspaceID,
		serverID,
		metadata.ProtocolVersion,
		metadata.ServerInfo,
		tools,
	)
}

func sanitizeProbeError(err error, server mcpregistry.ResolvedServer) string {
	message := err.Error()
	values := make([]string, 0, len(server.Headers)+len(server.Env))
	for _, value := range server.Headers {
		if value != "" {
			values = append(values, value)
		}
	}
	for _, value := range server.Env {
		if value != "" {
			values = append(values, value)
		}
	}
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	for _, value := range values {
		message = strings.ReplaceAll(message, value, "[redacted]")
		for _, token := range strings.Fields(value) {
			if len(token) < 6 {
				continue
			}
			pattern := regexp.MustCompile(`(?i:` + regexp.QuoteMeta(token) + `)`)
			message = pattern.ReplaceAllLiteralString(message, "[redacted]")
		}
	}
	if parsed, parseErr := url.Parse(server.URL); parseErr == nil && parsed.User != nil {
		message = strings.ReplaceAll(message, parsed.User.String()+"@", "[redacted]@")
	}
	runes := []rune(message)
	if len(runes) > maxLastErrorRunes {
		message = string(runes[:maxLastErrorRunes])
	}
	return message
}
