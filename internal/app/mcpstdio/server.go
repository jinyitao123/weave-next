// Package mcpstdio exposes Weave's shared MCP adapter over line-delimited stdio.
package mcpstdio

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/jinyitao123/weave/internal/app/mcpprotocol"
	"github.com/jinyitao123/weave/internal/app/weaveclient"
)

const maxMessageBytes = 16 << 20

const serverInstructions = "For each request, decide whether it is one work item or a project. Decompose projects in Codex, then call team_list per work item. Dispatch only a clear match with an available default workflow. For partial or no match, explain the gap; never silently substitute a team or free_collab. Discuss a new team here and call team_create only after explicit confirmation. If yielded, read the human task and ask the user. If completed, match the run in deliverable_list, call deliverable_get, and return it."

func Serve(ctx context.Context, input io.Reader, output io.Writer, client *weaveclient.Client) error {
	if client == nil {
		return fmt.Errorf("MCP client is unavailable")
	}
	adapter := mcpprotocol.Adapter{
		Dispatcher: NewToolDispatcher(client), ServerName: "weave",
		Instructions:             serverInstructions,
		UnsupportedMethodMessage: "method not supported",
	}
	encoder := json.NewEncoder(output)
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64*1024), maxMessageBytes)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := scanner.Bytes()
		if strings.TrimSpace(string(line)) == "" {
			continue
		}
		request, err := mcpprotocol.Decode(bytes.NewReader(line))
		if err != nil {
			if err := encoder.Encode(mcpprotocol.ErrorResponse(nil, err)); err != nil {
				return fmt.Errorf("write MCP response: %w", err)
			}
			continue
		}
		result, err := adapter.Handle(ctx, request)
		if err != nil {
			if err := encoder.Encode(mcpprotocol.ErrorResponse(request.ID, err)); err != nil {
				return fmt.Errorf("write MCP response: %w", err)
			}
			continue
		}
		if result.Notification {
			continue
		}
		if err := encoder.Encode(result.Response); err != nil {
			return fmt.Errorf("write MCP response: %w", err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read MCP request: %w", err)
	}
	return nil
}
