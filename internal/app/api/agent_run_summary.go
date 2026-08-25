package api

import (
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	"github.com/labstack/echo/v4"
)

// agentRunSummaryWindow is the fixed recent-terminal window (ticket C-BE-6:
// initial N=20 newest terminal runs) reported back as the response `window`
// field.
const agentRunSummaryWindow = 20

type agentRunSummaryLatestRun struct {
	RunID          string `json:"run_id"`
	Classification string `json:"classification"`
	StartedAt      string `json:"started_at"`
}

type agentRunSummaryResponse struct {
	Agent              string                    `json:"agent"`
	LatestRun          *agentRunSummaryLatestRun `json:"latest_run"`
	ActiveRunCount     int                       `json:"active_run_count"`
	RecentFailureCount int                       `json:"recent_failure_count"`
	Window             int                       `json:"window"`
}

// handleGetAgentRunSummary serves GET /v1/agents/:name/run-summary (contract
// C-CONSOLE §4, ticket C-BE-6). Run facts come from the PGRunLifecycleReader
// family with the registry's agent attribution; an agent without runs is a
// legal fact (latest_run=null, zero counts), not a 404.
func (s *Server) handleGetAgentRunSummary(c echo.Context) error {
	ctx := c.Request().Context()
	workspaceID := getTenant(c)
	name := c.Param("name")
	if s.Registry == nil || s.AgentRunReader == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "org read store unavailable"})
	}
	if _, err := s.Registry.Get(ctx, workspaceID, name); err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
	}
	result, err := s.AgentRunReader.ReadByAgent(ctx, loomruntime.RunLifecycleAgentQuery{
		WorkspaceID: workspaceID,
		Agent:       name,
	})
	if err != nil {
		var readErr *loomruntime.RunLifecycleReadError
		if errors.As(err, &readErr) && readErr.Retryable {
			return c.JSON(http.StatusServiceUnavailable, struct {
				Error     string `json:"error"`
				Retryable bool   `json:"retryable"`
			}{
				Error:     errTeamReadStoreUnavailable.Error(),
				Retryable: true,
			})
		}
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "run_summary_unavailable"})
	}
	response := summarizeAgentRuns(result.Runs)
	response.Agent = name
	return c.JSON(http.StatusOK, response)
}

// summarizeAgentRuns folds one agent's lifecycle items into the C-BE-6
// summary: latest_run is the run with the most recent started_at (run_id
// tiebreak); active_run_count buckets pending_active + reconciling;
// recent_failure_count scans the newest agentRunSummaryWindow valid-final
// (terminal_complete) runs and counts terminal markers with status failed.
func summarizeAgentRuns(runs []loomruntime.RunLifecycleItem) agentRunSummaryResponse {
	response := agentRunSummaryResponse{Window: agentRunSummaryWindow}
	terminals := make([]loomruntime.RunLifecycleItem, 0, len(runs))
	latestIndex := -1
	for index := range runs {
		item := runs[index]
		switch item.Classification {
		case loomruntime.RunLifecycleTerminalPendingActive,
			loomruntime.RunLifecycleTerminalReconciling:
			response.ActiveRunCount++
		case loomruntime.RunLifecycleTerminalComplete:
			terminals = append(terminals, item)
		}
		if latestIndex < 0 || agentRunStartedAfter(item, runs[latestIndex]) {
			latestIndex = index
		}
	}
	if latestIndex >= 0 {
		latest := runs[latestIndex]
		response.LatestRun = &agentRunSummaryLatestRun{
			RunID:          latest.Expected.RunID,
			Classification: string(latest.Classification),
			StartedAt:      agentRunStartedAt(latest),
		}
	}
	sort.Slice(terminals, func(left, right int) bool {
		return agentRunStartedAfter(terminals[left], terminals[right])
	})
	for index, item := range terminals {
		if index >= agentRunSummaryWindow {
			break
		}
		if item.MarkerStatus != nil &&
			*item.MarkerStatus == loomruntime.TerminalMarkerStatusFailed {
			response.RecentFailureCount++
		}
	}
	return response
}

func agentRunStartedAt(item loomruntime.RunLifecycleItem) string {
	if item.RunStartedAt == nil {
		return ""
	}
	return *item.RunStartedAt
}

// agentRunStartedAfter orders runs newest-first: later started_at wins, then
// the larger run_id; runs without a started_at fact sort last.
func agentRunStartedAfter(left, right loomruntime.RunLifecycleItem) bool {
	leftStarted, rightStarted := agentRunStartTime(left), agentRunStartTime(right)
	if !leftStarted.Equal(rightStarted) {
		return leftStarted.After(rightStarted)
	}
	return left.Expected.RunID > right.Expected.RunID
}

func agentRunStartTime(item loomruntime.RunLifecycleItem) time.Time {
	if item.RunStartedAt == nil {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339Nano, *item.RunStartedAt)
	if err != nil {
		return time.Time{}
	}
	return parsed
}
