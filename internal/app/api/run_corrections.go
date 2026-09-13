package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
	"github.com/labstack/echo/v4"
)

type requestRunCorrectionBody struct {
	TargetKind     string `json:"target_kind"`
	TargetMemberID string `json:"target_member_id,omitempty"`
	Instruction    string `json:"instruction"`
	IdempotencyKey string `json:"idempotency_key"`
}

type confirmRunCorrectionBody struct {
	Disposition    string `json:"disposition"`
	IdempotencyKey string `json:"idempotency_key"`
}

func decodeOneJSON(c echo.Context, target any, maxBytes int64) error {
	c.Request().Body = http.MaxBytesReader(c.Response(), c.Request().Body, maxBytes)
	decoder := json.NewDecoder(c.Request().Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request must contain one JSON object")
	}
	return nil
}

func (s *Server) handleRequestRunCorrection(c echo.Context) error {
	if s.teamRunCorrections == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "team_run_correction_unavailable"})
	}
	var body requestRunCorrectionBody
	if err := decodeOneJSON(c, &body, 20*1024); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_request"})
	}
	body.TargetKind = strings.TrimSpace(body.TargetKind)
	body.TargetMemberID = strings.TrimSpace(body.TargetMemberID)
	body.Instruction = strings.TrimSpace(body.Instruction)
	body.IdempotencyKey = strings.TrimSpace(body.IdempotencyKey)
	if body.TargetKind == "member" {
		if s.teamRunCancel == nil || s.teamRunCancel.Runs == nil || s.Workflow == nil {
			return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "correction_target_unavailable"})
		}
		run, err := s.teamRunCancel.Runs.Get(c.Request().Context(), getTenant(c), c.Param("id"))
		if errors.Is(err, teamrun.ErrTeamRunIdentityMismatch) {
			return c.JSON(http.StatusNotFound, map[string]string{"error": "run_not_found"})
		}
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "run_read_failed"})
		}
		artifact, err := s.Workflow.GetArtifact(c.Request().Context(), getTenant(c), run.WorkflowID, run.WorkflowVersion)
		if err != nil || artifact == nil {
			return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "correction_target_unavailable"})
		}
		payload, err := frozen.DecodeArtifactEnvelopeV1(frozen.ArtifactEnvelopeV1{
			WorkspaceID: artifact.WorkspaceID, WorkflowID: artifact.WorkflowID, WorkflowVersion: artifact.WorkflowVersion,
			ArtifactSchemaVersion: artifact.ArtifactSchemaVersion, CanonicalizationAlgorithm: artifact.CanonicalizationAlgorithm,
			CanonicalizationVersion: artifact.CanonicalizationVersion, HashAlgorithm: artifact.HashAlgorithm,
			ContentHash: artifact.ContentHash, Payload: artifact.Payload,
		})
		if err != nil {
			return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "correction_target_unavailable"})
		}
		graph, report := machine.DecodeGraphDefinitionV1(payload.GraphDefinition)
		if report != nil && len(report.Issues) > 0 {
			return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "correction_target_unavailable"})
		}
		members, _ := runActivityPublishedMembers(payload, graph, run.Status, nil)
		found := false
		for _, member := range members {
			if member.AgentID == body.TargetMemberID {
				found = true
				break
			}
		}
		if !found {
			return c.JSON(http.StatusUnprocessableEntity, map[string]string{"error": "correction_target_not_in_run"})
		}
	}
	item, err := s.teamRunCorrections.Request(c.Request().Context(), teamrun.RequestCorrectionRequest{
		WorkspaceID: getTenant(c), RunID: c.Param("id"), TargetKind: body.TargetKind,
		TargetMemberID: body.TargetMemberID, Instruction: body.Instruction,
		IdempotencyKey: body.IdempotencyKey, Actor: getUserID(c), OccurredAt: time.Now().UTC(),
	})
	if err != nil {
		switch {
		case errors.Is(err, teamrun.ErrTeamRunIdentityMismatch):
			return c.JSON(http.StatusNotFound, map[string]string{"error": "run_not_found"})
		case errors.Is(err, teamrun.ErrTeamRunStateConflict):
			return c.JSON(http.StatusConflict, map[string]string{"error": "correction_conflict"})
		default:
			return c.JSON(http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		}
	}
	return c.JSON(http.StatusAccepted, item)
}

func (s *Server) handleListRunCorrections(c echo.Context) error {
	if s.teamRunCorrections == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "team_run_correction_unavailable"})
	}
	items, err := s.teamRunCorrections.List(c.Request().Context(), getTenant(c), c.Param("id"), 20)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "correction_read_failed"})
	}
	return c.JSON(http.StatusOK, items)
}

func (s *Server) handleConfirmRunCorrection(c echo.Context) error {
	if s.teamRunCorrectionResume == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "team_run_correction_unavailable"})
	}
	var body confirmRunCorrectionBody
	if err := decodeOneJSON(c, &body, 4096); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_request"})
	}
	body.Disposition = strings.TrimSpace(body.Disposition)
	body.IdempotencyKey = strings.TrimSpace(body.IdempotencyKey)
	result, err := s.teamRunCorrectionResume.Confirm(c.Request().Context(), teamrun.ConfirmCorrectionRequest{
		WorkspaceID: getTenant(c), RunID: c.Param("id"), CorrectionID: c.Param("correction_id"),
		Disposition: body.Disposition, IdempotencyKey: body.IdempotencyKey,
		Actor: getUserID(c), OccurredAt: time.Now().UTC(),
	})
	if err != nil {
		switch {
		case errors.Is(err, teamrun.ErrTeamRunIdentityMismatch):
			return c.JSON(http.StatusNotFound, map[string]string{"error": "correction_not_found"})
		case errors.Is(err, teamrun.ErrTeamRunStateConflict), errors.Is(err, teamrun.ErrTeamRunResumeInvalid), errors.Is(err, teamrun.ErrTeamRunResumeStale):
			return c.JSON(http.StatusConflict, map[string]string{"error": "correction_confirmation_conflict"})
		default:
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
	}
	return c.JSON(http.StatusAccepted, map[string]any{"run_id": result.Run.RunID, "status": "queued", "task_id": result.TaskID, "idempotent": result.Idempotent})
}
