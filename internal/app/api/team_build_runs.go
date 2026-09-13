package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jinyitao123/weave/internal/app/teamconstruction"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/kernel/publication"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/labstack/echo/v4"
)

var teamBuildCandidateHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

var (
	errTeamBuildStoreUnavailable  = errors.New("team build store unavailable")
	errBuildRunNotActionable      = errors.New("build run is not actionable")
	errBuildRunWorkflowOutOfScope = errors.New("build run workflow is out of asset scope")
)

type candidateTestRunRequest struct {
	BuildRunID  string          `json:"build_run_id"`
	WorkflowID  string          `json:"workflow_id"`
	ContentHash string          `json:"content_hash"`
	Input       json.RawMessage `json:"input"`
}

type candidateTestRunResponse struct {
	RunID           string `json:"run_id"`
	WorkflowID      string `json:"workflow_id"`
	WorkflowVersion int    `json:"workflow_version"`
	TaskID          string `json:"task_id"`
	BuildRunID      string `json:"build_run_id"`
	ContentHash     string `json:"content_hash"`
}

type candidatePublishRequest struct {
	BuildRunID  string `json:"build_run_id"`
	WorkflowID  string `json:"workflow_id"`
	ContentHash string `json:"content_hash"`
}

type candidatePublishResponse struct {
	WorkflowID      string `json:"workflow_id"`
	WorkflowVersion int    `json:"workflow_version"`
	ContentHash     string `json:"content_hash"`
	BuildRunID      string `json:"build_run_id"`
	BuildRunStatus  string `json:"build_run_status"`
}

// handleCandidateTestRun starts one admin test run of a frozen candidate:
// the run snapshot records the build_run_id and content_hash, and the task
// queue carries the "api" source so the teamrun consumer and executor can
// resolve the candidate envelope instead of the published artifact.
func (s *Server) handleCandidateTestRun(c echo.Context) error {
	if s.Workflow == nil || s.TeamBuild == nil ||
		s.ScheduleTransactions == nil || s.ProductPublication == nil {
		return workflowError(
			c,
			http.StatusServiceUnavailable,
			"candidate_run_unavailable",
			"candidate test run service unavailable",
		)
	}
	var request candidateTestRunRequest
	if err := decodeWorkflowBody(c, &request); err != nil {
		return workflowSchemaError(c)
	}
	if err := validateCandidateTestRunRequest(request); err != nil {
		return workflowSchemaError(c)
	}
	workspaceID := getTenant(c)
	ctx := c.Request().Context()

	if _, err := s.authorizeTeamBuildRunWorkflow(
		ctx, workspaceID, request.BuildRunID, request.WorkflowID, false,
	); err != nil {
		return mapTeamBuildRunAuthorizationError(c, err)
	}

	payload := json.RawMessage(`{}`)
	if request.Input != nil {
		if !json.Valid(request.Input) {
			return workflowSchemaError(c)
		}
		payload = append(json.RawMessage(nil), request.Input...)
	}

	tx, err := s.ScheduleTransactions.Begin(ctx)
	if err != nil {
		return workflowStoreFailure(c, fmt.Errorf("begin candidate test run: %w", err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	candidate, err := s.WorkflowArtifacts.GetCandidate(ctx, workspaceID, request.WorkflowID, request.ContentHash)
	if err != nil {
		return mapCandidatePublishError(c, err)
	}
	if candidate.ContentHash != request.ContentHash {
		return workflowError(c, http.StatusConflict, "candidate_hash_mismatch", "candidate content hash does not match request")
	}
	_ = tx.Rollback(ctx)
	version, err := s.Workflow.GetVersion(ctx, workspaceID, candidate.WorkflowID, candidate.WorkflowVersion)
	if err != nil {
		return mapCandidatePublishError(c, err)
	}
	envelope, err := workflow.CandidateEnvelope(candidate)
	if err != nil {
		return workflowStoreFailure(c, err)
	}
	canonicalInput, err := frozen.CanonicalizeJSON(payload)
	if err != nil {
		return workflowSchemaError(c)
	}
	identity := sha256.Sum256(append([]byte(request.BuildRunID+"\x00"+request.WorkflowID+"\x00"+request.ContentHash+"\x00"), canonicalInput...))
	inputHash := sha256.Sum256(canonicalInput)
	requestID := "candidate-api:" + hex.EncodeToString(identity[:])
	record, err := s.ProductPublication.AdmitCandidate(ctx, teamconstruction.CandidateTarget{
		ExpectedAssetVersion: version.UpdatedAt.UTC().Format(time.RFC3339Nano),
		BuildRunID:           request.BuildRunID,
		RoundNo:              0,
		SourceRole:           teambuild.SourceRoleFixedWorkflowRoot,
	}, publication.CandidateRunRequest{
		Version:      publication.ContractVersion,
		RequestID:    requestID,
		Candidate:    envelope,
		Input:        payload,
		InputVersion: "sha256:" + hex.EncodeToString(inputHash[:]),
		SourceRef:    request.BuildRunID,
		Purpose:      "admin-candidate-test",
	})
	if err != nil {
		return mapWorkflowPublishError(c, err)
	}
	if record.Receipt == nil {
		return workflowStoreFailure(c, errors.New("candidate admission receipt unavailable"))
	}
	return c.JSON(http.StatusCreated, candidateTestRunResponse{
		RunID:           record.Receipt.RunID,
		WorkflowID:      record.Receipt.Revision.WorkflowID,
		WorkflowVersion: record.Receipt.Revision.WorkflowVersion,
		TaskID:          record.Receipt.TaskID,
		BuildRunID:      request.BuildRunID,
		ContentHash:     request.ContentHash,
	})
}

// handleCandidatePublish releases the exact frozen candidate the admin test
// run consumed: read candidate by content hash, verify the hash, rebuild and
// insert the publication (with the updated_at CAS), then mark the TeamBuildRun
// publishing -> passed with the final publication reference.
func (s *Server) handleCandidatePublish(c echo.Context) error {
	if s.Workflow == nil || s.TeamBuild == nil || s.ScheduleTransactions == nil || s.ProductPublication == nil {
		return workflowError(
			c,
			http.StatusServiceUnavailable,
			"candidate_publish_unavailable",
			"candidate publish service unavailable",
		)
	}
	var request candidatePublishRequest
	if err := decodeWorkflowBody(c, &request); err != nil {
		return workflowSchemaError(c)
	}
	if err := validateCandidatePublishRequest(request); err != nil {
		return workflowSchemaError(c)
	}
	workspaceID := getTenant(c)
	ctx := c.Request().Context()

	if _, err := s.authorizeTeamBuildRunWorkflow(
		ctx, workspaceID, request.BuildRunID, request.WorkflowID, true,
	); err != nil {
		return mapTeamBuildRunAuthorizationError(c, err)
	}

	tx, err := s.ScheduleTransactions.Begin(ctx)
	if err != nil {
		return workflowStoreFailure(c, fmt.Errorf("begin candidate publish: %w", err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	candidate, err := s.WorkflowArtifacts.GetCandidate(
		ctx, workspaceID, request.WorkflowID, request.ContentHash,
	)
	if err != nil {
		return mapCandidatePublishError(c, err)
	}
	if candidate.ContentHash != request.ContentHash {
		return workflowError(
			c,
			http.StatusConflict,
			"candidate_hash_mismatch",
			"candidate content hash does not match request",
		)
	}
	_ = tx.Rollback(ctx)
	version, err := s.Workflow.GetVersion(ctx, workspaceID, candidate.WorkflowID, candidate.WorkflowVersion)
	if err != nil {
		return mapCandidatePublishError(c, err)
	}
	requestID := "team-build-publish:" + request.BuildRunID + ":" + request.ContentHash
	command, err := teamconstruction.PublicationCommandForCandidate(requestID, candidate, teamconstruction.PublicationTarget{
		BuildRunID:           request.BuildRunID,
		TeamID:               candidate.Payload.Team.TeamID,
		ExpectedAssetVersion: version.UpdatedAt.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		var coded interface{ Code() string }
		if errors.As(err, &coded) && coded.Code() != "" {
			return workflowError(c, http.StatusUnprocessableEntity, coded.Code(), coded.Code())
		}
		return workflowStoreFailure(c, err)
	}
	record, err := s.ProductPublication.Publish(ctx, command)
	if err != nil {
		return mapWorkflowPublishError(c, err)
	}
	if record.Receipt == nil {
		return workflowStoreFailure(c, errors.New("candidate publication receipt unavailable"))
	}
	updated, err := s.TeamBuild.GetBuildRun(ctx, workspaceID, request.BuildRunID)
	if err != nil {
		return workflowError(
			c,
			http.StatusConflict,
			"build_run_publish_finalize_failed",
			"build run could not be finalized as passed",
		)
	}
	return c.JSON(http.StatusOK, candidatePublishResponse{
		WorkflowID:      record.Receipt.Revision.WorkflowID,
		WorkflowVersion: record.Receipt.Revision.WorkflowVersion,
		ContentHash:     record.Receipt.Revision.ContentHash,
		BuildRunID:      request.BuildRunID,
		BuildRunStatus:  updated.Status,
	})
}

// authorizeTeamBuildRunWorkflow is the API-layer TeamBuildRun gate modeled on
// the writegate receipt check: the run must be live and the workflow must sit
// inside the frozen asset scope. The publish entry additionally requires the
// run to already be in the publishing state, because MarkPublished finalizes
// exactly that transition.
func (s *Server) authorizeTeamBuildRunWorkflow(
	ctx context.Context,
	workspaceID, buildRunID, workflowID string,
	requirePublishing bool,
) (teambuild.TeamBuildRun, error) {
	if s.TeamBuild == nil {
		return teambuild.TeamBuildRun{}, errTeamBuildStoreUnavailable
	}
	run, err := s.TeamBuild.GetBuildRun(ctx, workspaceID, buildRunID)
	if err != nil {
		return teambuild.TeamBuildRun{}, err
	}
	if requirePublishing {
		if run.Status != teambuild.StatusPublishing {
			return teambuild.TeamBuildRun{}, errBuildRunNotActionable
		}
	} else {
		switch run.Status {
		case teambuild.StatusAuthorized,
			teambuild.StatusRoundRunning,
			teambuild.StatusPublishing:
		default:
			return teambuild.TeamBuildRun{}, errBuildRunNotActionable
		}
	}
	if !run.AssetScope.Contains(teambuild.AssetRef{Kind: "workflow", ID: workflowID}) {
		return teambuild.TeamBuildRun{}, errBuildRunWorkflowOutOfScope
	}
	return run, nil
}

func validateCandidateTestRunRequest(request candidateTestRunRequest) error {
	for name, value := range map[string]string{
		"build_run_id": request.BuildRunID,
		"workflow_id":  request.WorkflowID,
		"content_hash": request.ContentHash,
	} {
		if value == "" || value != strings.TrimSpace(value) {
			return errors.New(name + " is required")
		}
	}
	if !teamBuildCandidateHashPattern.MatchString(request.ContentHash) {
		return errors.New("content_hash is invalid")
	}
	if request.Input != nil && !json.Valid(request.Input) {
		return errors.New("input is invalid")
	}
	return nil
}

func validateCandidatePublishRequest(request candidatePublishRequest) error {
	for name, value := range map[string]string{
		"build_run_id": request.BuildRunID,
		"workflow_id":  request.WorkflowID,
		"content_hash": request.ContentHash,
	} {
		if value == "" || value != strings.TrimSpace(value) {
			return errors.New(name + " is required")
		}
	}
	if !teamBuildCandidateHashPattern.MatchString(request.ContentHash) {
		return errors.New("content_hash is invalid")
	}
	return nil
}

func (s *Server) respondCandidateRunAdmissionError(
	c echo.Context,
	tx interface {
		Rollback(context.Context) error
	},
	workflowID string,
	err error,
) error {
	ctx := c.Request().Context()
	var denial *workflow.FixedWorkflowAdmissionDenial
	if errors.As(err, &denial) {
		if rollbackErr := tx.Rollback(ctx); rollbackErr != nil {
			return workflowStoreFailure(c, errors.Join(err, rollbackErr))
		}
		record, auditErr := s.WorkflowArtifacts.RecordFixedWorkflowAdmissionDenial(
			context.WithoutCancel(ctx),
			workflow.FixedWorkflowAdmissionDenialAttempt{
				WorkspaceID:         getTenant(c),
				WorkflowID:          workflowID,
				WorkflowVersion:     denial.WorkflowVersion,
				TriggerType:         "api",
				AdmissionAttemptKey: uuid.NewString(),
				ReasonCode:          denial.ReasonCode,
			},
		)
		if auditErr != nil {
			return workflowStoreFailure(c, errors.Join(err, auditErr))
		}
		return workflowError(
			c,
			http.StatusConflict,
			string(record.ReasonCode),
			"candidate run admission denied",
		)
	}

	switch {
	case errors.Is(err, workflow.ErrCandidateNotFound):
		return workflowError(c, http.StatusNotFound, "candidate_not_found", "candidate not found")
	case errors.Is(err, workflow.ErrNotFound):
		return workflowError(c, http.StatusNotFound, "workflow_not_found", "workflow not found")
	case errors.Is(err, workflow.ErrArchived):
		return workflowError(c, http.StatusConflict, "workflow_archived", "workflow archived")
	case errors.Is(err, workflow.ErrWorkflowScheduleAdmissionDenied):
		return workflowError(
			c,
			http.StatusUnprocessableEntity,
			"workflow_not_runnable",
			"workflow is not runnable",
		)
	default:
		return workflowStoreFailure(c, err)
	}
}

func mapTeamBuildRunAuthorizationError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, teambuild.ErrBuildRunNotFound):
		return workflowError(c, http.StatusNotFound, "build_run_not_found", "build run not found")
	case errors.Is(err, errBuildRunNotActionable):
		return workflowError(
			c,
			http.StatusConflict,
			"build_run_not_actionable",
			"build run is not actionable",
		)
	case errors.Is(err, errBuildRunWorkflowOutOfScope):
		return workflowError(
			c,
			http.StatusForbidden,
			"build_run_workflow_out_of_scope",
			"workflow is outside the build run asset scope",
		)
	case errors.Is(err, errTeamBuildStoreUnavailable):
		return workflowError(
			c,
			http.StatusServiceUnavailable,
			"team_build_unavailable",
			"team build store unavailable",
		)
	default:
		return workflowStoreFailure(c, err)
	}
}

func mapCandidatePublishError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, workflow.ErrCandidateNotFound):
		return workflowError(c, http.StatusNotFound, "candidate_not_found", "candidate not found")
	case errors.Is(err, workflow.ErrCandidateInvalid):
		return workflowError(
			c,
			http.StatusUnprocessableEntity,
			"candidate_invalid",
			"candidate is invalid",
		)
	default:
		return workflowStoreFailure(c, err)
	}
}
