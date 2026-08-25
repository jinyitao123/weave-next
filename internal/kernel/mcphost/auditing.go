package mcphost

import (
	"context"
	"log/slog"
	"strings"

	"github.com/jinyitao123/loom/contract"
)

const maxAuditDetailRunes = 200

// AuditRecorder records one workspace-scoped MCP tool invocation.
type AuditRecorder interface {
	Record(ctx context.Context, workspaceID, agent, tool, status, detail string) error
}

// ServerAuditRecorder records stable registry server identity for new refs.
type ServerAuditRecorder interface {
	RecordServer(ctx context.Context, workspaceID, agent, serverID, tool, status, detail string) error
}

// GovernanceAuditRecorder supports both stable refs and legacy inline calls.
type GovernanceAuditRecorder interface {
	AuditRecorder
	ServerAuditRecorder
}

// AuditingDispatcher records the outcome of calls handled by an inner dispatcher.
type AuditingDispatcher struct {
	inner          contract.ToolDispatcher
	recorder       AuditRecorder
	serverRecorder ServerAuditRecorder
	workspaceID    string
	agent          string
	serverID       string
	writeTools     []string
}

// NewAuditingDispatcherForServer attaches stable server identity to every new
// registry-ref invocation.
func NewAuditingDispatcherForServer(
	inner contract.ToolDispatcher,
	recorder ServerAuditRecorder,
	workspaceID, agent, serverID string,
) *AuditingDispatcher {
	return &AuditingDispatcher{
		inner: inner, serverRecorder: recorder, workspaceID: workspaceID,
		agent: agent, serverID: serverID,
	}
}

// NewAuditingDispatcher wraps an MCP dispatcher with best-effort auditing.
func NewAuditingDispatcher(
	inner contract.ToolDispatcher,
	recorder AuditRecorder,
	workspaceID, agent string,
) *AuditingDispatcher {
	return &AuditingDispatcher{
		inner:       inner,
		recorder:    recorder,
		workspaceID: workspaceID,
		agent:       agent,
	}
}

// ListTools passes through to the inner dispatcher.
func (d *AuditingDispatcher) ListTools(ctx context.Context) ([]contract.ToolDef, error) {
	return d.inner.ListTools(ctx)
}

// Dispatch passes through the call and records its outcome without changing it.
func (d *AuditingDispatcher) Dispatch(
	ctx context.Context,
	call contract.ToolCall,
) (*contract.ToolResult, error) {
	result, err := d.inner.Dispatch(ctx, call)
	status := "ok"
	detail := classificationAuditDetail(ClassifyTool(call.Name, d.writeTools))
	if err != nil {
		status = "error"
		detail = joinAuditDetail(detail, err.Error())
	} else if result != nil && result.IsError {
		status = "error"
		detail = joinAuditDetail(detail, result.Content)
	}
	detail = sanitizeAuditDetail(detail, call.Args)

	if d.recorder != nil || d.serverRecorder != nil {
		var recordErr error
		if d.serverID != "" {
			recordErr = d.serverRecorder.RecordServer(
				ctx, d.workspaceID, d.agent, d.serverID, call.Name, status, detail,
			)
		} else {
			recordErr = d.recorder.Record(ctx, d.workspaceID, d.agent, call.Name, status, detail)
		}
		if recordErr != nil {
			slog.Warn("MCP audit record failed",
				"workspace_id", d.workspaceID,
				"agent", d.agent,
				"server_id", d.serverID,
				"tool", call.Name,
				"error", recordErr,
			)
		}
	}
	return result, err
}

func classificationAuditDetail(classification ToolClassification) string {
	if classification == ToolHeuristicWrite {
		return "classification=heuristic_write"
	}
	return ""
}

func joinAuditDetail(annotation, detail string) string {
	if annotation == "" {
		return detail
	}
	if detail == "" {
		return annotation
	}
	return annotation + "; " + detail
}

func sanitizeAuditDetail(detail, rawArgs string) string {
	if rawArgs != "" {
		detail = strings.ReplaceAll(detail, rawArgs, "[redacted]")
	}
	runes := []rune(detail)
	if len(runes) > maxAuditDetailRunes {
		return string(runes[:maxAuditDetailRunes])
	}
	return detail
}

var _ contract.ToolDispatcher = (*AuditingDispatcher)(nil)
