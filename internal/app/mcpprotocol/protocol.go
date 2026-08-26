// Package mcpprotocol implements Weave's shared server-side MCP method adapter.
// Transports are responsible only for framing requests and mapping adapter
// errors to their own error surface.
package mcpprotocol

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/jinyitao123/loom/contract"
)

const ProtocolVersion = "2025-03-26"

var (
	ErrInvalidRequest      = errors.New("invalid JSON-RPC request")
	ErrInvalidCallParams   = errors.New("invalid tools/call params")
	ErrUpstreamUnavailable = errors.New("upstream MCP unavailable")
	ErrEmptyToolResult     = errors.New("empty MCP tool result")
)

type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type Response struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *RPCError `json:"error,omitempty"`
}

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type Result struct {
	Response     *Response
	Notification bool
}

type Adapter struct {
	Dispatcher               contract.ToolDispatcher
	ServerName               string
	UnsupportedMethodMessage string
}

func Decode(reader io.Reader) (Request, error) {
	var request Request
	decoder := json.NewDecoder(reader)
	if err := decoder.Decode(&request); err != nil {
		return Request{}, ErrInvalidRequest
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Request{}, ErrInvalidRequest
	}
	return request, nil
}

func (a Adapter) Handle(ctx context.Context, request Request) (Result, error) {
	switch request.Method {
	case "initialize":
		return Result{Response: &Response{
			JSONRPC: "2.0", ID: responseID(request.ID),
			Result: map[string]any{
				"protocolVersion": ProtocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]string{"name": a.ServerName, "version": "1"},
			},
		}}, nil
	case "notifications/initialized":
		return Result{Notification: true}, nil
	case "tools/list":
		tools, err := a.Dispatcher.ListTools(ctx)
		if err != nil {
			return Result{}, ErrUpstreamUnavailable
		}
		resultTools := make([]Tool, 0, len(tools))
		for _, tool := range tools {
			resultTools = append(resultTools, Tool{
				Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema,
			})
		}
		return Result{Response: &Response{
			JSONRPC: "2.0", ID: responseID(request.ID), Result: map[string]any{"tools": resultTools},
		}}, nil
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(request.Params, &params); err != nil {
			return Result{}, ErrInvalidCallParams
		}
		if len(params.Arguments) == 0 {
			params.Arguments = json.RawMessage(`{}`)
		}
		result, err := a.Dispatcher.Dispatch(ctx, contract.ToolCall{
			ID: requestID(request.ID), Name: params.Name, Args: string(params.Arguments),
		})
		if err != nil {
			return Result{}, ErrUpstreamUnavailable
		}
		if result == nil {
			return Result{}, ErrEmptyToolResult
		}
		content := result.Content
		if result.IsError {
			content = "upstream MCP error"
		}
		return Result{Response: &Response{
			JSONRPC: "2.0", ID: responseID(request.ID),
			Result: map[string]any{
				"content": []map[string]string{{"type": "text", "text": content}},
				"isError": result.IsError,
			},
		}}, nil
	default:
		return Result{Response: &Response{
			JSONRPC: "2.0", ID: responseID(request.ID),
			Error: &RPCError{Code: -32601, Message: a.UnsupportedMethodMessage},
		}}, nil
	}
}

func requestID(raw json.RawMessage) string {
	var id string
	if err := json.Unmarshal(raw, &id); err == nil {
		return id
	}
	return string(raw)
}

func responseID(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return raw
}
