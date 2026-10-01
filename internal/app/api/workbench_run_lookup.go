package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
	"github.com/labstack/echo/v4"
)

// workbenchRunLookupLimit bounds one lookup (contract work-run-lookup).
const workbenchRunLookupLimit = 100

type workbenchRunLookupRequest struct {
	Version string   `json:"version"`
	RunIDs  []string `json:"runIds"`
}

type workbenchRunLookupCounts struct {
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Unknown   int `json:"unknown"`
}

type workbenchRunLookupItem struct {
	RunID              string                   `json:"runId"`
	InputRevisionID    string                   `json:"inputRevisionId"`
	WorkbenchSessionID string                   `json:"workbenchSessionId"`
	Status             string                   `json:"status"`
	BusinessResult     string                   `json:"businessResult,omitempty"`
	IsCurrent          bool                     `json:"isCurrent"`
	ActionCounts       workbenchRunLookupCounts `json:"actionCounts"`
}

type workbenchRunLookupResponse struct {
	Version string                   `json:"version"`
	Runs    []workbenchRunLookupItem `json:"runs"`
	Missing []string                 `json:"missing"`
}

// handleLookupWorkbenchRuns answers, for the caller's own Workbench runs,
// whether each run's input is still the session's current input and how the
// run ended for the business (decision 002). The desktop maps these values
// and no longer derives supersession or results from message order.
func (s *Server) handleLookupWorkbenchRuns(c echo.Context) error {
	workspaceID, userID := getTenant(c), getUserID(c)
	if workspaceID == "" || userID == "" {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "workbench_context_identity_required"})
	}
	var request workbenchRunLookupRequest
	decoder := json.NewDecoder(io.LimitReader(c.Request().Body, 256<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || request.Version != "1" ||
		len(request.RunIDs) == 0 || len(request.RunIDs) > workbenchRunLookupLimit {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "workbench_run_lookup_invalid"})
	}
	seen := make(map[string]struct{}, len(request.RunIDs))
	for _, runID := range request.RunIDs {
		if runID == "" || len(runID) > 512 || strings.TrimSpace(runID) != runID {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "workbench_run_lookup_invalid"})
		}
		if _, duplicate := seen[runID]; duplicate {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "workbench_run_lookup_invalid"})
		}
		seen[runID] = struct{}{}
	}
	if s.GetPool() == nil || s.teamRunActivities == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "workbench_context_unavailable"})
	}
	response := workbenchRunLookupResponse{Version: "1", Runs: []workbenchRunLookupItem{}, Missing: []string{}}
	for _, runID := range request.RunIDs {
		item, found, err := s.lookupWorkbenchRun(c.Request().Context(), workspaceID, userID, runID)
		if err != nil {
			// A partial answer would read as "no actions" for the failed runs.
			return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "workbench_context_unavailable"})
		}
		if !found {
			response.Missing = append(response.Missing, runID)
			continue
		}
		response.Runs = append(response.Runs, item)
	}
	return c.JSON(http.StatusOK, response)
}

func (s *Server) lookupWorkbenchRun(ctx context.Context, workspaceID, userID, runID string) (workbenchRunLookupItem, bool, error) {
	run, owned, workbenchBound, err := s.workbenchRunAccess(ctx, workspaceID, userID, runID)
	if err != nil {
		return workbenchRunLookupItem{}, false, err
	}
	if !workbenchBound || !owned || run.RunID != runID {
		return workbenchRunLookupItem{}, false, nil
	}
	item := workbenchRunLookupItem{RunID: runID, Status: run.Status}
	err = s.GetPool().QueryRow(ctx, `SELECT input_revision_id,workbench_session_id,is_current
		FROM weave_dispatch_input_revisions
		WHERE workspace_id=$1 AND user_id=$2 AND consumed_run_id=$3 AND project_id=$4`,
		workspaceID, userID, runID, workbenchProjectID(userID)).Scan(&item.InputRevisionID, &item.WorkbenchSessionID, &item.IsCurrent)
	if errors.Is(err, pgx.ErrNoRows) {
		return workbenchRunLookupItem{}, false, nil
	}
	if err != nil {
		return workbenchRunLookupItem{}, false, err
	}
	finalResult, err := s.readWorkbenchFinalResult(ctx, workspaceID, userID, runID)
	if err != nil {
		return workbenchRunLookupItem{}, false, err
	}
	events, err := s.teamRunActivities.ListBusinessActionEvents(ctx, workspaceID, runID)
	if err != nil {
		return workbenchRunLookupItem{}, false, err
	}
	counts := teamrun.CountBusinessActions(events)
	item.ActionCounts = workbenchRunLookupCounts{Succeeded: counts.Succeeded, Failed: counts.Failed, Unknown: counts.Unknown}
	disposition := ""
	if finalResult != nil {
		disposition = finalResult.Disposition
	}
	item.BusinessResult = string(teamrun.ClassifyRunBusinessResult(run.Status, disposition, counts))
	return item, true, nil
}
