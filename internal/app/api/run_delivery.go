package api

import (
	"context"
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
}

type runDeliveryCheck struct {
	CheckID string                         `json:"check_id"`
	Status  deliverable.VerificationStatus `json:"status"`
	Reason  string                         `json:"reason"`
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
	return projectRunDelivery(run.Status, state)
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
		result.Checks = append(result.Checks, runDeliveryCheck{CheckID: check.CheckID, Status: check.Status, Reason: check.Reason})
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
	return c.JSON(http.StatusOK, map[string]any{"run_id": run.RunID, "status": run.Status, "delivery": projectRunDelivery(run.Status, state), "binding": state.Binding, "report": state.Report})
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
