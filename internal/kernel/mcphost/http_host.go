package mcphost

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jinyitao123/loom/contract"
)

const (
	protocolVersion = "2025-03-26"
	clientName      = "weave"
	clientVersion   = "1.0.0"
)

// HTTPHost implements contract.ToolDispatcher by calling an MCP server over HTTP.
type HTTPHost struct {
	baseURL                string
	httpClient             *http.Client
	filter                 map[string]bool // if non-empty, only expose these tool names
	headers                map[string]string
	dispatchGuard          func(context.Context) error
	unknownDispatchOutcome bool
	toolContract           *ToolContract
	contractMu             sync.Mutex
	liveContract           *ToolContract
	liveContractDigest     [32]byte

	initOnce    sync.Once
	initialized bool
	sessionID   string
	protocol    string
	nextID      atomic.Int64
}

// HostOption configures an HTTPHost.
type HostOption func(*HTTPHost)

// WithToolContract validates the final outgoing arguments against an immutable
// caller-owned catalog. A live tool listing cannot replace this contract.
func WithToolContract(bound *ToolContract) HostOption {
	return func(h *HTTPHost) { h.toolContract = bound }
}

// WithDispatchGuard rechecks dynamic authority at the final outgoing effect
// boundary, after any initialization or remote catalog requests.
func WithDispatchGuard(guard func(context.Context) error) HostOption {
	return func(h *HTTPHost) { h.dispatchGuard = guard }
}

// WithUnknownDispatchOutcome returns a typed error when a request may have
// reached the MCP server but no usable protocol response was received. It is
// intended for callers that need to distinguish uncertain external writes.
func WithUnknownDispatchOutcome() HostOption {
	return func(h *HTTPHost) { h.unknownDispatchOutcome = true }
}

// WithTimeout sets the HTTP request timeout.
func WithTimeout(d time.Duration) HostOption {
	return func(h *HTTPHost) {
		h.httpClient.Timeout = d
	}
}

// WithFilter restricts which tools are exposed from this MCP server.
func WithFilter(names []string) HostOption {
	return func(h *HTTPHost) {
		h.filter = make(map[string]bool, len(names))
		for _, n := range names {
			h.filter[n] = true
		}
	}
}

// WithHeaders adds per-server headers to every request. MCP protocol headers
// are set by HTTPHost after these values and cannot be overridden.
func WithHeaders(headers map[string]string) HostOption {
	return func(h *HTTPHost) {
		h.headers = make(map[string]string, len(headers))
		for name, value := range headers {
			h.headers[name] = value
		}
	}
}

// WithHTTPClient replaces the HTTP client used for every request. A nil client
// is ignored so the default client stays in place. The runtime daemon uses this
// to dial the task-scoped MCP gateway through the same client (transport, TLS,
// proxy) it already uses for the rest of the server API.
func WithHTTPClient(client *http.Client) HostOption {
	return func(h *HTTPHost) {
		if client != nil {
			h.httpClient = client
		}
	}
}

// NewHTTPHost creates a new MCP HTTP client.
func NewHTTPHost(baseURL string, opts ...HostOption) *HTTPHost {
	h := &HTTPHost{
		baseURL:    baseURL,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// jsonRPCRequest is a JSON-RPC 2.0 request.
type jsonRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
	ID      int64  `json:"id,omitempty"`
}

// jsonRPCResponse is a JSON-RPC 2.0 response.
type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
	ID      int64           `json:"id"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ProbeMetadata is the connection metadata returned by a successful strict
// Streamable HTTP initialization handshake.
type ProbeMetadata struct {
	ProtocolVersion string
	ServerInfo      json.RawMessage
}

// MCPTool is the MCP wire representation needed by both runtime dispatch and
// the persistent registry catalog.
type MCPTool struct {
	Name         string
	Description  string
	InputSchema  json.RawMessage
	Annotations  json.RawMessage
	ReadOnlyHint *bool
}

func (h *HTTPHost) newRequest(ctx context.Context, payload any, negotiated bool) (*http.Request, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, h.baseURL, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	for name, value := range h.headers {
		httpReq.Header.Set(name, value)
	}

	// Set protocol-owned headers last so per-server configuration cannot
	// override the MCP transport contract.
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	if negotiated {
		negotiatedProtocol := h.protocol
		if negotiatedProtocol == "" {
			negotiatedProtocol = protocolVersion
		}
		httpReq.Header.Set("MCP-Protocol-Version", negotiatedProtocol)
		if h.sessionID != "" {
			httpReq.Header.Set("Mcp-Session-Id", h.sessionID)
		} else {
			httpReq.Header.Del("Mcp-Session-Id")
		}
	} else {
		httpReq.Header.Del("MCP-Protocol-Version")
		httpReq.Header.Del("Mcp-Session-Id")
	}
	return httpReq, nil
}

func (h *HTTPHost) post(ctx context.Context, payload any, negotiated bool) (*http.Response, error) {
	httpReq, err := h.newRequest(ctx, payload, negotiated)
	if err != nil {
		return nil, err
	}
	resp, err := h.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func readResponse(resp *http.Response, expectedID int64) (jsonRPCResponse, error) {
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return jsonRPCResponse{}, fmt.Errorf("http %d: read response: %w", resp.StatusCode, err)
		}
		return jsonRPCResponse{}, fmt.Errorf("http %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}

	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mediaType == "text/event-stream" {
		return readSSEResponse(resp.Body, expectedID)
	}

	var rpcResp jsonRPCResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return jsonRPCResponse{}, fmt.Errorf("mcp response parse: %w", err)
	}
	return rpcResp, nil
}

func readSSEResponse(body io.Reader, expectedID int64) (jsonRPCResponse, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var dataLines []string

	parseEvent := func() (jsonRPCResponse, bool) {
		if len(dataLines) == 0 {
			return jsonRPCResponse{}, false
		}
		payload := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		var rpcResp jsonRPCResponse
		if err := json.Unmarshal([]byte(payload), &rpcResp); err != nil {
			return jsonRPCResponse{}, false
		}
		return rpcResp, rpcResp.ID == expectedID
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if rpcResp, matches := parseEvent(); matches {
				return rpcResp, nil
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := scanner.Err(); err != nil {
		return jsonRPCResponse{}, fmt.Errorf("mcp SSE response read: %w", err)
	}
	if rpcResp, matches := parseEvent(); matches {
		return rpcResp, nil
	}
	return jsonRPCResponse{}, fmt.Errorf("mcp SSE response missing request id %d", expectedID)
}

func (h *HTTPHost) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := h.nextID.Add(1)
	reqBody := jsonRPCRequest{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
		ID:      id,
	}
	resp, err := h.post(ctx, reqBody, h.initialized)
	if err != nil {
		return nil, fmt.Errorf("mcp call %s: %w", method, err)
	}
	defer resp.Body.Close()

	rpcResp, err := readResponse(resp, id)
	if err != nil {
		return nil, fmt.Errorf("mcp call %s: %w", method, err)
	}
	if rpcResp.Error != nil {
		if h.unknownDispatchOutcome && method == "tools/call" {
			return nil, fmt.Errorf("%w: code %d", ErrDispatchExplicitFailure, rpcResp.Error.Code)
		}
		return nil, fmt.Errorf("mcp error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}
	return rpcResp.Result, nil
}

func (h *HTTPHost) notify(ctx context.Context, method string, params any) error {
	resp, err := h.post(ctx, jsonRPCRequest{JSONRPC: "2.0", Method: method, Params: params}, true)
	if err != nil {
		return fmt.Errorf("mcp notify %s: %w", method, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return fmt.Errorf("mcp notify %s: http %d: read response: %w", method, resp.StatusCode, readErr)
		}
		return fmt.Errorf("mcp notify %s: http %d: %s", method, resp.StatusCode, bytes.TrimSpace(body))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func (h *HTTPHost) initialize(ctx context.Context, strict bool) (ProbeMetadata, error) {
	id := h.nextID.Add(1)
	params := map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    clientName,
			"version": clientVersion,
		},
	}
	resp, err := h.post(ctx, jsonRPCRequest{
		JSONRPC: "2.0", Method: "initialize", Params: params, ID: id,
	}, false)
	if err != nil {
		return ProbeMetadata{}, fmt.Errorf("mcp initialize: %w", err)
	}
	defer resp.Body.Close()

	rpcResp, err := readResponse(resp, id)
	if err != nil {
		return ProbeMetadata{}, fmt.Errorf("mcp initialize: %w", err)
	}
	if rpcResp.Error != nil {
		return ProbeMetadata{}, fmt.Errorf("mcp initialize error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}
	var result struct {
		ProtocolVersion string          `json:"protocolVersion"`
		ServerInfo      json.RawMessage `json:"serverInfo"`
	}
	if err := json.Unmarshal(rpcResp.Result, &result); err != nil || result.ProtocolVersion == "" {
		if err != nil {
			return ProbeMetadata{}, fmt.Errorf("mcp initialize result: %w", err)
		}
		return ProbeMetadata{}, fmt.Errorf("mcp initialize result missing protocolVersion")
	}
	if len(result.ServerInfo) == 0 || bytes.Equal(bytes.TrimSpace(result.ServerInfo), []byte("null")) {
		result.ServerInfo = json.RawMessage(`{}`)
	}

	h.sessionID = resp.Header.Get("Mcp-Session-Id")
	h.protocol = result.ProtocolVersion
	h.initialized = true
	if err := h.notify(ctx, "notifications/initialized", nil); err != nil {
		if strict {
			return ProbeMetadata{}, fmt.Errorf("mcp initialized notification: %w", err)
		}
		slog.Debug("mcp initialized notification failed", "url", h.baseURL, "error", err)
	}
	return ProbeMetadata{
		ProtocolVersion: result.ProtocolVersion,
		ServerInfo:      append(json.RawMessage(nil), result.ServerInfo...),
	}, nil
}

func (h *HTTPHost) ensureInitialized(ctx context.Context) {
	h.initOnce.Do(func() {
		if _, err := h.initialize(ctx, false); err != nil {
			slog.Debug("mcp initialize unavailable; using legacy JSON-RPC mode", "url", h.baseURL, "error", err)
		}
	})
}

// StrictProbe performs the required initialize → initialized → tools/list
// sequence. Unlike ListTools, it never falls back to legacy JSON-RPC mode.
func (h *HTTPHost) StrictProbe(ctx context.Context) (ProbeMetadata, []MCPTool, error) {
	metadata, err := h.initialize(ctx, true)
	if err != nil {
		return ProbeMetadata{}, nil, err
	}
	tools, err := h.listMCPTools(ctx)
	if err != nil {
		return ProbeMetadata{}, nil, err
	}
	return metadata, tools, nil
}

func (h *HTTPHost) listMCPTools(ctx context.Context) ([]MCPTool, error) {
	result, err := h.call(ctx, "tools/list", nil)
	if err != nil {
		return nil, err
	}
	var response struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
			Annotations json.RawMessage `json:"annotations"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(result, &response); err != nil {
		return nil, err
	}

	tools := make([]MCPTool, 0, len(response.Tools))
	for _, wire := range response.Tools {
		inputSchema := append(json.RawMessage(nil), wire.InputSchema...)
		value, err := parseToolJSON(inputSchema, maxToolSchemaBytes)
		if err != nil {
			return nil, fmt.Errorf("%w: mcp_tool_schema_invalid", ErrFailClosed)
		}
		if _, ok := value.(map[string]any); !ok {
			return nil, fmt.Errorf("%w: mcp_tool_schema_not_object", ErrFailClosed)
		}
		annotations := normalizeJSONObject(wire.Annotations)
		var annotationHints struct {
			ReadOnlyHint *bool `json:"readOnlyHint"`
		}
		if err := json.Unmarshal(annotations, &annotationHints); err != nil {
			return nil, fmt.Errorf("decode MCP tool %q annotations: %w", wire.Name, err)
		}
		tools = append(tools, MCPTool{
			Name:         wire.Name,
			Description:  wire.Description,
			InputSchema:  inputSchema,
			Annotations:  annotations,
			ReadOnlyHint: annotationHints.ReadOnlyHint,
		})
	}
	return tools, nil
}

func normalizeJSONObject(raw json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return json.RawMessage(`{}`)
	}
	return append(json.RawMessage(nil), trimmed...)
}

// ListTools calls the MCP server's tools/list method.
func (h *HTTPHost) ListTools(ctx context.Context) ([]contract.ToolDef, error) {
	h.ensureInitialized(ctx)
	wireTools, err := h.listMCPTools(ctx)
	if err != nil {
		return nil, err
	}

	tools := make([]contract.ToolDef, 0, len(wireTools))
	for _, wire := range wireTools {
		tools = append(tools, contract.ToolDef{
			Name:        wire.Name,
			Description: wire.Description,
			InputSchema: append(json.RawMessage(nil), wire.InputSchema...),
			ReadOnly:    wire.ReadOnlyHint != nil && *wire.ReadOnlyHint,
		})
	}

	// Apply filter if set.
	if len(h.filter) == 0 {
		return tools, nil
	}
	var filtered []contract.ToolDef
	for _, t := range tools {
		if h.filter[t.Name] {
			filtered = append(filtered, t)
		}
	}
	return filtered, nil
}

// Dispatch calls the MCP server's tools/call method.
func (h *HTTPHost) Dispatch(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	// Enforce filter on dispatch — block tools not in the allowlist.
	if len(h.filter) > 0 && !h.filter[call.Name] {
		return &contract.ToolResult{
			CallID:  call.ID,
			Content: fmt.Sprintf("tool %q is not available for this agent", call.Name),
			IsError: true,
		}, nil
	}
	bound, err := h.contractForDispatch(ctx)
	if err != nil {
		return nil, err
	}
	if rejected := bound.Validate(call); rejected != nil {
		return rejected, nil
	}
	h.ensureInitialized(ctx)
	if h.dispatchGuard != nil {
		if err := h.dispatchGuard(ctx); err != nil {
			// Keep the guard's cause: callers tell an expired authorization from a
			// scope error with errors.Is, and the tool has not been called.
			return nil, fmt.Errorf("%w: MCP dispatch authority expired: %w", ErrFailClosed, err)
		}
	}

	params := map[string]any{
		"name":      call.Name,
		"arguments": json.RawMessage(call.Args),
	}
	result, err := h.call(ctx, "tools/call", params)
	if err != nil {
		if h.unknownDispatchOutcome {
			if errors.Is(err, ErrDispatchExplicitFailure) {
				return nil, err
			}
			return nil, fmt.Errorf("%w: %v", ErrDispatchOutcomeUnknown, err)
		}
		return &contract.ToolResult{CallID: call.ID, Content: err.Error(), IsError: true}, nil
	}

	var resp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		if h.unknownDispatchOutcome {
			return nil, fmt.Errorf("%w: malformed tools/call response", ErrDispatchOutcomeUnknown)
		}
		return &contract.ToolResult{CallID: call.ID, Content: string(result)}, nil
	}

	var content string
	for _, c := range resp.Content {
		if c.Type == "text" {
			content += c.Text
		}
	}
	return &contract.ToolResult{
		CallID:  call.ID,
		Content: content,
		IsError: resp.IsError,
	}, nil
}

func (h *HTTPHost) contractForDispatch(ctx context.Context) (*ToolContract, error) {
	if h.toolContract != nil {
		return h.toolContract, nil
	}
	// Legacy inline access has no published catalog. It must still validate
	// the actual call against the current server definition, without claiming
	// that this constitutes publication binding.
	tools, err := h.ListTools(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: mcp_tool_catalog_unavailable", ErrFailClosed)
	}
	raw, err := json.Marshal(tools)
	if err != nil {
		return nil, fmt.Errorf("%w: mcp_tool_catalog_invalid", ErrFailClosed)
	}
	digest := sha256.Sum256(raw)
	h.contractMu.Lock()
	defer h.contractMu.Unlock()
	if h.liveContract != nil && h.liveContractDigest == digest {
		return h.liveContract, nil
	}
	bound, err := NewToolContract(tools)
	if err != nil {
		return nil, err
	}
	h.liveContract, h.liveContractDigest = bound, digest
	return bound, nil
}

// Compile-time interface check.
var _ contract.ToolDispatcher = (*HTTPHost)(nil)
