package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/jinyitao123/weave/internal/app/workflowcatalog"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/labstack/echo/v4"
)

var revocationVersionPattern = regexp.MustCompile(`^[1-9][0-9]*$`)

type revocationErrorBody struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

type revocationErrorEnvelope struct {
	Error revocationErrorBody `json:"error"`
}

type admissionChangeRequest struct {
	IdempotencyKey string `json:"idempotency_key"`
	DesiredBlocked *bool  `json:"desired_blocked"`
	Reason         string `json:"reason"`
}

func (s *Server) handleGetTeamWorkerRevocationImpact(c echo.Context) error {
	limit := 100
	if _, present := c.QueryParams()["limit"]; present {
		rawLimit := c.QueryParam("limit")
		parsed, err := strconv.Atoi(rawLimit)
		if err != nil || parsed < 1 || parsed > 100 {
			return writeRevocationError(
				c,
				http.StatusBadRequest,
				"invalid_revocation_request",
				"limit must be an integer between 1 and 100",
				nil,
			)
		}
		limit = parsed
	}
	if s.Workflow == nil {
		return writeRevocationError(
			c, http.StatusServiceUnavailable, "revocation_store_unavailable", "revocation store is unavailable", nil,
		)
	}
	page, err := s.Workflow.ReadRevocationImpact(
		c.Request().Context(),
		workflowcatalog.RevocationImpactQuery{
			WorkspaceID:   getTenant(c),
			TeamID:        c.Param("id"),
			WorkerAgentID: c.Param("worker"),
			Limit:         limit,
			Cursor:        c.QueryParam("cursor"),
		},
	)
	if err != nil {
		switch {
		case errors.Is(err, workflowcatalog.ErrRevocationImpactInvalid):
			return writeRevocationError(
				c, http.StatusBadRequest, "invalid_revocation_request", "invalid revocation impact query", nil,
			)
		case errors.Is(err, workflowcatalog.ErrRevocationImpactNotFound):
			return writeRevocationError(
				c, http.StatusNotFound, "team_worker_not_found", "team worker relationship was not found", nil,
			)
		default:
			return writeRevocationError(
				c, http.StatusServiceUnavailable, "revocation_store_unavailable", "revocation store is unavailable", nil,
			)
		}
	}
	return c.JSON(http.StatusOK, page)
}

func decodeRevocationJSON(c echo.Context, destination any) error {
	decoder := json.NewDecoder(c.Request().Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request body contains trailing JSON")
		}
		return err
	}
	return nil
}

func writeRevocationError(
	c echo.Context,
	status int,
	code, message string,
	details map[string]any,
) error {
	return c.JSON(status, revocationErrorEnvelope{Error: revocationErrorBody{
		Code:    code,
		Message: message,
		Details: details,
	}})
}

func parseRevocationVersion(raw string) (int, error) {
	if !revocationVersionPattern.MatchString(raw) {
		return 0, errors.New("version must be a positive decimal integer")
	}
	version, err := strconv.Atoi(raw)
	if err != nil || version <= 0 {
		return 0, errors.New("version is out of range")
	}
	return version, nil
}

func (s *Server) handlePutWorkflowAdmission(c echo.Context) error {
	version, err := parseRevocationVersion(c.Param("version"))
	if err != nil {
		return writeRevocationError(
			c, http.StatusBadRequest, "invalid_revocation_request", err.Error(), nil,
		)
	}
	var request admissionChangeRequest
	if err := decodeRevocationJSON(c, &request); err != nil {
		return writeRevocationError(
			c, http.StatusBadRequest, "invalid_revocation_request", "invalid admission request", nil,
		)
	}
	if request.IdempotencyKey == "" || request.IdempotencyKey != strings.TrimSpace(request.IdempotencyKey) ||
		request.DesiredBlocked == nil || strings.TrimSpace(request.Reason) == "" {
		return writeRevocationError(
			c,
			http.StatusBadRequest,
			"invalid_revocation_request",
			"idempotency_key, desired_blocked, and a non-empty reason are required",
			nil,
		)
	}
	if s.Workflow == nil {
		return writeRevocationError(
			c, http.StatusServiceUnavailable, "revocation_store_unavailable", "revocation store is unavailable", nil,
		)
	}

	result, err := s.WorkflowArtifacts.SetBlocked(c.Request().Context(), workflow.AdmissionChange{
		WorkspaceID:     getTenant(c),
		WorkflowID:      c.Param("id"),
		WorkflowVersion: version,
		IdempotencyKey:  request.IdempotencyKey,
		DesiredBlocked:  *request.DesiredBlocked,
		OperatorID:      getUserID(c),
		Reason:          strings.TrimSpace(request.Reason),
	})
	if err != nil {
		switch {
		case errors.Is(err, workflow.ErrNotFound):
			return writeRevocationError(
				c,
				http.StatusNotFound,
				"workflow_version_admission_not_found",
				"workflow version admission state was not found",
				nil,
			)
		case errors.Is(err, workflow.ErrAdmissionIdempotencyConflict):
			return writeRevocationError(
				c,
				http.StatusConflict,
				"admission_idempotency_conflict",
				"idempotency key is already bound to another admission command",
				nil,
			)
		default:
			return writeRevocationError(
				c,
				http.StatusServiceUnavailable,
				"revocation_store_unavailable",
				"revocation store is unavailable",
				nil,
			)
		}
	}
	return c.JSON(http.StatusOK, result)
}

func (s *Server) handleListWorkflowAdmissionAudit(c echo.Context) error {
	version, err := parseRevocationVersion(c.Param("version"))
	if err != nil {
		return writeRevocationError(
			c, http.StatusBadRequest, "invalid_revocation_request", err.Error(), nil,
		)
	}
	if s.Workflow == nil {
		return writeRevocationError(
			c, http.StatusServiceUnavailable, "revocation_store_unavailable", "revocation store is unavailable", nil,
		)
	}

	workflowID := c.Param("id")
	storedVersion, err := s.Workflow.GetVersion(
		c.Request().Context(), getTenant(c), workflowID, version,
	)
	if err != nil || storedVersion.Status != workflow.VersionStatusPublished {
		if err == nil || errors.Is(err, workflow.ErrNotFound) {
			return writeRevocationError(
				c,
				http.StatusNotFound,
				"workflow_version_admission_not_found",
				"workflow version admission state was not found",
				nil,
			)
		}
		return writeRevocationError(
			c, http.StatusServiceUnavailable, "revocation_store_unavailable", "revocation store is unavailable", nil,
		)
	}

	audits, err := s.WorkflowArtifacts.ListAdmissionAudit(
		c.Request().Context(), getTenant(c), workflowID, version,
	)
	if err != nil {
		return writeRevocationError(
			c, http.StatusServiceUnavailable, "revocation_store_unavailable", "revocation store is unavailable", nil,
		)
	}
	if audits == nil {
		audits = []workflow.AdmissionAudit{}
	}
	return c.JSON(http.StatusOK, audits)
}
