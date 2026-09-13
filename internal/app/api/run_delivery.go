package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
	"github.com/labstack/echo/v4"
)

// The delivery summary projects saved checks; execution status and artifact
// list completeness never imply that the user's requirements were satisfied.
type runDeliverySummary struct {
	RevisionID           string                         `json:"revision_id,omitempty"`
	ContractDigest       string                         `json:"contract_digest,omitempty"`
	VerificationID       string                         `json:"verification_id,omitempty"`
	VerificationStatus   deliverable.VerificationStatus `json:"verification_status"`
	Reason               string                         `json:"reason,omitempty"`
	Checks               []runDeliveryCheck             `json:"checks"`
	CheckCounts          map[string]int                 `json:"check_counts"`
	Available            bool                           `json:"available"`
	EvidenceCompleteness string                         `json:"evidence_completeness"`
	InputRevisionKind    string                         `json:"input_revision_kind,omitempty"`
	ParentRunID          string                         `json:"parent_run_id,omitempty"`
	ParentMaterialCount  int                            `json:"parent_material_count,omitempty"`
}

type runDeliveryCheck struct {
	Title    string                         `json:"title,omitempty"`
	Actual   string                         `json:"actual,omitempty"`
	Expected string                         `json:"expected,omitempty"`
	CheckID  string                         `json:"check_id"`
	Status   deliverable.VerificationStatus `json:"status"`
	Reason   string                         `json:"reason"`
}

func (s *Server) runDelivery(ctx context.Context, run teamrun.TeamRun) runDeliverySummary {
	unknown := runDeliverySummary{VerificationStatus: deliverable.VerificationUnknown, Reason: "delivery_verification_unavailable", Checks: []runDeliveryCheck{}, CheckCounts: map[string]int{}, EvidenceCompleteness: "unavailable"}
	if s.Deliverables == nil {
		return unknown
	}
	state, err := s.Deliverables.GetDeliveryState(ctx, run.WorkspaceID, run.RunID)
	if errors.Is(err, deliverable.ErrNotFound) {
		unknown.Reason, unknown.EvidenceCompleteness = "delivery_contract_missing", "complete"
		return unknown
	}
	if err != nil {
		return unknown
	}
	if state.Binding.WorkspaceID != run.WorkspaceID || state.Binding.RunSnapshotID != run.RunSnapshotID || (state.Binding.RunID != "" && state.Binding.RunID != run.RunID) {
		unknown.Reason = "delivery_binding_mismatch"
		return unknown
	}
	result := projectRunDelivery(run.Status, state)
	s.attachDeliveryLineage(ctx, run.WorkspaceID, state.Binding.InputRevisionID, &result)
	return result
}

func (s *Server) attachDeliveryLineage(ctx context.Context, workspaceID, inputRevisionID string, result *runDeliverySummary) {
	if s.GetPool() == nil || inputRevisionID == "" {
		return
	}
	_ = s.GetPool().QueryRow(ctx, `SELECT revision_kind,COALESCE(parent_run_id,''),jsonb_array_length(parent_materials)
		FROM weave_dispatch_input_revisions WHERE workspace_id=$1 AND input_revision_id=$2`,
		workspaceID, inputRevisionID).Scan(&result.InputRevisionKind, &result.ParentRunID, &result.ParentMaterialCount)
}

func projectRunDelivery(status teamrun.Status, state deliverable.DeliveryState) runDeliverySummary {
	result := runDeliverySummary{RevisionID: state.RevisionID, ContractDigest: state.ContractDigest,
		VerificationID: state.VerificationID, VerificationStatus: deliverable.VerificationUnknown,
		Checks: []runDeliveryCheck{}, CheckCounts: map[string]int{}, EvidenceCompleteness: "complete", Reason: "delivery_report_missing"}
	if state.Report == nil {
		if status == teamrun.StatusQueued || status == teamrun.StatusRunning || status == teamrun.StatusParked {
			result.VerificationStatus, result.Reason = deliverable.VerificationPending, "awaiting_delivery"
		}
		return result
	}
	report := state.Report
	if report.RevisionID != state.RevisionID || report.ContractDigest != state.ContractDigest || report.ID != state.VerificationID {
		result.Reason, result.EvidenceCompleteness = "delivery_report_mismatch", "unavailable"
		return result
	}
	result.VerificationStatus, result.Reason, result.Available = report.Status, "", state.RevisionID != ""
	for _, check := range report.Checks {
		item := runDeliveryCheck{CheckID: check.CheckID, Title: check.Title, Status: check.Status, Reason: check.Reason}
		if check.VerifierID == "weave.deterministic" && check.VerifierVersion == "v1" {
			var evidence struct {
				Actual   string `json:"actual"`
				Expected string `json:"expected"`
			}
			if json.Unmarshal(check.Evidence, &evidence) == nil {
				item.Actual, item.Expected = evidence.Actual, evidence.Expected
			}
		}
		result.Checks = append(result.Checks, item)
		result.CheckCounts[string(check.Status)]++
	}
	return result
}

// The activity response contains bounded check summaries. This exact-run read
// returns the saved requirements, candidate manifest and verification evidence.
func (s *Server) handleGetRunDelivery(c echo.Context) error {
	if s.teamRunCancel == nil || s.teamRunCancel.Runs == nil || s.Deliverables == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "delivery_verification_unavailable"})
	}
	run, err := s.teamRunCancel.Runs.Get(c.Request().Context(), getTenant(c), c.Param("id"))
	if errors.Is(err, teamrun.ErrTeamRunIdentityMismatch) {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "run_not_found"})
	}
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "run_read_failed"})
	}
	state, err := s.Deliverables.GetDeliveryState(c.Request().Context(), getTenant(c), run.RunID)
	if errors.Is(err, deliverable.ErrNotFound) {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "delivery_contract_missing"})
	}
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "delivery_read_failed"})
	}
	if state.Binding.WorkspaceID != run.WorkspaceID || state.Binding.RunSnapshotID != run.RunSnapshotID || (state.Binding.RunID != "" && state.Binding.RunID != run.RunID) {
		return c.JSON(http.StatusConflict, map[string]string{"error": "delivery_binding_mismatch"})
	}
	delivery := projectRunDelivery(run.Status, state)
	s.attachDeliveryLineage(c.Request().Context(), run.WorkspaceID, state.Binding.InputRevisionID, &delivery)
	return c.JSON(http.StatusOK, map[string]any{"run_id": run.RunID, "status": run.Status, "delivery": delivery, "binding": state.Binding, "report": state.Report})
}

func (s *Server) handleRecheckRunDelivery(c echo.Context) error {
	if s.Deliverables == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "delivery_verification_unavailable"})
	}
	var body struct {
		RevisionID     string `json:"revision_id"`
		ContractDigest string `json:"contract_digest"`
	}
	if err := decodeOneJSON(c, &body, 2048); err != nil || body.RevisionID == "" || body.ContractDigest == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_request"})
	}
	result, err := s.Deliverables.RecheckCurrentDelivery(c.Request().Context(), deliverable.RecheckRequest{
		WorkspaceID: getTenant(c), RunID: c.Param("id"), RevisionID: body.RevisionID, ContractDigest: body.ContractDigest,
	})
	if errors.Is(err, deliverable.ErrNotFound) {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "delivery_not_found"})
	}
	if errors.Is(err, deliverable.ErrVerificationConflict) {
		return c.JSON(http.StatusConflict, map[string]string{"error": "delivery_revision_changed"})
	}
	if errors.Is(err, deliverable.ErrVerificationUnavailable) {
		return c.JSON(http.StatusUnprocessableEntity, map[string]string{"error": "delivery_recheck_unavailable"})
	}
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "delivery_recheck_failed"})
	}
	return c.JSON(http.StatusOK, result)
}

func (s *Server) handleGetRunVerification(c echo.Context) error {
	if s.Deliverables == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "delivery_verification_unavailable"})
	}
	report, err := s.Deliverables.GetVerificationReport(c.Request().Context(), getTenant(c), c.Param("id"), c.Param("verification_id"))
	if errors.Is(err, deliverable.ErrNotFound) {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "delivery_report_not_found"})
	}
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "delivery_read_failed"})
	}
	return c.JSON(http.StatusOK, report)
}
