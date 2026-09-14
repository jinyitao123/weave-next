package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/app/teamconstruction"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/admissionfence"
	"github.com/jinyitao123/weave/internal/kernel/publication"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
	"github.com/labstack/echo/v4"
)

type createWorkflowRequest struct {
	Name            string          `json:"name"`
	Description     string          `json:"description"`
	TriggerConfig   json.RawMessage `json:"trigger_config"`
	GraphDefinition json.RawMessage `json:"graph_definition"`
}

type createWorkflowResponse struct {
	Workflow workflow.TeamWorkflow        `json:"workflow"`
	Draft    workflow.TeamWorkflowVersion `json:"draft"`
}

type updateWorkflowDraftRequest struct {
	ExpectedUpdatedAt time.Time       `json:"expected_updated_at"`
	TriggerConfig     json.RawMessage `json:"trigger_config"`
	GraphDefinition   json.RawMessage `json:"graph_definition"`
}

func (s *Server) handleCreateWorkflow(c echo.Context) error {
	if s.Workflow == nil || s.OrgStore == nil {
		return workflowError(
			c,
			http.StatusServiceUnavailable,
			"workflow_store_unavailable",
			"workflow store unavailable",
		)
	}

	var request createWorkflowRequest
	if err := decodeWorkflowBody(c, &request); err != nil ||
		strings.TrimSpace(request.Name) == "" ||
		!validWorkflowSchemaObject(request.TriggerConfig) ||
		!validWorkflowSchemaObject(request.GraphDefinition) {
		return workflowSchemaError(c)
	}

	workspaceID := getTenant(c)
	teamID := c.Param("id")
	teams, err := s.OrgStore.ListTeams(c.Request().Context(), workspaceID)
	if err != nil {
		return workflowStoreFailure(c, err)
	}
	found := false
	for _, team := range teams {
		if team.ID == teamID {
			found = true
			break
		}
	}
	if !found {
		return workflowError(c, http.StatusNotFound, "team_not_found", "team not found")
	}

	record := &workflow.TeamWorkflow{
		WorkspaceID: workspaceID,
		ID:          uuid.NewString(),
		TeamID:      teamID,
		Name:        strings.TrimSpace(request.Name),
		Description: request.Description,
	}
	draft, err := s.Workflow.Create(c.Request().Context(), record, workflow.DraftInput{
		TriggerConfig:   request.TriggerConfig,
		GraphDefinition: request.GraphDefinition,
		CreatedBy:       getUserID(c),
	})
	if err != nil {
		return s.mapWorkflowWriteError(c, err)
	}
	return c.JSON(http.StatusCreated, createWorkflowResponse{
		Workflow: *record,
		Draft:    *draft,
	})
}

func (s *Server) handleCreateWorkflowDraft(c echo.Context) error {
	if s.Workflow == nil {
		return workflowError(
			c,
			http.StatusServiceUnavailable,
			"workflow_store_unavailable",
			"workflow store unavailable",
		)
	}

	if err := decodeEmptyWorkflowBody(c); err != nil {
		return workflowSchemaError(c)
	}

	version, err := s.Workflow.CreateDraft(
		c.Request().Context(),
		getTenant(c),
		c.Param("id"),
		getUserID(c),
	)
	if err != nil {
		return s.mapWorkflowWriteError(c, err)
	}
	return c.JSON(http.StatusCreated, version)
}

func (s *Server) handleUpdateWorkflowDraft(c echo.Context) error {
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
	var request updateWorkflowDraftRequest
	if err := decodeWorkflowBody(c, &request); err != nil ||
		request.ExpectedUpdatedAt.IsZero() ||
		!validWorkflowSchemaObject(request.TriggerConfig) ||
		!validWorkflowSchemaObject(request.GraphDefinition) {
		return workflowSchemaError(c)
	}

	version, err := s.Workflow.UpdateDraft(
		c.Request().Context(),
		getTenant(c),
		c.Param("id"),
		versionNumber,
		request.ExpectedUpdatedAt,
		workflow.DraftInput{
			TriggerConfig:   request.TriggerConfig,
			GraphDefinition: request.GraphDefinition,
		},
	)
	if err != nil {
		return s.mapWorkflowWriteError(c, err)
	}
	return c.JSON(http.StatusOK, version)
}

// workflowPublicationPreflight carries the shared publish/validate preflight
// results: the resolved draft version, the built publication candidate, the
// validation report, and the still-open transaction. The caller owns the
// transaction — publish commits it, the validate dry run rolls it back.
type workflowPublicationPreflight struct {
	version   *workflow.TeamWorkflowVersion
	candidate *workflow.PublicationCandidate
	report    *machine.Report
	tx        pgx.Tx
}

// runWorkflowPublicationPreflight executes the shared candidate build +
// machine validation pipeline used by publish and the validate dry run:
// resolve the exact version, reject non-empty bodies, enforce draft status,
// then build the publication candidate and run machine.Validate inside one
// transaction. On failure it writes the error response and returns nil with
// the response write result. On success the transaction is still open; the
// caller must commit (publish) or roll back (validate).
func (s *Server) runWorkflowPublicationPreflight(
	c echo.Context,
	versionNumber int,
) (*workflowPublicationPreflight, error) {
	version, err := s.Workflow.GetVersion(
		c.Request().Context(),
		getTenant(c),
		c.Param("id"),
		versionNumber,
	)
	if err != nil {
		if errors.Is(err, workflow.ErrNotFound) {
			return nil, workflowError(
				c,
				http.StatusNotFound,
				"workflow_not_found",
				"workflow not found",
			)
		}
		return nil, workflowStoreFailure(c, err)
	}
	if err := decodeEmptyWorkflowBody(c); err != nil {
		return nil, workflowSchemaError(c)
	}
	if version.Status != workflow.VersionStatusDraft {
		return nil, workflowError(
			c,
			http.StatusConflict,
			"workflow_version_conflict",
			"workflow version conflict",
		)
	}
	ctx := c.Request().Context()
	tx, err := s.ScheduleTransactions.Begin(ctx)
	if err != nil {
		return nil, workflowStoreFailure(c, err)
	}
	if s.PublicationAuthority == nil {
		_ = tx.Rollback(ctx)
		return nil, workflowError(c, http.StatusServiceUnavailable, "workflow_store_unavailable", "workflow publication service unavailable")
	}
	candidate, report, err := s.PublicationAuthority.BuildCandidateTx(ctx, tx, workflow.CandidateInput{
		WorkspaceID:     getTenant(c),
		WorkflowID:      c.Param("id"),
		WorkflowVersion: versionNumber,
	})
	if err != nil {
		_ = tx.Rollback(ctx)
		return nil, mapWorkflowPublishError(c, err)
	}
	return &workflowPublicationPreflight{
		version:   version,
		candidate: candidate,
		report:    report,
		tx:        tx,
	}, nil
}

// workflowValidateResponse is the validate dry-run payload. Issues carries the
// exact machine.ValidationIssue slice produced by the shared preflight, so the
// array serializes byte-identically to the publish 422 issues.
type workflowValidateResponse struct {
	Valid  bool                      `json:"valid"`
	Issues []machine.ValidationIssue `json:"issues"`
}

func (s *Server) handleValidateWorkflowVersion(c echo.Context) error {
	if s.Workflow == nil ||
		s.Registry == nil ||
		s.DeliveryTargets == nil ||
		s.Credentials == nil ||
		s.AgentSchedules == nil ||
		s.Descriptors == nil ||
		s.Skills == nil ||
		s.ScheduleTransactions == nil {
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
	preflight, err := s.runWorkflowPublicationPreflight(c, versionNumber)
	if preflight == nil {
		return err
	}
	// Dry run: the transaction only performed reads and locks; never commit.
	_ = preflight.tx.Rollback(c.Request().Context())
	issues := []machine.ValidationIssue{}
	if preflight.report != nil && len(preflight.report.Issues) != 0 {
		issues = preflight.report.Issues
	}
	return c.JSON(http.StatusOK, workflowValidateResponse{
		Valid:  len(issues) == 0,
		Issues: issues,
	})
}

func (s *Server) handlePublishWorkflowVersion(c echo.Context) error {
	if s.Workflow == nil || s.OrgStore == nil || s.ProductPublication == nil {
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
	if s.Registry == nil ||
		s.DeliveryTargets == nil ||
		s.Credentials == nil ||
		s.AgentSchedules == nil ||
		s.Descriptors == nil ||
		s.Skills == nil ||
		s.ScheduleTransactions == nil {
		return workflowError(
			c,
			http.StatusServiceUnavailable,
			"workflow_store_unavailable",
			"workflow store unavailable",
		)
	}

	preflight, err := s.runWorkflowPublicationPreflight(c, versionNumber)
	if preflight == nil {
		return err
	}
	ctx := c.Request().Context()
	tx := preflight.tx
	defer func() { _ = tx.Rollback(ctx) }()

	candidate := preflight.candidate
	if preflight.report != nil && len(preflight.report.Issues) != 0 {
		return c.JSON(http.StatusUnprocessableEntity, map[string]any{
			"error":  "workflow candidate invalid",
			"code":   "workflow_candidate_invalid",
			"issues": preflight.report.Issues,
		})
	}
	if candidate == nil || !candidate.ExpectedUpdatedAt.Equal(preflight.version.UpdatedAt) {
		return mapWorkflowPublishError(c, workflow.ErrVersionConflict)
	}
	// Candidate building uses the product read transaction only. Publication has
	// its own durable request and kernel transaction, so the product lock must
	// not be held across that boundary.
	if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		return workflowStoreFailure(c, err)
	}
	requestID := "workflow-publish:" + candidate.WorkflowID + ":" + candidate.ContentHash
	command, err := teamconstruction.PublicationCommandForCandidate(requestID, candidate, teamconstruction.PublicationTarget{
		TeamID:               candidate.Payload.Team.TeamID,
		ExpectedAssetVersion: preflight.version.UpdatedAt.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		var coded interface{ Code() string }
		if errors.As(err, &coded) && coded.Code() != "" {
			return workflowError(c, http.StatusUnprocessableEntity, coded.Code(), coded.Code())
		}
		return workflowStoreFailure(c, err)
	}
	if _, err := s.ProductPublication.Publish(ctx, command); err != nil {
		return mapWorkflowPublishError(c, err)
	}
	published, err := s.Workflow.GetVersion(
		ctx,
		getTenant(c),
		c.Param("id"),
		versionNumber,
	)
	if err != nil {
		return workflowStoreFailure(c, err)
	}
	return c.JSON(http.StatusOK, published)
}

func mapWorkflowPublishError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, workflow.ErrNotFound):
		return workflowError(c, http.StatusNotFound, "workflow_not_found", "workflow not found")
	case errors.Is(err, workflow.ErrArchived),
		errors.Is(err, workflow.ErrVersionConflict),
		errors.Is(err, publication.ErrRequestConflict):
		return workflowError(
			c,
			http.StatusConflict,
			"workflow_version_conflict",
			"workflow version conflict",
		)
	case errors.Is(err, publication.ErrInvalidRequest),
		errors.Is(err, publication.ErrInvalidReceipt):
		return workflowError(c, http.StatusUnprocessableEntity, "workflow_publication_invalid", "workflow publication could not be verified")
	case errors.Is(err, execution.ErrSubjectMismatch):
		return workflowError(c, http.StatusConflict, "workflow_publication_owner_conflict", "workflow publication is already owned by another operator")
	case errors.Is(err, execution.ErrSubjectRequired):
		return workflowError(c, http.StatusUnauthorized, "workflow_publication_identity_required", "authenticated publication identity is required")
	default:
		return workflowStoreFailure(c, err)
	}
}

func (s *Server) mapWorkflowWriteError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, workflow.ErrNotFound):
		return workflowError(
			c,
			http.StatusNotFound,
			"workflow_not_found",
			"workflow not found",
		)
	case errors.Is(err, workflow.ErrArchived):
		return workflowError(
			c,
			http.StatusConflict,
			"workflow_archived",
			"workflow archived",
		)
	case errors.Is(err, workflow.ErrDraftExists):
		return workflowError(
			c,
			http.StatusConflict,
			"workflow_draft_conflict",
			"workflow draft conflict",
		)
	case errors.Is(err, workflow.ErrVersionConflict):
		return workflowError(
			c,
			http.StatusConflict,
			"workflow_draft_conflict",
			"workflow draft conflict",
		)
	default:
		return workflowStoreFailure(c, err)
	}
}

func decodeWorkflowBody(c echo.Context, destination any) error {
	decoder := json.NewDecoder(c.Request().Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("workflow request contains trailing JSON")
		}
		return err
	}
	return nil
}

func decodeEmptyWorkflowBody(c echo.Context) error {
	decoder := json.NewDecoder(c.Request().Body)
	decoder.DisallowUnknownFields()
	var request *struct{}
	if err := decoder.Decode(&request); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	if request == nil {
		return errors.New("workflow request must be an empty object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("workflow request contains trailing JSON")
		}
		return err
	}
	return nil
}

func validWorkflowSchemaObject(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	if len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
		return false
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return false
	}
	schemaVersion, ok := object["schema_version"]
	return ok && strings.TrimSpace(string(schemaVersion)) == "1"
}

func workflowVersionParam(c echo.Context) (int, bool) {
	version, err := strconv.ParseInt(c.Param("version"), 10, 32)
	if err != nil || version <= 0 {
		return 0, false
	}
	return int(version), true
}

func workflowSchemaError(c echo.Context) error {
	return workflowError(
		c,
		http.StatusBadRequest,
		"workflow_schema_invalid",
		"workflow schema invalid",
	)
}

func workflowStoreFailure(c echo.Context, err error) error {
	if errors.Is(err, admissionfence.ErrBlocked) {
		return workflowError(c, http.StatusConflict, "workflow_access_revoked", "这项任务需要的权限已撤销，暂时无法启动。")
	}
	if errors.Is(err, admissionfence.ErrChanged) {
		return workflowError(c, http.StatusConflict, "workflow_access_changed", "任务权限已变化，请重新确认后启动。")
	}

	slog.Error(
		"workflow store request failed",
		"method", c.Request().Method,
		"route", c.Path(),
		"workspace", getTenant(c),
		"error", err,
	)
	return workflowError(
		c,
		http.StatusInternalServerError,
		"workflow_store_failed",
		"workflow store failed",
	)
}

func workflowError(c echo.Context, status int, code, message string) error {
	return c.JSON(status, map[string]string{
		"error": message,
		"code":  code,
	})
}
