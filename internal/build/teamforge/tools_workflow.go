package teamforge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/kernel/org"
	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

type teamWorkflowSummaryJSON struct {
	WorkflowID       string `json:"workflow_id"`
	Name             string `json:"name"`
	Status           string `json:"status"`
	PublishedVersion *int   `json:"published_version"`
}

func summarizeTeamWorkflows(rows []workflow.TeamWorkflow) []teamWorkflowSummaryJSON {
	out := make([]teamWorkflowSummaryJSON, 0, len(rows))
	for _, row := range rows {
		out = append(out, teamWorkflowSummaryJSON{
			WorkflowID:       row.ID,
			Name:             row.Name,
			Status:           row.Status,
			PublishedVersion: row.PublishedVersion,
		})
	}
	return out
}

type workflowVersionJSON struct {
	Version         int             `json:"version"`
	Status          string          `json:"status"`
	TriggerConfig   json.RawMessage `json:"trigger_config"`
	GraphDefinition json.RawMessage `json:"graph_definition"`
	CreatedBy       string          `json:"created_by"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
	PublishedAt     *time.Time      `json:"published_at,omitempty"`
}

type getWorkflowJSON struct {
	Status           string                        `json:"status"`
	Workflow         *workflow.TeamWorkflow        `json:"workflow"`
	Versions         []workflowVersionJSON         `json:"versions"`
	RequestedVersion *workflow.TeamWorkflowVersion `json:"requested_version,omitempty"`
	CreationAllowed  bool                          `json:"creation_allowed,omitempty"`
	Guidance         string                        `json:"guidance,omitempty"`
}

type workflowTeamLookupJSON struct {
	Status    string                    `json:"status"`
	TeamID    string                    `json:"team_id"`
	TeamName  string                    `json:"team_name"`
	Workflows []teamWorkflowSummaryJSON `json:"workflows"`
	Guidance  string                    `json:"guidance"`
}

// getWorkflow implements tf_get_workflow: the workflow row plus its full
// version list (draft and published contents) via the existing workflow
// store read paths; an optional exact version is read with GetVersion.
func (d *ReadToolsDispatcher) getWorkflow(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	if d.deps.Workflows == nil {
		return toolError(call.ID, "workflow read is unavailable"), nil
	}
	var input struct {
		WorkflowID string `json:"workflow_id"`
		Version    int    `json:"version"`
	}
	if err := json.Unmarshal([]byte(call.Args), &input); err != nil {
		return toolError(call.ID, "invalid input: "+err.Error()), nil
	}
	if input.WorkflowID == "" {
		return toolError(call.ID, "workflow_id is required"), nil
	}
	workflowRow, err := d.deps.Workflows.Get(ctx, d.workspaceID, input.WorkflowID)
	if err != nil {
		if errors.Is(err, workflow.ErrNotFound) {
			if d.isPendingOptimizeWorkflow(ctx, call, input.WorkflowID) {
				return toolJSON(call.ID, getWorkflowJSON{
					Status:          "pending_create",
					Workflow:        nil,
					Versions:        []workflowVersionJSON{},
					CreationAllowed: true,
					Guidance:        "This workflow is an authorized future asset for an optimize run. It does not exist yet; include its template configuration in the TeamBlueprint and call tf_blueprint_plan.",
				})
			}
			if fallback, ok, fallbackErr := d.workflowsForTeamIdentifier(ctx, input.WorkflowID); fallbackErr != nil {
				return toolError(call.ID, fallbackErr.Error()), nil
			} else if ok {
				return toolJSON(call.ID, fallback)
			}
		}
		return toolError(call.ID, err.Error()), nil
	}
	versions, err := d.deps.Workflows.ListVersionsByWorkflows(ctx, d.workspaceID, []string{input.WorkflowID})
	if err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	out := getWorkflowJSON{
		Status:   "exists",
		Workflow: workflowRow,
		Versions: make([]workflowVersionJSON, 0, len(versions)),
	}
	for _, version := range versions {
		out.Versions = append(out.Versions, workflowVersionJSON{
			Version:         version.Version,
			Status:          version.Status,
			TriggerConfig:   version.TriggerConfig,
			GraphDefinition: version.GraphDefinition,
			CreatedBy:       version.CreatedBy,
			CreatedAt:       version.CreatedAt,
			UpdatedAt:       version.UpdatedAt,
			PublishedAt:     version.PublishedAt,
		})
	}
	if input.Version >= 1 {
		exact, err := d.deps.Workflows.GetVersion(ctx, d.workspaceID, input.WorkflowID, input.Version)
		if err != nil {
			return toolError(call.ID, err.Error()), nil
		}
		out.RequestedVersion = exact
	}
	return toolJSON(call.ID, out)
}

func (d *ReadToolsDispatcher) workflowsForTeamIdentifier(
	ctx context.Context,
	identifier string,
) (workflowTeamLookupJSON, bool, error) {
	if d.deps.Teams == nil {
		return workflowTeamLookupJSON{}, false, nil
	}
	teams, err := d.deps.Teams.ListTeams(ctx, d.workspaceID)
	if err != nil {
		return workflowTeamLookupJSON{}, false, err
	}
	var matched org.Team
	for _, team := range teams {
		if team.ID == identifier || team.Name == identifier {
			matched = team
			break
		}
	}
	if matched.ID == "" {
		return workflowTeamLookupJSON{}, false, nil
	}
	rows, err := d.deps.Workflows.ListByTeam(ctx, d.workspaceID, matched.ID)
	if err != nil {
		return workflowTeamLookupJSON{}, false, err
	}
	summaries := summarizeTeamWorkflows(rows)
	labels := make([]string, 0, len(summaries))
	for _, item := range summaries {
		labels = append(labels, fmt.Sprintf("%s（%s）", item.Name, item.WorkflowID))
	}
	list := "无"
	if len(labels) > 0 {
		list = strings.Join(labels, "、")
	}
	return workflowTeamLookupJSON{
		Status:    "team_workflows",
		TeamID:    matched.ID,
		TeamName:  matched.Name,
		Workflows: summaries,
		Guidance:  fmt.Sprintf("你传入的是团队标识。该团队的工作流：%s。请用 workflow_id 重试。", list),
	}, true, nil
}

func (d *ReadToolsDispatcher) isPendingOptimizeWorkflow(
	ctx context.Context,
	call contract.ToolCall,
	workflowID string,
) bool {
	if d.discovery || d.runs == nil {
		return false
	}
	buildRunID, err := d.resolveBuildRunID(call.Args)
	if err != nil {
		return false
	}
	run, err := d.runs.GetBuildRun(ctx, d.workspaceID, buildRunID)
	if err != nil || run.Status != teambuild.StatusPlanning {
		return false
	}
	for _, ref := range pendingCreateAssets(run) {
		if ref.Kind == "workflow" && ref.ID == workflowID {
			return true
		}
	}
	return false
}
