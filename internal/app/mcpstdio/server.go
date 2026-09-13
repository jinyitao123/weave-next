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

const serverInstructions = "Workbench owns the work conversation; call team_list for a matching team. Confirm team, task scope and deliverable before dispatch; honor prior explicit approval without asking again. Dispatch the original business task and materials, excluding interface or platform instructions. Use an available default workflow; explain gaps and never silently substitute a team or free_collab. Create a team only after approval. For reusable capabilities, call capability_list first; capability_plan creates a draft, and capability_publish requires user confirmation. Follow the same run after dispatch. If yielded or parked, inspect the waiting reason; ask the user only for a human task. Workbench shows progress, recovery and deliverables. On completion, match the run in deliverable_list and call deliverable_get. Give a user-facing completion summary with findings and files. Keep internal run IDs, deliverable IDs, runtime IDs, host paths, hashes, commands and engine details out unless asked."

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
