package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

// runGraphView is the flow a run executed, exactly as it was frozen: the
// published artifact for a published run, the candidate for a trial. The
// envelope is decoded with its content hash checked, so the graph returned is
// the one the hash names.
type runGraphView struct {
	RunID            string          `json:"run_id"`
	WorkflowID       string          `json:"workflow_id"`
	WorkflowVersion  int             `json:"workflow_version"`
	ContentHash      string          `json:"content_hash"`
	DevelopmentTrial bool            `json:"development_trial"`
	GraphDefinition  json.RawMessage `json:"graph_definition"`
}

func (s *Server) handleGetRunGraph(c echo.Context) error {
	ctx, workspace, runID := c.Request().Context(), getTenant(c), c.Param("id")
	_, owned, workbenchBound, accessErr := s.workbenchRunAccess(ctx, workspace, getUserID(c), runID)
	if accessErr != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "run_read_unavailable"})
	}
	if workbenchBound && !owned {
		if !adminRunReader(c) {
			return c.JSON(http.StatusNotFound, map[string]string{"error": "run_not_found"})
		}
		s.auditRunView(c, runID, "graph")
	}
	if s.teamRunCancel == nil || s.teamRunCancel.Runs == nil || s.GetPool() == nil || s.WorkflowArtifacts == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "run_read_unavailable"})
	}
	run, err := s.teamRunCancel.Runs.Get(ctx, workspace, runID)
	if errors.Is(err, teamrun.ErrTeamRunIdentityMismatch) {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "run_not_found"})
	}
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "run_read_unavailable"})
	}
	if run.SourceKind == teamrun.SourceAPI {
		_, trial, trialErr := developmentTrialForRun(ctx, s.GetPool(), workspace, runID)
		if trialErr != nil {
			return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "run_read_unavailable"})
		}
		if trial && !callerAtLeast(c, roleDeveloper) {
			return c.JSON(http.StatusForbidden, map[string]string{"error": "insufficient permissions"})
		}
	}
	// Run inspection has been authorized above. Read only its frozen graph
	// reference, scoped to the workspace and the run's own snapshot. The
	// execution snapshot reader's actor filter stays reserved for execution;
	// an authorized reviewer does not become the original execution subject.
	view := runGraphView{RunID: runID}
	var candidateHash string
	err = s.GetPool().QueryRow(ctx, `SELECT COALESCE(artifact_workflow_id,''),
		COALESCE(artifact_workflow_version,0), COALESCE(candidate_content_hash,'')
		FROM weave_team_run_snapshots WHERE workspace_id=$1 AND run_id=$2`, workspace, run.RunSnapshotID).
		Scan(&view.WorkflowID, &view.WorkflowVersion, &candidateHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "run_graph_not_found"})
	}
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "run_read_unavailable"})
	}
	view.DevelopmentTrial = candidateHash != ""
	if view.WorkflowID == "" || view.WorkflowVersion < 1 {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "run_graph_not_found"})
	}
	var artifact *workflow.PublishedArtifactContent
	if view.DevelopmentTrial {
		if !callerAtLeast(c, roleDeveloper) {
			return c.JSON(http.StatusForbidden, map[string]string{"error": "insufficient permissions"})
		}
		artifact, err = s.WorkflowArtifacts.GetCandidateArtifact(ctx, workspace, view.WorkflowID, view.WorkflowVersion, candidateHash)
	} else {
		artifact, err = s.WorkflowArtifacts.GetArtifact(ctx, workspace, view.WorkflowID, view.WorkflowVersion)
	}
	if errors.Is(err, workflow.ErrNotFound) || errors.Is(err, workflow.ErrCandidateNotFound) {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "run_graph_not_found"})
	}
	if errors.Is(err, workflow.ErrCandidateInvalid) {
		return c.JSON(http.StatusConflict, map[string]string{"error": "run_graph_corrupt"})
	}
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "run_read_unavailable"})
	}
	payload, err := frozen.DecodeArtifactEnvelopeV1(frozen.ArtifactEnvelopeV1{
		WorkspaceID: artifact.WorkspaceID, WorkflowID: artifact.WorkflowID, WorkflowVersion: artifact.WorkflowVersion,
		ArtifactSchemaVersion: artifact.ArtifactSchemaVersion, CanonicalizationAlgorithm: artifact.CanonicalizationAlgorithm,
		CanonicalizationVersion: artifact.CanonicalizationVersion, HashAlgorithm: artifact.HashAlgorithm,
		ContentHash: artifact.ContentHash, Payload: artifact.Payload,
	})
	if err != nil {
		return c.JSON(http.StatusConflict, map[string]string{"error": "run_graph_corrupt"})
	}
	view.ContentHash, view.GraphDefinition = artifact.ContentHash, payload.GraphDefinition
	return c.JSON(http.StatusOK, view)
}
