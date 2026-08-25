package api

import (
	"context"
	"fmt"
	"net/http"
	"sort"

	"github.com/jinyitao123/weave/internal/kernel/org"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/labstack/echo/v4"
)

// teamSummaryErrorCodeReadStoreUnavailable is the only stable degradation code
// allowed by the C-ORGREAD contract for summary read failures.
const teamSummaryErrorCodeReadStoreUnavailable = "read_store_unavailable"

type teamMembershipAgent struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
}

type teamLeadMembership struct {
	TeamID     string `json:"team_id"`
	TeamName   string `json:"team_name"`
	TeamStatus string `json:"team_status"`
}

type teamWorkerMembership struct {
	TeamID       string   `json:"team_id"`
	TeamName     string   `json:"team_name"`
	TeamStatus   string   `json:"team_status"`
	Duty         string   `json:"duty"`
	AllowedKinds []string `json:"allowed_kinds"`
	DefaultKind  string   `json:"default_kind"`
	Enabled      bool     `json:"enabled"`
}

type teamMembershipsResponse struct {
	Agent    teamMembershipAgent    `json:"agent"`
	LeadOf   []teamLeadMembership   `json:"lead_of"`
	WorkerOf []teamWorkerMembership `json:"worker_of"`
}

type teamSummaryLatestRun struct {
	RunID          string `json:"run_id"`
	Classification string `json:"classification"`
}

// teamSummary is the single-team detail summary (contract C-ORGREAD §2.2);
// it intentionally has no attention field.
type teamSummary struct {
	WorkerCount            int                   `json:"worker_count"`
	ActiveWorkflowCount    int                   `json:"active_workflow_count"`
	PublishedWorkflowCount int                   `json:"published_workflow_count"`
	LatestRun              *teamSummaryLatestRun `json:"latest_run"`
}

// teamRosterSummary extends the detail summary with the roster attention flag
// (contract C-ORGREAD §2.3).
type teamRosterSummary struct {
	teamSummary
	Attention bool `json:"attention"`
}

type teamSummaryError struct {
	Code string `json:"code"`
}

type teamDetailSummaryResponse struct {
	TeamRoster
	Summary      *teamSummary      `json:"summary"`
	SummaryError *teamSummaryError `json:"summary_error,omitempty"`
}

type teamRosterSummaryItem struct {
	TeamRoster
	Summary      *teamRosterSummary `json:"summary"`
	SummaryError *teamSummaryError  `json:"summary_error,omitempty"`
}

func (s *Server) handleGetAgentTeamMemberships(c echo.Context) error {
	ctx := c.Request().Context()
	workspaceID := getTenant(c)
	if s.OrgStore == nil || s.Registry == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "org read store unavailable"})
	}
	agent, err := s.Registry.Get(ctx, workspaceID, c.Param("name"))
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
	}
	teams, err := s.OrgStore.ListTeams(ctx, workspaceID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	teamWorkers, err := s.Registry.ListTeamWorkers(ctx, workspaceID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	teamsByID := make(map[string]org.Team, len(teams))
	leadOf := make([]teamLeadMembership, 0)
	for _, team := range teams {
		teamsByID[team.ID] = team
		if team.LeadAvatarID == agent.ID {
			leadOf = append(leadOf, teamLeadMembership{
				TeamID:     team.ID,
				TeamName:   team.Name,
				TeamStatus: team.Status,
			})
		}
	}
	workerOf := make([]teamWorkerMembership, 0)
	for _, worker := range teamWorkers {
		if worker.WorkerAgentID != agent.ID {
			continue
		}
		team, ok := teamsByID[worker.TeamID]
		if !ok {
			continue
		}
		workerOf = append(workerOf, teamWorkerMembership{
			TeamID:       team.ID,
			TeamName:     team.Name,
			TeamStatus:   team.Status,
			Duty:         worker.Duty,
			AllowedKinds: append([]string{}, worker.AllowedKinds...),
			DefaultKind:  worker.DefaultKind,
			Enabled:      worker.Enabled,
		})
	}
	sort.Slice(leadOf, func(a, b int) bool {
		if leadOf[a].TeamName != leadOf[b].TeamName {
			return leadOf[a].TeamName < leadOf[b].TeamName
		}
		return leadOf[a].TeamID < leadOf[b].TeamID
	})
	sort.Slice(workerOf, func(a, b int) bool {
		if workerOf[a].TeamName != workerOf[b].TeamName {
			return workerOf[a].TeamName < workerOf[b].TeamName
		}
		return workerOf[a].TeamID < workerOf[b].TeamID
	})

	return c.JSON(http.StatusOK, teamMembershipsResponse{
		Agent: teamMembershipAgent{
			ID:          agent.ID,
			Name:        agent.Name,
			DisplayName: agent.DisplayName,
			Role:        agent.Role,
		},
		LeadOf:   leadOf,
		WorkerOf: workerOf,
	})
}

func (s *Server) handleGetTeam(c echo.Context) error {
	ctx := c.Request().Context()
	workspaceID := getTenant(c)
	if s.OrgStore == nil || s.Registry == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "org read store unavailable"})
	}
	teams, err := s.OrgStore.ListTeams(ctx, workspaceID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	teamID := c.Param("id")
	team, found := findTeamByID(teams, teamID)
	if !found {
		return c.JSON(http.StatusNotFound, map[string]string{"error": org.ErrTeamNotFound.Error()})
	}
	roster, err := s.buildSingleTeamRoster(ctx, workspaceID, team)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	if c.QueryParam("include") != "summary" {
		return c.JSON(http.StatusOK, roster)
	}

	summary, _, summaryErr := s.buildTeamSummary(ctx, workspaceID, team.ID, len(roster.Workers))
	response := teamDetailSummaryResponse{TeamRoster: roster, Summary: summary}
	if summaryErr != nil {
		response.SummaryError = &teamSummaryError{Code: teamSummaryErrorCodeReadStoreUnavailable}
	}
	return c.JSON(http.StatusOK, response)
}

func (s *Server) buildSingleTeamRoster(
	ctx context.Context,
	workspaceID string,
	team org.Team,
) (TeamRoster, error) {
	agents, err := s.Registry.List(ctx, workspaceID)
	if err != nil {
		return TeamRoster{}, err
	}
	teamWorkers, err := s.Registry.ListTeamWorkers(ctx, workspaceID)
	if err != nil {
		return TeamRoster{}, err
	}
	return aggregateTeamRosters([]org.Team{team}, agents, teamWorkers)[0], nil
}

func findTeamByID(teams []org.Team, teamID string) (org.Team, bool) {
	for _, team := range teams {
		if team.ID == teamID {
			return team, true
		}
	}
	return org.Team{}, false
}

func (s *Server) buildTeamRosterSummaryItems(
	ctx context.Context,
	workspaceID string,
	rosters []TeamRoster,
) []teamRosterSummaryItem {
	items := make([]teamRosterSummaryItem, len(rosters))
	for index, roster := range rosters {
		summary, attention, err := s.buildTeamSummary(ctx, workspaceID, roster.Team.ID, len(roster.Workers))
		item := teamRosterSummaryItem{TeamRoster: roster}
		if err != nil {
			item.SummaryError = &teamSummaryError{Code: teamSummaryErrorCodeReadStoreUnavailable}
		} else {
			item.Summary = &teamRosterSummary{teamSummary: *summary, Attention: attention}
		}
		items[index] = item
	}
	return items
}

// buildTeamSummary assembles the contract C-ORGREAD summary for one team.
// Run facts (latest_run classification and the attention defect buckets) reuse
// the teamReader/PGRunLifecycleReader family; any failure degrades the whole
// summary instead of failing the enclosing response.
func (s *Server) buildTeamSummary(
	ctx context.Context,
	workspaceID string,
	teamID string,
	workerCount int,
) (*teamSummary, bool, error) {
	if s.Workflow == nil || s.Snapshots == nil || s.TeamReader == nil {
		return nil, false, errTeamReadStoreUnavailable
	}
	workflows, err := s.Workflow.ListByTeam(ctx, workspaceID, teamID)
	if err != nil {
		return nil, false, err
	}
	summary := &teamSummary{WorkerCount: workerCount}
	for _, teamWorkflow := range workflows {
		if teamWorkflow.Status == workflow.WorkflowStatusActive {
			summary.ActiveWorkflowCount++
		}
		if teamWorkflow.PublishedVersion != nil {
			summary.PublishedWorkflowCount++
		}
	}

	snapshots, err := s.Snapshots.ListByTeam(ctx, workspaceID, teamID)
	if err != nil {
		return nil, false, err
	}
	report, err := s.TeamReader.read(ctx, workspaceID, teamSelector{
		View:            teamViewTeam,
		TeamID:          teamID,
		AggregationMode: aggregationModeAllExclusive,
	})
	if err != nil {
		return nil, false, err
	}
	if latest, ok := latestTeamRunSnapshot(snapshots); ok {
		classification, found := teamReportClassification(report, latest.RunID)
		if !found {
			return nil, false, fmt.Errorf("latest team run %q missing from team read report", latest.RunID)
		}
		summary.LatestRun = &teamSummaryLatestRun{RunID: latest.RunID, Classification: classification}
	}
	return summary, teamDiagnosticsNeedAttention(report.Diagnostics), nil
}

func latestTeamRunSnapshot(snapshots []snapshot.TeamRunSnapshot) (snapshot.TeamRunSnapshot, bool) {
	if len(snapshots) == 0 {
		return snapshot.TeamRunSnapshot{}, false
	}
	latest := snapshots[0]
	for _, candidate := range snapshots[1:] {
		if candidate.CreatedAt.After(latest.CreatedAt) ||
			(candidate.CreatedAt.Equal(latest.CreatedAt) && candidate.RunID > latest.RunID) {
			latest = candidate
		}
	}
	return latest, true
}

func teamReportClassification(report teamReadReport, runID string) (string, bool) {
	for _, row := range report.Rows {
		if row.RunID == runID {
			return row.Classification, true
		}
	}
	return "", false
}

// teamDiagnosticsNeedAttention reports whether any defect bucket of the
// runs?view=team diagnostics is non-empty (contract C-ORGREAD §2.3).
func teamDiagnosticsNeedAttention(diagnostics teamDiagnostics) bool {
	return len(diagnostics.TerminalMissingRunIDs) > 0 ||
		len(diagnostics.TerminalProjectionBlockedRunIDs) > 0 ||
		len(diagnostics.TerminalAssociationDefectRunIDs) > 0 ||
		len(diagnostics.TerminalLineageDefectRunIDs) > 0 ||
		len(diagnostics.TerminalLivenessUnknownRunIDs) > 0 ||
		len(diagnostics.CorruptTerminalRunIDs) > 0
}
