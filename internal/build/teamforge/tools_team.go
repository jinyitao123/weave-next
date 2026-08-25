package teamforge

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/kernel/org"
)

type rosterEntryJSON struct {
	WorkerAgentID      string    `json:"worker_agent_id"`
	Duty               string    `json:"duty"`
	WhenToUse          string    `json:"when_to_use"`
	ContextInstruction string    `json:"context_instruction,omitempty"`
	AllowedKinds       []string  `json:"allowed_kinds"`
	DefaultKind        string    `json:"default_kind"`
	ResultRequirement  string    `json:"result_requirement,omitempty"`
	Enabled            bool      `json:"enabled"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type getTeamJSON struct {
	Team        org.Team                  `json:"team"`
	Roster      []rosterEntryJSON         `json:"roster"`
	RosterCount int                       `json:"roster_count"`
	Workflows   []teamWorkflowSummaryJSON `json:"workflows"`
}

// getTeam implements tf_get_team using the existing team list read path
// (org.Store.ListTeams, filtered by ID exactly like the API), the complete
// team-worker roster, and workflow summaries from workflow.Store.ListByTeam.
func (d *ReadToolsDispatcher) getTeam(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	if d.deps.Teams == nil || d.deps.Roster == nil || d.deps.Workflows == nil {
		return toolError(call.ID, "team read is unavailable"), nil
	}
	var input struct {
		TeamID string `json:"team_id"`
	}
	if err := json.Unmarshal([]byte(call.Args), &input); err != nil {
		return toolError(call.ID, "invalid input: "+err.Error()), nil
	}
	if input.TeamID == "" {
		return toolError(call.ID, "team_id is required"), nil
	}
	teams, err := d.deps.Teams.ListTeams(ctx, d.workspaceID)
	if err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	team, found := findTeamByID(teams, input.TeamID)
	if !found {
		return toolError(call.ID, "team not found"), nil
	}
	workers, err := d.deps.Roster.ListByTeam(ctx, d.workspaceID, input.TeamID)
	if err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	workflows, err := d.deps.Workflows.ListByTeam(ctx, d.workspaceID, input.TeamID)
	if err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	out := getTeamJSON{
		Team:      team,
		Roster:    make([]rosterEntryJSON, 0, len(workers)),
		Workflows: summarizeTeamWorkflows(workflows),
	}
	for _, worker := range workers {
		out.Roster = append(out.Roster, rosterEntryJSON{
			WorkerAgentID:      worker.WorkerAgentID,
			Duty:               worker.Duty,
			WhenToUse:          worker.WhenToUse,
			ContextInstruction: worker.ContextInstruction,
			AllowedKinds:       worker.AllowedKinds,
			DefaultKind:        worker.DefaultKind,
			ResultRequirement:  worker.ResultRequirement,
			Enabled:            worker.Enabled,
			CreatedAt:          worker.CreatedAt,
			UpdatedAt:          worker.UpdatedAt,
		})
	}
	out.RosterCount = len(out.Roster)
	return toolJSON(call.ID, out)
}

func findTeamByID(teams []org.Team, teamID string) (org.Team, bool) {
	for _, team := range teams {
		if team.ID == teamID {
			return team, true
		}
	}
	return org.Team{}, false
}

type listTeamsEntryJSON struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Objective    string `json:"objective"`
	Status       string `json:"status"`
	LeadAvatarID string `json:"lead_avatar_id"`
}

// listTeams implements tf_list_teams with deliberately minimal fields: the
// tool exists for duplicate-construction checks in create mode and real
// target selection in optimize mode, so roster details and dispatch rules
// stay with tf_get_team / tf_get_dispatch_rules. Built-in "__" platform
// teams are excluded at the query layer so the model physically cannot see
// them.
func (d *ReadToolsDispatcher) listTeams(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	if d.deps.Teams == nil {
		return toolError(call.ID, "team read is unavailable"), nil
	}
	lister, ok := d.deps.Teams.(BusinessTeamLister)
	if !ok {
		return toolError(call.ID, "team list is unavailable"), nil
	}
	teams, err := lister.ListBusinessTeams(ctx, d.workspaceID)
	if err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	out := make([]listTeamsEntryJSON, 0, len(teams))
	for _, team := range teams {
		out = append(out, listTeamsEntryJSON{
			ID:           team.ID,
			Name:         team.Name,
			Objective:    team.Objective,
			Status:       team.Status,
			LeadAvatarID: team.LeadAvatarID,
		})
	}
	return toolJSON(call.ID, map[string]any{"teams": out, "total": len(out)})
}

// getDispatchRules implements tf_get_dispatch_rules via the existing
// org.Store.GetTeamDispatchRules read path (free-collaboration rules with
// platform defaults when no rule row is stored).
func (d *ReadToolsDispatcher) getDispatchRules(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	if d.deps.DispatchRules == nil {
		return toolError(call.ID, "dispatch rules read is unavailable"), nil
	}
	var input struct {
		TeamID string `json:"team_id"`
	}
	if err := json.Unmarshal([]byte(call.Args), &input); err != nil {
		return toolError(call.ID, "invalid input: "+err.Error()), nil
	}
	if input.TeamID == "" {
		return toolError(call.ID, "team_id is required"), nil
	}
	rules, err := d.deps.DispatchRules.GetTeamDispatchRules(ctx, d.workspaceID, input.TeamID)
	if err != nil {
		if errors.Is(err, org.ErrTeamNotFound) {
			return toolError(call.ID, "team not found"), nil
		}
		return toolError(call.ID, err.Error()), nil
	}
	return toolJSON(call.ID, rules)
}
