package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/labstack/echo/v4"
)

type workflowTriggerSummary struct {
	Type string `json:"type"`
}

type workflowWorkerReference struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	AgentVersion int64  `json:"agent_version"`
}

type workflowLatestRun struct {
	RunID          string `json:"run_id"`
	Classification string `json:"classification"`
}

type workflowSummary struct {
	ID                string                    `json:"id"`
	Name              string                    `json:"name"`
	Description       string                    `json:"description"`
	Status            string                    `json:"status"`
	PublishedVersion  *int                      `json:"published_version"`
	DraftVersion      *int                      `json:"draft_version"`
	TriggerSummary    *workflowTriggerSummary   `json:"trigger_summary"`
	ReferencedWorkers []workflowWorkerReference `json:"referenced_workers"`
	LatestRun         *workflowLatestRun        `json:"latest_run"`
	CreatedAt         time.Time                 `json:"created_at"`
	UpdatedAt         time.Time                 `json:"updated_at"`
}

type workflowVersionSlot struct {
	Version       int             `json:"version"`
	UpdatedAt     time.Time       `json:"updated_at"`
	TriggerConfig json.RawMessage `json:"trigger_config"`
}

type workflowListResponse struct {
	Workflows []workflowSummary `json:"workflows"`
}

type workflowDetailResponse struct {
	TeamID    string               `json:"team_id"`
	Workflow  workflowSummary      `json:"workflow"`
	Published *workflowVersionSlot `json:"published"`
	Draft     *workflowVersionSlot `json:"draft"`
}

type workflowVersionResponse struct {
	WorkflowID      string          `json:"workflow_id"`
	TeamID          string          `json:"team_id"`
	Version         int             `json:"version"`
	Status          string          `json:"status"`
	TriggerConfig   json.RawMessage `json:"trigger_config"`
	GraphDefinition json.RawMessage `json:"graph_definition"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

// workflowVersionGroup holds the currently pointed published version and the
// mutable draft of one workflow. Either slot may be absent.
type workflowVersionGroup struct {
	published *workflow.TeamWorkflowVersion
	draft     *workflow.TeamWorkflowVersion
}

func (s *Server) handleListTeamWorkflows(c echo.Context) error {
	if s.Workflow == nil || s.OrgStore == nil || s.Registry == nil {
		return workflowError(
			c,
			http.StatusServiceUnavailable,
			"workflow_store_unavailable",
			"workflow store unavailable",
		)
	}
	ctx := c.Request().Context()
	workspaceID := getTenant(c)
	teamID := c.Param("id")
	teams, err := s.OrgStore.ListTeams(ctx, workspaceID)
	if err != nil {
		return workflowStoreFailure(c, err)
	}
	teamFound := false
	for _, team := range teams {
		if team.ID == teamID {
			teamFound = true
			break
		}
	}
	if !teamFound {
		return workflowError(c, http.StatusNotFound, "team_not_found", "team not found")
	}

	records, err := s.Workflow.ListByTeam(ctx, workspaceID, teamID)
	if err != nil {
		return workflowStoreFailure(c, err)
	}
	if c.QueryParam("include") != "archived" {
		active := make([]workflow.TeamWorkflow, 0, len(records))
		for _, record := range records {
			if record.Status != workflow.WorkflowStatusArchived {
				active = append(active, record)
			}
		}
		records = active
	}

	workflowIDs := make([]string, 0, len(records))
	for _, record := range records {
		workflowIDs = append(workflowIDs, record.ID)
	}
	versions, err := s.Workflow.ListVersionsByWorkflows(ctx, workspaceID, workflowIDs)
	if err != nil {
		return workflowStoreFailure(c, err)
	}
	agentsByID, err := s.workflowAgentsByID(ctx, workspaceID)
	if err != nil {
		return workflowStoreFailure(c, err)
	}

	items := make([]workflowSummary, 0, len(records))
	for _, record := range records {
		summary := buildWorkflowSummary(record, groupWorkflowVersions(record, versions), agentsByID)
		latestRun, err := s.resolveWorkflowLatestRun(ctx, workspaceID, record, versions)
		if err != nil {
			return writeWorkflowLatestRunError(c, err)
		}
		summary.LatestRun = latestRun
		items = append(items, summary)
	}
	return c.JSON(http.StatusOK, workflowListResponse{Workflows: items})
}

func (s *Server) handleGetWorkflow(c echo.Context) error {
	if s.Workflow == nil || s.Registry == nil {
		return workflowError(
			c,
			http.StatusServiceUnavailable,
			"workflow_store_unavailable",
			"workflow store unavailable",
		)
	}
	ctx := c.Request().Context()
	workspaceID := getTenant(c)
	record, err := s.Workflow.Get(ctx, workspaceID, c.Param("id"))
	if err != nil {
		if errors.Is(err, workflow.ErrNotFound) {
			return workflowError(c, http.StatusNotFound, "workflow_not_found", "workflow not found")
		}
		return workflowStoreFailure(c, err)
	}
	versions, err := s.Workflow.ListVersionsByWorkflows(ctx, workspaceID, []string{record.ID})
	if err != nil {
		return workflowStoreFailure(c, err)
	}
	agentsByID, err := s.workflowAgentsByID(ctx, workspaceID)
	if err != nil {
		return workflowStoreFailure(c, err)
	}

	group := groupWorkflowVersions(*record, versions)
	summary := buildWorkflowSummary(*record, group, agentsByID)
	latestRun, err := s.resolveWorkflowLatestRun(ctx, workspaceID, *record, versions)
	if err != nil {
		return writeWorkflowLatestRunError(c, err)
	}
	summary.LatestRun = latestRun

	var publishedSlot, draftSlot *workflowVersionSlot
	if group.published != nil {
		publishedSlot = &workflowVersionSlot{
			Version:       group.published.Version,
			UpdatedAt:     group.published.UpdatedAt.UTC(),
			TriggerConfig: group.published.TriggerConfig,
		}
	}
	if group.draft != nil {
		draftSlot = &workflowVersionSlot{
			Version:       group.draft.Version,
			UpdatedAt:     group.draft.UpdatedAt.UTC(),
			TriggerConfig: group.draft.TriggerConfig,
		}
	}
	return c.JSON(http.StatusOK, workflowDetailResponse{
		TeamID:    record.TeamID,
		Workflow:  summary,
		Published: publishedSlot,
		Draft:     draftSlot,
	})
}

func (s *Server) handleGetWorkflowVersion(c echo.Context) error {
	if s.Workflow == nil {
		return workflowError(
			c,
			http.StatusServiceUnavailable,
			"workflow_store_unavailable",
			"workflow store unavailable",
		)
	}
	versionNumber, ok := workflowVersionParam(c)
	if !ok {
		return workflowSchemaError(c)
	}
	ctx := c.Request().Context()
	workspaceID := getTenant(c)
	record, err := s.Workflow.Get(ctx, workspaceID, c.Param("id"))
	if err != nil {
		if errors.Is(err, workflow.ErrNotFound) {
			return workflowError(c, http.StatusNotFound, "workflow_not_found", "workflow not found")
		}
		return workflowStoreFailure(c, err)
	}
	version, err := s.Workflow.GetVersion(ctx, workspaceID, record.ID, versionNumber)
	if err != nil {
		if errors.Is(err, workflow.ErrNotFound) {
			return workflowError(c, http.StatusNotFound, "workflow_not_found", "workflow not found")
		}
		return workflowStoreFailure(c, err)
	}
	return c.JSON(http.StatusOK, workflowVersionResponse{
		WorkflowID:      version.WorkflowID,
		TeamID:          record.TeamID,
		Version:         version.Version,
		Status:          version.Status,
		TriggerConfig:   version.TriggerConfig,
		GraphDefinition: version.GraphDefinition,
		CreatedAt:       version.CreatedAt.UTC(),
		UpdatedAt:       version.UpdatedAt.UTC(),
	})
}

func (s *Server) workflowAgentsByID(
	ctx context.Context,
	workspaceID string,
) (map[string]registry.AgentRecord, error) {
	agents, err := s.Registry.List(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list workspace agents: %w", err)
	}
	agentsByID := make(map[string]registry.AgentRecord, len(agents))
	for _, agent := range agents {
		agentsByID[agent.ID] = agent
	}
	return agentsByID, nil
}

func groupWorkflowVersions(
	record workflow.TeamWorkflow,
	versions []workflow.TeamWorkflowVersion,
) workflowVersionGroup {
	group := workflowVersionGroup{}
	for index := range versions {
		version := &versions[index]
		if version.WorkflowID != record.ID {
			continue
		}
		switch version.Status {
		case workflow.VersionStatusDraft:
			if group.draft == nil || version.Version > group.draft.Version {
				group.draft = version
			}
		case workflow.VersionStatusPublished:
			if record.PublishedVersion != nil && version.Version == *record.PublishedVersion {
				group.published = version
			}
		}
	}
	return group
}

func buildWorkflowSummary(
	record workflow.TeamWorkflow,
	group workflowVersionGroup,
	agentsByID map[string]registry.AgentRecord,
) workflowSummary {
	summary := workflowSummary{
		ID:                record.ID,
		Name:              record.Name,
		Description:       record.Description,
		Status:            record.Status,
		PublishedVersion:  cloneWorkflowInt(record.PublishedVersion),
		ReferencedWorkers: make([]workflowWorkerReference, 0),
		CreatedAt:         record.CreatedAt.UTC(),
		UpdatedAt:         record.UpdatedAt.UTC(),
	}
	if group.draft != nil {
		draftVersion := group.draft.Version
		summary.DraftVersion = &draftVersion
	}
	selected := group.published
	if selected == nil {
		selected = group.draft
	}
	if selected != nil {
		summary.TriggerSummary = workflowTriggerSummaryFor(selected.TriggerConfig)
		summary.ReferencedWorkers = workflowReferencedWorkers(selected.GraphDefinition, agentsByID)
	}
	return summary
}

func workflowTriggerSummaryFor(raw json.RawMessage) *workflowTriggerSummary {
	var trigger struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &trigger); err != nil || trigger.Type == "" {
		return nil
	}
	return &workflowTriggerSummary{Type: trigger.Type}
}

// workflowReferencedWorkers extracts worker node references from one graph
// definition in first-seen node order, deduplicated by agent identity and
// version. Nodes referencing agents absent from the workspace registry are
// skipped because the read model cannot honestly name them.
func workflowReferencedWorkers(
	raw json.RawMessage,
	agentsByID map[string]registry.AgentRecord,
) []workflowWorkerReference {
	workers := make([]workflowWorkerReference, 0)
	var graph struct {
		Nodes []struct {
			Type   string `json:"type"`
			Config struct {
				AgentID      string `json:"agent_id"`
				AgentVersion int64  `json:"agent_version"`
			} `json:"config"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(raw, &graph); err != nil {
		return workers
	}
	seen := make(map[string]struct{})
	for _, node := range graph.Nodes {
		if node.Type != "worker" || node.Config.AgentID == "" {
			continue
		}
		agent, ok := agentsByID[node.Config.AgentID]
		if !ok {
			continue
		}
		key := node.Config.AgentID + "\x00" + strconv.FormatInt(node.Config.AgentVersion, 10)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		workers = append(workers, workflowWorkerReference{
			ID:           agent.ID,
			Name:         agent.Name,
			AgentVersion: node.Config.AgentVersion,
		})
	}
	return workers
}

// resolveWorkflowLatestRun finds the newest fixed-workflow run snapshot of one
// workflow across every published version and classifies it through the same
// team reader family that serves runs?view=workflow. It returns nil when the
// workflow has never run.
func (s *Server) resolveWorkflowLatestRun(
	ctx context.Context,
	workspaceID string,
	record workflow.TeamWorkflow,
	versions []workflow.TeamWorkflowVersion,
) (*workflowLatestRun, error) {
	publishedVersions := make([]int, 0, 1)
	for index := range versions {
		if versions[index].WorkflowID == record.ID &&
			versions[index].Status == workflow.VersionStatusPublished {
			publishedVersions = append(publishedVersions, versions[index].Version)
		}
	}
	if len(publishedVersions) == 0 {
		return nil, nil
	}
	if s.Snapshots == nil || s.TeamReader == nil {
		return nil, errTeamReadStoreUnavailable
	}
	var latest *snapshot.TeamRunSnapshot
	for _, version := range publishedVersions {
		snapshots, err := s.Snapshots.ListByWorkflow(ctx, workspaceID, record.TeamID, record.ID, version)
		if err != nil {
			return nil, fmt.Errorf("list workflow run snapshots: %w", err)
		}
		for index := range snapshots {
			candidate := snapshots[index]
			if latest == nil || workflowSnapshotIsLater(candidate, *latest) {
				latest = &candidate
			}
		}
	}
	if latest == nil {
		return nil, nil
	}
	report, err := s.TeamReader.read(ctx, workspaceID, teamSelector{
		View:            teamViewWorkflow,
		TeamID:          record.TeamID,
		WorkflowID:      record.ID,
		WorkflowVersion: latest.WorkflowVersion,
		AggregationMode: aggregationModeRootSubtree,
	})
	// The aggregation invariant only gates usage totals; per-run
	// classifications remain the authoritative nine-state judgement and stay
	// usable when totals fail to reconcile.
	if err != nil && !errors.Is(err, errTeamAggregationInvariant) {
		return nil, err
	}
	for _, row := range report.Rows {
		if row.RunID == latest.RunID {
			return &workflowLatestRun{
				RunID:          row.RunID,
				Classification: row.Classification,
			}, nil
		}
	}
	return nil, fmt.Errorf("workflow latest run %q missing from read report", latest.RunID)
}

func workflowSnapshotIsLater(candidate, current snapshot.TeamRunSnapshot) bool {
	if !candidate.CreatedAt.Equal(current.CreatedAt) {
		return candidate.CreatedAt.After(current.CreatedAt)
	}
	return candidate.RunID > current.RunID
}

func writeWorkflowLatestRunError(c echo.Context, err error) error {
	if errors.Is(err, errTeamReadStoreUnavailable) {
		return c.JSON(http.StatusServiceUnavailable, struct {
			Error     string `json:"error"`
			Retryable bool   `json:"retryable"`
		}{
			Error:     errTeamReadStoreUnavailable.Error(),
			Retryable: true,
		})
	}
	return workflowStoreFailure(c, err)
}

func cloneWorkflowInt(value *int) *int {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}
