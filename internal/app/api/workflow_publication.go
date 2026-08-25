package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/labstack/echo/v4"
)

const (
	workflowAdmissionAdmitted      = "admitted"
	workflowAdmissionBlocked       = "blocked"
	workflowAdmissionGrandfathered = "grandfathered"
	workflowAdmissionUnknown       = "unknown"

	// workflowAdmissionReasonTightened marks a published version whose frozen
	// authorization no longer fits the live roster; the version stays startable
	// under grandfathering until explicitly blocked.
	workflowAdmissionReasonTightened = "authorization_tightened"
)

type archiveWorkflowResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type workflowDependencyItem struct {
	Kind  string `json:"kind"`
	Key   string `json:"key"`
	Owner string `json:"owner"`
	Hash  string `json:"hash"`
}

type workflowDependenciesResponse struct {
	WorkflowID   string                   `json:"workflow_id"`
	Version      int                      `json:"version"`
	ArtifactHash string                   `json:"artifact_hash"`
	Dependencies []workflowDependencyItem `json:"dependencies"`
}

type workflowAdmissionStatusResponse struct {
	Status    string     `json:"status"`
	Reasons   []string   `json:"reasons"`
	UpdatedAt *time.Time `json:"updated_at"`
}

// handleArchiveWorkflow retires one workflow (active→archived) while keeping
// every version and its history. Archiving is idempotent: an already archived
// workflow returns the same 200 response instead of a conflict.
func (s *Server) handleArchiveWorkflow(c echo.Context) error {
	if s.Workflow == nil {
		return workflowError(
			c,
			http.StatusServiceUnavailable,
			"workflow_store_unavailable",
			"workflow store unavailable",
		)
	}
	workflowID := c.Param("id")
	if err := s.Workflow.Archive(c.Request().Context(), getTenant(c), workflowID); err != nil {
		if errors.Is(err, workflow.ErrNotFound) {
			return workflowError(c, http.StatusNotFound, "workflow_not_found", "workflow not found")
		}
		return workflowStoreFailure(c, err)
	}
	return c.JSON(http.StatusOK, archiveWorkflowResponse{
		ID:     workflowID,
		Status: workflow.WorkflowStatusArchived,
	})
}

// handleGetWorkflowVersionDependencies returns one published version's frozen
// dependency index in canonical stable order. Draft versions have no artifact
// and are refused honestly with 404 artifact_not_found.
func (s *Server) handleGetWorkflowVersionDependencies(c echo.Context) error {
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
	workflowID := c.Param("id")
	if _, err := s.Workflow.GetVersion(ctx, workspaceID, workflowID, versionNumber); err != nil {
		if errors.Is(err, workflow.ErrNotFound) {
			return workflowError(c, http.StatusNotFound, "workflow_not_found", "workflow not found")
		}
		return workflowStoreFailure(c, err)
	}
	artifact, err := s.Workflow.GetArtifact(ctx, workspaceID, workflowID, versionNumber)
	if err != nil {
		if errors.Is(err, workflow.ErrNotFound) {
			return workflowError(c, http.StatusNotFound, "artifact_not_found", "artifact not found")
		}
		return workflowStoreFailure(c, err)
	}
	dependencies, err := s.Workflow.ListDependencies(ctx, workspaceID, workflowID, versionNumber)
	if err != nil {
		return workflowStoreFailure(c, err)
	}
	items := make([]workflowDependencyItem, 0, len(dependencies))
	for _, dependency := range dependencies {
		items = append(items, workflowDependencyItem{
			Kind:  dependency.DependencyType,
			Key:   dependency.DependencyKey,
			Owner: dependency.OwnerID,
			Hash:  dependency.ContentHash,
		})
	}
	return c.JSON(http.StatusOK, workflowDependenciesResponse{
		WorkflowID:   workflowID,
		Version:      versionNumber,
		ArtifactHash: artifact.ContentHash,
		Dependencies: items,
	})
}

// handleGetWorkflowVersionAdmission reports one version's current admission
// status: blocked (kill switch), grandfathered (frozen authorization tightened
// by the live roster but not blocked), admitted, or unknown (the version
// exists but was never evaluated, e.g. a draft).
func (s *Server) handleGetWorkflowVersionAdmission(c echo.Context) error {
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
	workflowID := c.Param("id")
	version, err := s.Workflow.GetVersion(ctx, workspaceID, workflowID, versionNumber)
	if err != nil {
		if errors.Is(err, workflow.ErrNotFound) {
			return workflowError(c, http.StatusNotFound, "workflow_not_found", "workflow not found")
		}
		return workflowStoreFailure(c, err)
	}

	response := workflowAdmissionStatusResponse{
		Status:  workflowAdmissionUnknown,
		Reasons: []string{},
	}
	view, err := s.Workflow.GetVersionAdmissionView(ctx, workspaceID, workflowID, versionNumber)
	if err != nil {
		if errors.Is(err, workflow.ErrNotFound) {
			return c.JSON(http.StatusOK, response)
		}
		return workflowStoreFailure(c, err)
	}
	updatedAt := view.LatestAuditAt
	if updatedAt == nil {
		updatedAt = version.PublishedAt
	}
	if updatedAt != nil {
		normalized := updatedAt.UTC()
		response.UpdatedAt = &normalized
	}
	switch {
	case view.Blocked:
		response.Status = workflowAdmissionBlocked
		if view.LatestBlockReason != nil {
			response.Reasons = []string{*view.LatestBlockReason}
		}
	case view.Tightened:
		response.Status = workflowAdmissionGrandfathered
		response.Reasons = []string{workflowAdmissionReasonTightened}
	default:
		response.Status = workflowAdmissionAdmitted
	}
	return c.JSON(http.StatusOK, response)
}
