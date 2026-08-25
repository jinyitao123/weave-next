package mcphost

import (
	"context"
	"strings"

	"github.com/jinyitao123/loom/contract"
)

// writeToolVerbs drives the ADVISORY heuristic classification only (audit
// annotation via ClassifyTool → classification=heuristic_write). It never gates:
// the WriteGateDispatcher parks solely ToolDeclaredWrite. Broadening it just
// makes more undeclared-but-write-shaped tools observable in the audit trail
// (e.g. a `*_confirm`/`*_approve` tool attached without a write_tools entry),
// which is the whole point of the heuristic signal.
var writeToolVerbs = []string{
	"create",
	"update",
	"delete",
	"write",
	"send",
	"put",
	"post",
	"remove",
	"set",
	"insert",
	"modify",
	"confirm",
	"approve",
	"commit",
	"apply",
	"submit",
	"register",
	"publish",
	"cancel",
	"revoke",
	"grant",
	"upsert",
	"provision",
	"save",
	"sync",
}

// ToolClassification separates authoritative write declarations from the
// heuristic signal retained for audit annotation.
type ToolClassification string

const (
	ToolRead           ToolClassification = "read"
	ToolDeclaredWrite  ToolClassification = "declared_write"
	ToolHeuristicWrite ToolClassification = "heuristic_write"
)

// ClassifyTool classifies one tool. ToolDeclaredWrite is rejected by the write
// gate; heuristic matches are advisory audit metadata.
func ClassifyTool(name string, declared []string) ToolClassification {
	if isDeclaredWriteTool(name, declared) {
		return ToolDeclaredWrite
	}
	lowerName := strings.ToLower(name)
	for _, verb := range writeToolVerbs {
		if strings.Contains(lowerName, verb) {
			return ToolHeuristicWrite
		}
	}
	return ToolRead
}

// IsWriteTool reports declared or heuristic write classification.
func IsWriteTool(name string, declared []string) bool {
	return ClassifyTool(name, declared) != ToolRead
}

func isDeclaredWriteTool(name string, declared []string) bool {
	for _, tool := range declared {
		if strings.EqualFold(name, tool) {
			return true
		}
	}
	return false
}

// WriteGateDispatcher rejects declared write calls instead of forwarding them.
type WriteGateDispatcher struct {
	inner          contract.ToolDispatcher
	workspaceID    string
	agent          string
	serverID       string
	serverURL      string
	conversationID string
	writeTools     []string
}

// NewWriteGateDispatcherForServer wraps a stable registry server ref and
// persists its server ID with any parked action.
func NewWriteGateDispatcherForServer(
	inner contract.ToolDispatcher,
	workspaceID, agent, serverID, serverURL, conversationID string,
	writeTools []string,
) *WriteGateDispatcher {
	return &WriteGateDispatcher{
		inner:       inner,
		workspaceID: workspaceID, agent: agent, serverID: serverID,
		serverURL: serverURL, conversationID: conversationID,
		writeTools: writeTools,
	}
}

// NewWriteGateDispatcher wraps one MCP server dispatcher with its write policy.
func NewWriteGateDispatcher(
	inner contract.ToolDispatcher,
	workspaceID, agent, serverURL, conversationID string,
	writeTools []string,
) *WriteGateDispatcher {
	return &WriteGateDispatcher{
		inner:          inner,
		workspaceID:    workspaceID,
		agent:          agent,
		serverURL:      serverURL,
		conversationID: conversationID,
		writeTools:     writeTools,
	}
}

// ListTools passes through to the wrapped MCP server.
func (d *WriteGateDispatcher) ListTools(ctx context.Context) ([]contract.ToolDef, error) {
	return d.inner.ListTools(ctx)
}

// Dispatch rejects declared writes and forwards all other calls.
func (d *WriteGateDispatcher) Dispatch(
	ctx context.Context,
	call contract.ToolCall,
) (*contract.ToolResult, error) {
	if isDeclaredWriteTool(call.Name, d.writeTools) {
		return &contract.ToolResult{
			CallID:  call.ID,
			IsError: true,
			Content: "该工具是声明写工具，当前产品模式不允许业务团队直接执行外部不可逆动作。",
		}, nil
	}
	return d.inner.Dispatch(ctx, call)
}

var _ contract.ToolDispatcher = (*WriteGateDispatcher)(nil)
