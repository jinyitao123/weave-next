package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jinyitao123/weave/internal/base/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/labstack/echo/v4"
)

const (
	runtimeContextKey          = "runtime"
	runtimeWorkspaceContextKey = "runtime_workspace"
	maxRuntimeClaimWaitSeconds = 30
)

type runtimeListItem struct {
	ID                       string                               `json:"id"`
	Name                     string                               `json:"name"`
	Engines                  []string                             `json:"engines"`
	EngineCapabilities       map[string]runtimes.EngineCapability `json:"engine_capabilities"`
	FunctionalRevision       int64                                `json:"functional_revision"`
	HealthStatus             string                               `json:"health_status"`
	TotalSlots               int                                  `json:"total_slots"`
	ActiveSlots              int                                  `json:"active_slots"`
	PoolID                   string                               `json:"pool_id,omitempty"`
	ConsecutiveInfraFailures int                                  `json:"consecutive_infra_failures"`
	QuarantineUntil          *time.Time                           `json:"quarantine_until,omitempty"`
	LastFailureReason        string                               `json:"last_failure_reason,omitempty"`
	Enabled                  bool                                 `json:"enabled"`
	RevokedAt                *time.Time                           `json:"revoked_at"`
	DeletedAt                *time.Time                           `json:"deleted_at"`
	Online                   bool                                 `json:"online"`
	LastHeartbeatAt          *time.Time                           `json:"last_heartbeat_at"`
	CreatedAt                time.Time                            `json:"created_at"`
}

func (s *Server) handleListRuntimes(c echo.Context) error {
	if s.Runtimes == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "runtime store not configured"})
	}
	stored, err := s.Runtimes.List(c.Request().Context(), getTenant(c))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	items := make([]runtimeListItem, 0, len(stored))
	for _, runtime := range stored {
		items = append(items, runtimeListItem{
			ID:                       runtime.ID,
			Name:                     runtime.Name,
			Engines:                  runtime.Engines,
			EngineCapabilities:       runtime.EngineCapabilities,
			FunctionalRevision:       runtime.FunctionalRevision,
			HealthStatus:             runtimeHealthStatus(runtime),
			TotalSlots:               runtime.TotalSlots,
			ActiveSlots:              runtime.ActiveSlots,
			PoolID:                   runtime.PoolID,
			ConsecutiveInfraFailures: runtime.ConsecutiveInfraFailures,
			QuarantineUntil:          runtime.QuarantineUntil,
			LastFailureReason:        runtime.LastFailureReason,
			Enabled:                  runtime.Enabled,
			RevokedAt:                runtime.RevokedAt,
			DeletedAt:                runtime.DeletedAt,
			Online:                   runtime.Online,
			LastHeartbeatAt:          runtime.LastHeartbeatAt,
			CreatedAt:                runtime.CreatedAt,
		})
	}
	return c.JSON(http.StatusOK, map[string]any{"runtimes": items})
}

func runtimeHealthStatus(runtime runtimes.Runtime) string {
	if runtime.HealthStatus == "quarantined" {
		return runtime.HealthStatus
	}
	if !runtime.Online {
		return "offline"
	}
	if runtime.HealthStatus == "degraded" {
		return runtime.HealthStatus
	}
	if runtime.ActiveSlots >= runtime.TotalSlots {
		return "busy"
	}
	return "healthy"
}

func (s *Server) handleCreateRuntime(c echo.Context) error {
	if s.Runtimes == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "runtime store not configured"})
	}
	var request struct {
		Name string `json:"name"`
	}
	if err := c.Bind(&request); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}
	request.Name = strings.TrimSpace(request.Name)
	if request.Name == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "name is required"})
	}
	runtime, token, err := s.Runtimes.Create(c.Request().Context(), getTenant(c), request.Name)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusCreated, map[string]any{
		"id": runtime.ID, "name": runtime.Name, "token": token,
		"next_commands": map[string]string{
			"direct":  `weave runtime --server "$WEAVE_API_URL" --runtime-token '` + token + `'`,
			"install": `curl -fsSL "${WEAVE_API_URL%/}/install.sh" | sh -s -- --server "$WEAVE_API_URL" --token '` + token + `'`,
			"docker":  `WEAVE_RUNTIME_TOKEN='` + token + `' docker compose -f docker-compose.platform.yml --profile runtime up -d`,
		},
	})
}

func (s *Server) handleRenameRuntime(c echo.Context) error {
	if s.Runtimes == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "runtime store not configured"})
	}
	var request struct {
		Name   string  `json:"name"`
		PoolID *string `json:"pool_id"`
	}
	if err := c.Bind(&request); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}
	request.Name = strings.TrimSpace(request.Name)
	if request.Name == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "name is required"})
	}
	if err := s.Runtimes.Configure(c.Request().Context(), getTenant(c), c.Param("id"), request.Name, request.PoolID); err != nil {
		if errors.Is(err, runtimes.ErrRuntimeUnavailable) {
			return c.JSON(http.StatusNotFound, map[string]string{"error": "runtime not found"})
		}
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Server) handleDeleteRuntime(c echo.Context) error {
	if s.Runtimes == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "runtime store not configured"})
	}
	if err := s.Runtimes.Delete(c.Request().Context(), getTenant(c), c.Param("id")); err != nil {
		if errors.Is(err, runtimes.ErrRuntimeUnavailable) {
			return c.JSON(http.StatusNotFound, map[string]string{"error": "runtime not found"})
		}
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Server) runtimeAuthMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			authorization := c.Request().Header.Get(echo.HeaderAuthorization)
			token := strings.TrimPrefix(authorization, "Bearer ")
			if token == authorization || !strings.HasPrefix(token, "rtk_") {
				return c.JSON(http.StatusUnauthorized, map[string]string{"error": "invalid runtime token"})
			}
			if s.Runtimes == nil {
				return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "runtime store not configured"})
			}
			runtime, err := s.Runtimes.ValidateToken(c.Request().Context(), token)
			if err != nil {
				if errors.Is(err, runtimes.ErrInvalidRuntimeToken) {
					return c.JSON(http.StatusUnauthorized, map[string]string{"error": "invalid runtime token"})
				}
				return c.JSON(
					http.StatusServiceUnavailable,
					map[string]string{"error": "runtime authentication unavailable"},
				)
			}
			c.Set(runtimeContextKey, runtime)
			c.Set(runtimeWorkspaceContextKey, runtime.WorkspaceID)
			return next(c)
		}
	}
}

func authenticatedRuntime(c echo.Context) *runtimes.Runtime {
	runtime, _ := c.Get(runtimeContextKey).(*runtimes.Runtime)
	return runtime
}

func (s *Server) handleRuntimeHello(c echo.Context) error {
	runtime := authenticatedRuntime(c)
	if runtime == nil {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "runtime authentication required"})
	}
	var request struct {
		Engines      []string                    `json:"engines"`
		Capabilities []runtimes.EngineCapability `json:"engine_capabilities"`
		TotalSlots   int                         `json:"total_slots"`
	}
	if err := c.Bind(&request); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}
	if request.TotalSlots == 0 {
		request.TotalSlots = 1
	}
	if err := s.Runtimes.HelloWithCapabilities(
		c.Request().Context(), runtime.WorkspaceID, runtime.ID,
		request.Engines, request.Capabilities, request.TotalSlots,
	); err != nil {
		if errors.Is(err, runtimes.ErrInvalidEngines) {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
		if errors.Is(err, runtimes.ErrRuntimeUnavailable) {
			return c.JSON(http.StatusUnauthorized, map[string]string{"error": "invalid runtime token"})
		}
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, map[string]string{"runtime_id": runtime.ID, "name": runtime.Name})
}

func (s *Server) handleRuntimeHeartbeat(c echo.Context) error {
	runtime := authenticatedRuntime(c)
	if runtime == nil {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "runtime authentication required"})
	}
	var request struct {
		ActiveSlots *int `json:"active_slots"`
	}
	activeSlots := -1
	if c.Request().Body != nil && c.Request().ContentLength != 0 {
		if err := c.Bind(&request); err != nil {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
		}
		if request.ActiveSlots != nil {
			activeSlots = *request.ActiveSlots
		}
	}
	if err := s.Runtimes.HeartbeatWithLoad(c.Request().Context(), runtime.WorkspaceID, runtime.ID, activeSlots); err != nil {
		if errors.Is(err, runtimes.ErrRuntimeUnavailable) {
			return c.JSON(http.StatusUnauthorized, map[string]string{"error": "invalid runtime token"})
		}
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Server) handleRuntimeClaim(c echo.Context) error {
	runtime := authenticatedRuntime(c)
	if runtime == nil {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "runtime authentication required"})
	}
	if s.Tasks == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "task queue not configured"})
	}
	var request struct {
		WaitSeconds int `json:"wait_seconds"`
	}
	if err := c.Bind(&request); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}
	if request.WaitSeconds < 0 {
		request.WaitSeconds = 0
	}
	if request.WaitSeconds > maxRuntimeClaimWaitSeconds {
		request.WaitSeconds = maxRuntimeClaimWaitSeconds
	}

	workerID := runtimes.RuntimeWorkerID(runtime.WorkspaceID, runtime.ID)
	filter := taskqueue.ClaimFilter{
		Kind: "engine_exec", WorkspaceID: runtime.WorkspaceID,
		RuntimeID: runtime.ID, IdentityKind: taskqueue.IdentityAgent,
	}
	deadline := time.Now().Add(time.Duration(request.WaitSeconds) * time.Second)
	for {
		task, err := s.Tasks.Claim(c.Request().Context(), workerID, filter)
		if err != nil {
			if errors.Is(err, taskqueue.ErrRuntimeClaimUnavailable) {
				return c.JSON(http.StatusUnauthorized, map[string]string{"error": "invalid runtime token"})
			}
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
		if task != nil {
			// Never hand secrets to the daemon: loom payloads are redacted
			// on a copy — the store snapshot keeps the full record for the
			// task-scoped MCP gateway.
			redacted, redactErr := runtimes.RedactClaimPayload(task.Payload)
			if redactErr != nil {
				return c.JSON(http.StatusInternalServerError, map[string]string{"error": redactErr.Error()})
			}
			claimed := *task
			claimed.Payload = redacted
			return c.JSON(http.StatusOK, map[string]any{"task": &claimed})
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return c.NoContent(http.StatusNoContent)
		}
		if remaining > time.Second {
			remaining = time.Second
		}
		timer := time.NewTimer(remaining)
		select {
		case <-c.Request().Context().Done():
			if !timer.Stop() {
				<-timer.C
			}
			return c.Request().Context().Err()
		case <-timer.C:
		}
	}
}

func (s *Server) handleRuntimeTaskRenew(c echo.Context) error {
	runtime, task, err := s.runtimeTask(c)
	if err != nil {
		return err
	}
	workerID := runtimes.RuntimeWorkerID(runtime.WorkspaceID, runtime.ID)
	if task.WorkerID != "" && task.WorkerID != workerID {
		return c.JSON(http.StatusForbidden, map[string]string{"error": "task is not claimed by this runtime"})
	}
	if err := s.Tasks.Heartbeat(c.Request().Context(), task.ID, workerID); err != nil {
		return c.JSON(http.StatusConflict, map[string]string{"error": "task lease lost"})
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Server) handleRuntimeTaskComplete(c echo.Context) error {
	runtime, task, err := s.runtimeTask(c)
	if err != nil {
		return err
	}
	// Typed result: CLI daemons keep sending {"output"} (new fields stay
	// omitted via omitempty); loom daemons add stop_reason/usage/run_id.
	var request runtimes.EngineExecResult
	if err := c.Bind(&request); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}
	normalizeRuntimeUsage(task, &request)
	if err := validateRuntimeEngineExecResult(task, request); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	result, err := json.Marshal(request)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "cannot encode task result"})
	}
	if task.Status == taskqueue.StatusCompleted {
		// The durable result may have committed before the response was lost.
		// A byte-equivalent typed replay acknowledges that same result only.
		var stored runtimes.EngineExecResult
		if json.Unmarshal(task.Result, &stored) == nil {
			canonical, _ := json.Marshal(stored)
			if string(canonical) == string(result) {
				return c.NoContent(http.StatusNoContent)
			}
		}
		return c.JSON(http.StatusConflict, map[string]string{"error": "task already completed with another result"})
	}
	if task.WorkerID != runtimes.RuntimeWorkerID(runtime.WorkspaceID, runtime.ID) {
		return c.JSON(http.StatusForbidden, map[string]string{"error": "task is not claimed by this runtime"})
	}
	if err := s.Tasks.CompleteClaimed(
		c.Request().Context(),
		task.ID,
		runtimes.RuntimeWorkerID(runtime.WorkspaceID, runtime.ID),
		result,
		"",
	); err != nil {
		return c.JSON(http.StatusConflict, map[string]string{"error": "task lease lost"})
	}
	return c.NoContent(http.StatusNoContent)
}

// Older adapters cut a UTF-8 summary at byte 4096. JSON replaces up to three
// trailing partial bytes with U+FFFD, expanding the wire value beyond the bound.
// Remove only that recognizable tail; all other oversized receipts still fail
// validation, and reported usage/provenance are never changed.
func repairLegacyUsageSummary(receipt *engine.UsageReceipt) {
	if receipt == nil || len(receipt.RawSummary) <= 4096 || len(receipt.RawSummary) > 4102 {
		return
	}
	prefix := strings.TrimRight(receipt.RawSummary, "\uFFFD")
	if len(prefix) >= 4093 && len(prefix) <= 4095 {
		receipt.RawSummary = prefix
	}
}

// Optional accounting must not discard a completed answer or its files. An
// invalid receipt contributes no usage and leaves an explicit diagnostic.
func normalizeRuntimeUsage(task *taskqueue.Task, result *runtimes.EngineExecResult) {
	models := make([]string, 0, min(len(result.ReportedModels), 16))
	for _, model := range result.ReportedModels {
		if len(models) == 16 {
			break
		}
		if model != "" && len(model) <= 200 && !strings.ContainsAny(model, "\n\r\t") {
			models = append(models, model)
		}
	}
	result.ReportedModels = models

	if engine.ValidateDiagnostics(result.Diagnostics) != nil {
		result.Diagnostics = []engine.Diagnostic{{Code: "diagnostics_invalid", Message: "Some execution diagnostics were invalid; work output is preserved"}}
	}
	if engine.ValidateEvents(result.Events) != nil {
		result.Events = nil
		result.RetrySafeBeforeExecution = false
		result.Diagnostics = append(result.Diagnostics[:min(len(result.Diagnostics), 31)], engine.Diagnostic{Code: "events_invalid", Message: "Recorded activity was invalid; work output is preserved"})
	}
	if result.Status != "failed" || len(result.Artifacts) > 0 {
		result.RetrySafeBeforeExecution = false
	}
	for _, event := range result.Events {
		if event.Kind == "tool_call" || event.Kind == "tool_result" {
			result.RetrySafeBeforeExecution = false
		}
	}
	var payload runtimes.EngineExecRequest
	if json.Unmarshal(task.Payload, &payload) != nil || !engine.IsCLIEngine(payload.Engine) || result.UsageReceipt == nil {
		return
	}
	repairLegacyUsageSummary(result.UsageReceipt)
	if validateRuntimeUsageReceipt(payload, result.UsageReceipt) == nil {
		return
	}
	result.UsageReceipt = nil
	result.Diagnostics = append(result.Diagnostics[:min(len(result.Diagnostics), 31)], engine.Diagnostic{
		Code: "usage_invalid", Message: "CLI usage statistics are unavailable because the receipt is invalid; work output is preserved",
	})
}

func validateRuntimeEngineExecResult(task *taskqueue.Task, result runtimes.EngineExecResult) error {
	if task == nil {
		return errors.New("task is required")
	}
	var payload runtimes.EngineExecRequest
	if err := json.Unmarshal(task.Payload, &payload); err != nil {
		return errors.New("task payload is invalid")
	}
	if !engine.IsCLIEngine(payload.Engine) {
		if result.UsageReceipt != nil || len(result.Diagnostics) > 0 || len(result.Events) > 0 ||
			len(result.Artifacts) > 0 || result.Status != "" || result.Error != "" {
			return errors.New("CLI result fields are forbidden for a non-CLI task")
		}
		return nil
	}
	if result.Usage != nil || result.RunID != "" || result.StopReason != "" {
		return errors.New("loom result fields are forbidden for a CLI task")
	}
	switch result.Status {
	case "", "completed", "failed", "timeout":
	default:
		return errors.New("engine result status is invalid")
	}
	if result.Status == "completed" && result.Error != "" {
		return errors.New("completed engine result cannot carry an error")
	}
	if (result.Status == "failed" || result.Status == "timeout") && strings.TrimSpace(result.Error) == "" {
		return errors.New("failed engine result requires an error")
	}
	if err := engine.ValidateDiagnostics(result.Diagnostics); err != nil {
		return err
	}
	if err := engine.ValidateEvents(result.Events); err != nil {
		return err
	}
	if err := engine.ValidateArtifacts(result.Artifacts); err != nil {
		return err
	}
	return validateRuntimeUsageReceipt(payload, result.UsageReceipt)
}

func validateRuntimeUsageReceipt(payload runtimes.EngineExecRequest, receipt *engine.UsageReceipt) error {
	if err := engine.ValidateUsageReceipt(receipt); err != nil {
		return err
	}
	if receipt != nil {
		if receipt.Scope != engine.UsageScopeInvocation {
			return errors.New("resumed/session-cumulative CLI usage is not accepted")
		}
		if strings.TrimSpace(payload.EngineVersion) == "" || receipt.EngineVersion != payload.EngineVersion {
			return errors.New("usage receipt engine_version does not match the admitted runtime")
		}
	}
	return nil
}

func (s *Server) handleRuntimeTaskFail(c echo.Context) error {
	runtime, task, err := s.claimedRuntimeTask(c)
	if err != nil {
		return err
	}
	var request struct {
		Error string `json:"error"`
	}
	if err := c.Bind(&request); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}
	if err := s.Tasks.FailClaimed(
		c.Request().Context(),
		task.ID,
		runtimes.RuntimeWorkerID(runtime.WorkspaceID, runtime.ID),
		request.Error,
	); err != nil {
		return c.JSON(http.StatusConflict, map[string]string{"error": "task lease lost"})
	}
	return c.NoContent(http.StatusNoContent)
}

// The runtime sends this acknowledgement only after waiting for execution exit.
// Authentication remains pinned to the exact runtime and original task owner.
func (s *Server) handleRuntimeTaskStopped(c echo.Context) error {
	runtime, task, err := s.runtimeTask(c)
	if err != nil {
		return err
	}
	workerID := runtimes.RuntimeWorkerID(runtime.WorkspaceID, runtime.ID)
	if task.WorkerID == "" && task.IsTerminal() {
		return c.NoContent(http.StatusNoContent) // acknowledgement response was lost
	}
	if task.WorkerID != workerID {
		return c.JSON(http.StatusForbidden, map[string]string{"error": "task is not claimed by this runtime"})
	}
	if task.Status != taskqueue.StatusRunning && task.Status != taskqueue.StatusCancelRequested &&
		!(task.Status == taskqueue.StatusFailed && task.Error == taskqueue.RuntimeLeaseExpiredError) {
		return c.JSON(http.StatusConflict, map[string]string{"error": "execution stop is not awaiting acknowledgement"})
	}
	if err := s.Tasks.AcknowledgeExecutionStopped(c.Request().Context(), task.ID, workerID); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "cannot acknowledge execution stop"})
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Server) handleRuntimeTaskAttachment(c echo.Context) error {
	runtime, task, err := s.claimedRuntimeTask(c)
	if err != nil {
		return err
	}
	attachmentID := c.Param("aid")
	if !runtimeTaskHasAttachment(task, attachmentID) {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "attachment not found"})
	}
	root := ""
	if s.Config != nil {
		root = s.Config.WorkspacesRoot
	}
	attachment, err := ResolveAttachment(root, runtime.WorkspaceID, attachmentID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "attachment not found"})
	}
	return c.File(attachment.Path)
}

func (s *Server) runtimeTask(c echo.Context) (*runtimes.Runtime, *taskqueue.Task, error) {
	runtime := authenticatedRuntime(c)
	if runtime == nil {
		return nil, nil, echo.NewHTTPError(http.StatusUnauthorized, "runtime authentication required")
	}
	if s.Tasks == nil {
		return nil, nil, echo.NewHTTPError(http.StatusServiceUnavailable, "task queue not configured")
	}
	task, err := s.Tasks.Get(c.Request().Context(), runtime.WorkspaceID, c.Param("id"))
	if err != nil || task.Kind != "engine_exec" || task.RuntimeID != runtime.ID {
		return nil, nil, echo.NewHTTPError(http.StatusNotFound, "task not found")
	}
	return runtime, task, nil
}

func (s *Server) claimedRuntimeTask(c echo.Context) (*runtimes.Runtime, *taskqueue.Task, error) {
	runtime, task, err := s.runtimeTask(c)
	if err != nil {
		return nil, nil, err
	}
	if task.WorkerID != runtimes.RuntimeWorkerID(runtime.WorkspaceID, runtime.ID) {
		return nil, nil, echo.NewHTTPError(http.StatusForbidden, "task is not claimed by this runtime")
	}
	return runtime, task, nil
}

func runtimeTaskHasAttachment(task *taskqueue.Task, attachmentID string) bool {
	var payload struct {
		Attachments []struct {
			ID string `json:"id"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal(task.Payload, &payload); err != nil {
		return false
	}
	for _, attachment := range payload.Attachments {
		if attachment.ID == attachmentID {
			return true
		}
	}
	return false
}
