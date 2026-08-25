package teamforge

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jinyitao123/loom/contract"
)

type agentSummaryJSON struct {
	Name        string    `json:"name"`
	ID          string    `json:"id"`
	DisplayName string    `json:"display_name,omitempty"`
	Role        string    `json:"role,omitempty"`
	Version     int       `json:"version"`
	TeamID      string    `json:"team_id,omitempty"`
	Engine      string    `json:"engine,omitempty"`
	RuntimeID   string    `json:"runtime_id,omitempty"`
	Model       string    `json:"model,omitempty"`
	Archived    bool      `json:"archived"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type listAgentsJSON struct {
	Agents []agentSummaryJSON `json:"agents"`
	Total  int                `json:"total"`
}

// listAgents implements tf_list_agents via the existing AgentRegistry.List
// read path. The archived annotation is the registry's soft-delete flag; the
// existing list read path returns live agents only.
func (d *ReadToolsDispatcher) listAgents(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	if d.deps.Agents == nil {
		return toolError(call.ID, "agent registry read is unavailable"), nil
	}
	records, err := d.deps.Agents.List(ctx, d.workspaceID)
	if err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	out := listAgentsJSON{Agents: make([]agentSummaryJSON, 0, len(records))}
	for _, rec := range records {
		out.Agents = append(out.Agents, agentSummaryJSON{
			Name:        rec.Name,
			ID:          rec.ID,
			DisplayName: rec.DisplayName,
			Role:        rec.Role,
			Version:     rec.Version,
			TeamID:      rec.TeamID,
			Engine:      rec.Engine,
			RuntimeID:   rec.RuntimeID,
			Model:       rec.Model,
			Archived:    rec.Deleted,
			UpdatedAt:   rec.UpdatedAt,
		})
	}
	out.Total = len(out.Agents)
	return toolJSON(call.ID, out)
}

// getAgentVersion implements tf_get_agent_version via registry.GetVersion:
// one exact immutable version, never a fallback to latest.
func (d *ReadToolsDispatcher) getAgentVersion(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	if d.deps.Agents == nil {
		return toolError(call.ID, "agent registry read is unavailable"), nil
	}
	var input struct {
		AgentID string `json:"agent_id"`
		Version int    `json:"version"`
	}
	if err := json.Unmarshal([]byte(call.Args), &input); err != nil {
		return toolError(call.ID, "invalid input: "+err.Error()), nil
	}
	if input.AgentID == "" {
		return toolError(call.ID, "agent_id is required"), nil
	}
	if input.Version < 1 {
		return toolError(call.ID, "version must be a positive integer"), nil
	}
	rec, err := d.deps.Agents.GetVersion(ctx, d.workspaceID, input.AgentID, input.Version)
	if err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	content, err := json.Marshal(rec)
	if err != nil {
		return toolError(call.ID, "failed to encode agent version: "+err.Error()), nil
	}
	return &contract.ToolResult{CallID: call.ID, Content: string(content)}, nil
}
