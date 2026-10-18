package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"github.com/jinyitao123/weave/internal/kernel/runtimeprotocol"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
)

// Control sessions on the person's own devices (weave-workbench decision 009,
// contract 设备控制契约). A session is one task pinned to the device; each
// operation is queued here, taken by the device with a long poll and answered.

const (
	deviceSessionRequestLimit = 256
	deviceSessionReasonLimit  = 1000
	deviceSessionIdle         = 5 * time.Minute
	deviceSessionLimit        = 60 * time.Minute
	deviceSessionUnreachable  = 30 * time.Second
	deviceOperationWait       = 60 * time.Second
	deviceExecTimeoutMax      = 600
	deviceNextWaitMax         = 25
	deviceParamsLimit         = 1 << 20
	devicePollInterval        = 200 * time.Millisecond
)

// deviceOperationLevels maps each operation to the level that allows it.
var deviceOperationLevels = map[string]string{
	"screenshot": "observe", "ui_tree": "observe", "windows": "observe",
	"act":  "act",
	"exec": "exec", "file_read": "exec", "file_write": "exec",
}

type deviceFacts struct {
	Platform          string   `json:"platform"`
	Available         bool     `json:"available"`
	UnavailableReason string   `json:"unavailable_reason"`
	Levels            []string `json:"levels"`
}

func deviceFactsOf(runtime *runtimes.Runtime) deviceFacts {
	var facts deviceFacts
	_ = json.Unmarshal(runtime.DeviceFacts, &facts)
	return facts
}

func deviceOnline(runtime *runtimes.Runtime) bool {
	return runtime.LastHeartbeatAt != nil && time.Since(*runtime.LastHeartbeatAt) < 90*time.Second
}

func deviceFail(c echo.Context, status int, code, message string) error {
	return c.JSON(status, map[string]string{"code": code, "error": message})
}

// deviceRefusal is a check's answer when it refuses. Checks return it rather
// than writing the response, so a refused request can never run on.
type deviceRefusal struct {
	status        int
	code, message string
}

func refuse(status int, code, message string) *deviceRefusal {
	return &deviceRefusal{status: status, code: code, message: message}
}

func (r *deviceRefusal) send(c echo.Context) error {
	return deviceFail(c, r.status, r.code, r.message)
}

// ownDevice loads a device the caller enrolled; any other runtime is unknown to them.
func (s *Server) ownDevice(c echo.Context, id string) (*runtimes.Runtime, *deviceRefusal) {
	runtime, err := s.Runtimes.Get(c.Request().Context(), getTenant(c), id)
	if err != nil || runtime.DeviceOwnerUserID == "" || runtime.DeletedAt != nil {
		return nil, refuse(http.StatusNotFound, "not_found", "没有这台设备")
	}
	if runtime.DeviceOwnerUserID != getUserID(c) {
		return nil, refuse(http.StatusForbidden, "forbidden", "这台设备绑定的是其他账号")
	}
	return runtime, nil
}

func (s *Server) deviceGuard(c echo.Context) *deviceRefusal {
	if s.Runtimes == nil || s.Tasks == nil || s.GetPool() == nil {
		return refuse(http.StatusServiceUnavailable, "temporarily_unavailable", "服务暂不可用")
	}
	if !personalAccountSession(c) {
		return refuse(http.StatusForbidden, "account_session_required", "设备只能用本人账号登录后控制，密钥不能办理")
	}
	return nil
}

func (s *Server) handleListDevices(c echo.Context) error {
	if refusal := s.deviceGuard(c); refusal != nil {
		return refusal.send(c)
	}
	all, err := s.Runtimes.List(c.Request().Context(), getTenant(c))
	if err != nil {
		return deviceFail(c, http.StatusInternalServerError, "failed", "设备读取失败")
	}
	devices := []map[string]any{}
	for i := range all {
		runtime := &all[i]
		if runtime.DeviceOwnerUserID != getUserID(c) || runtime.DeletedAt != nil {
			continue
		}
		facts := deviceFactsOf(runtime)
		view := map[string]any{"device_id": runtime.ID, "name": runtime.Name, "platform": facts.Platform,
			"online": deviceOnline(runtime), "levels": facts.Levels, "available": facts.Available && deviceOnline(runtime)}
		if reason := deviceUnavailableReason(runtime, facts); reason != "" {
			view["unavailable_reason"] = reason
		}
		if session, err := s.activeDeviceSession(c.Request().Context(), runtime.WorkspaceID, runtime.ID); err == nil && session != "" {
			view["session_id"] = session
		}
		devices = append(devices, view)
	}
	return c.JSON(http.StatusOK, map[string]any{"devices": devices})
}

func deviceUnavailableReason(runtime *runtimes.Runtime, facts deviceFacts) string {
	switch {
	case !deviceOnline(runtime):
		return "设备不在线"
	case !facts.Available && facts.UnavailableReason != "":
		return facts.UnavailableReason
	case !facts.Available:
		return "设备尚未报告可用状态"
	}
	return ""
}

// activeDeviceSession returns the device's open session, if any.
func (s *Server) activeDeviceSession(ctx context.Context, workspaceID, runtimeID string) (string, error) {
	var id string
	err := s.GetPool().QueryRow(ctx, `SELECT id FROM weave_task_queue
		WHERE workspace_id=$1 AND runtime_id=$2 AND kind=$3 AND status IN ('queued','running','cancel_requested')
		ORDER BY created_at DESC LIMIT 1`, workspaceID, runtimeID, taskqueue.KindDeviceSession).Scan(&id)
	if err != nil {
		return "", err
	}
	return id, nil
}

type deviceSessionPayload struct {
	RequestID string            `json:"request_id"`
	Reason    string            `json:"reason"`
	OpenedBy  string            `json:"opened_by"`
	Requests  map[string]string `json:"requests,omitempty"`
}

func (s *Server) handleOpenDeviceSession(c echo.Context) error {
	if refusal := s.deviceGuard(c); refusal != nil {
		return refusal.send(c)
	}
	runtime, refusal := s.ownDevice(c, c.Param("id"))
	if refusal != nil {
		return refusal.send(c)
	}
	var request struct {
		Reason    string `json:"reason"`
		RequestID string `json:"request_id"`
	}
	if err := c.Bind(&request); err != nil || strings.TrimSpace(request.Reason) == "" || utf8.RuneCountInString(strings.TrimSpace(request.Reason)) > deviceSessionReasonLimit || uuid.Validate(request.RequestID) != nil {
		return deviceFail(c, http.StatusBadRequest, "invalid_arguments", "原因须为 1 至 1000 字，并给出 UUID 形式的 request_id")
	}
	ctx := c.Request().Context()
	openedBy := s.callerName(c)
	tx, err := s.GetPool().Begin(ctx)
	if err != nil {
		return deviceFail(c, http.StatusInternalServerError, "failed", "会话登记失败")
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Serialize the request across devices, then admission to this device. Both
	// locks and EnqueueTx belong to the same existing task-queue transaction.
	for _, identity := range [][]string{
		{"request", runtime.WorkspaceID, getUserID(c), request.RequestID},
		{"device", runtime.WorkspaceID, runtime.ID},
	} {
		key, _ := json.Marshal(identity)
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(current_schema() || $1,0))`, "device-session:"+string(key)); err != nil {
			return deviceFail(c, http.StatusInternalServerError, "failed", "会话登记失败")
		}
	}
	// Every acknowledged request keeps its device and reason binding, including
	// requests that joined an already active session, even after that session ends.
	var existing, existingRuntime string
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT id, runtime_id, payload FROM weave_task_queue
		WHERE workspace_id=$1 AND kind=$2 AND actor_subject->>'user_id'=$4
		AND (payload->>'request_id'=$3 OR payload->'requests' ? $3)`,
		runtime.WorkspaceID, taskqueue.KindDeviceSession, request.RequestID, getUserID(c)).Scan(&existing, &existingRuntime, &raw)
	if err == nil {
		var payload deviceSessionPayload
		if err := json.Unmarshal(raw, &payload); err != nil {
			return deviceFail(c, http.StatusInternalServerError, "failed", "会话读取失败")
		}
		reason := payload.Reason
		if payload.RequestID != request.RequestID {
			reason = payload.Requests[request.RequestID]
		}
		if existingRuntime != runtime.ID || reason != strings.TrimSpace(request.Reason) {
			return deviceFail(c, http.StatusConflict, "request_conflict", "同一个 request_id 用于了不同的内容")
		}
		_ = tx.Rollback(ctx)
		return s.deviceSessionResponse(c, existing)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return deviceFail(c, http.StatusInternalServerError, "failed", "会话读取失败")
	}
	err = tx.QueryRow(ctx, `SELECT id, payload FROM weave_task_queue
		WHERE workspace_id=$1 AND runtime_id=$2 AND kind=$3 AND status IN ('queued','running','cancel_requested')
		ORDER BY created_at DESC LIMIT 1`, runtime.WorkspaceID, runtime.ID, taskqueue.KindDeviceSession).Scan(&existing, &raw)
	if err == nil {
		var payload deviceSessionPayload
		if err := json.Unmarshal(raw, &payload); err != nil {
			return deviceFail(c, http.StatusInternalServerError, "failed", "会话读取失败")
		}
		if len(payload.Requests)+1 >= deviceSessionRequestLimit {
			return deviceFail(c, http.StatusConflict, "session_request_limit", "当前会话的请求记录已满，请先结束该会话，再重新打开")
		}
		if _, err := tx.Exec(ctx, `UPDATE weave_task_queue SET payload=payload || jsonb_build_object('requests',
			COALESCE(payload->'requests','{}'::jsonb) || jsonb_build_object($3::text,$4::text))
			WHERE workspace_id=$1 AND id=$2`, runtime.WorkspaceID, existing, request.RequestID, strings.TrimSpace(request.Reason)); err != nil {
			return deviceFail(c, http.StatusInternalServerError, "failed", "会话登记失败")
		}
		if err := tx.Commit(ctx); err != nil {
			return deviceFail(c, http.StatusInternalServerError, "failed", "会话登记失败")
		}
		return s.deviceSessionResponse(c, existing)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return deviceFail(c, http.StatusInternalServerError, "failed", "会话读取失败")
	}
	facts := deviceFactsOf(runtime)
	if reason := deviceUnavailableReason(runtime, facts); reason != "" {
		code := "device_unavailable"
		if !deviceOnline(runtime) {
			code = "device_offline"
		}
		return deviceFail(c, http.StatusConflict, code, reason)
	}
	payload, _ := json.Marshal(deviceSessionPayload{RequestID: request.RequestID, Reason: strings.TrimSpace(request.Reason), OpenedBy: openedBy})
	deadline := time.Now().Add(deviceSessionLimit)
	task := &taskqueue.Task{
		ID: uuid.NewString(), WorkspaceID: runtime.WorkspaceID,
		IdentityKind: taskqueue.IdentityDevice, IdentitySchemaVersion: 2,
		Source: "device", Kind: taskqueue.KindDeviceSession, RuntimeID: runtime.ID,
		Payload: payload, DeadlineAt: &deadline, OutcomeSensitive: true,
	}
	if err := s.Tasks.EnqueueTx(ctx, tx, task); err != nil {
		return deviceFail(c, http.StatusInternalServerError, "failed", "会话登记失败")
	}
	if err := tx.Commit(ctx); err != nil {
		return deviceFail(c, http.StatusInternalServerError, "failed", "会话登记失败")
	}
	return s.deviceSessionResponse(c, task.ID)
}

func (s *Server) callerName(c echo.Context) string {
	if s.UserStore != nil {
		if user, err := s.UserStore.GetByID(c.Request().Context(), getTenant(c), getUserID(c)); err == nil && user != nil {
			if user.DisplayName != "" {
				return user.DisplayName
			}
			return user.Username
		}
	}
	return getUserID(c)
}

// ownDeviceSession loads a session the caller opened, settling its limits first.
func (s *Server) ownDeviceSession(c echo.Context, id string) (*taskqueue.Task, *deviceRefusal) {
	ctx := c.Request().Context()
	task, err := s.Tasks.Get(ctx, getTenant(c), id)
	if err != nil || task.Kind != taskqueue.KindDeviceSession || task.Subject.UserID != getUserID(c) {
		return nil, refuse(http.StatusNotFound, "not_found", "没有这个会话")
	}
	return s.settleDeviceSession(ctx, task), nil
}

// settleDeviceSession ends a session that has outlived its limits and returns
// its current state. Limits are applied whenever the session is looked at,
// by its owner or by the device.
func (s *Server) settleDeviceSession(ctx context.Context, task *taskqueue.Task) *taskqueue.Task {
	now := time.Now()
	reason := ""
	switch task.Status {
	case taskqueue.StatusQueued:
		if now.Sub(task.CreatedAt) > deviceSessionUnreachable {
			reason = "device_unreachable"
		}
	case taskqueue.StatusRunning:
		started := task.CreatedAt
		if task.StartedAt != nil {
			started = *task.StartedAt
		}
		last, _ := s.Runtimes.DeviceLastActivity(ctx, task.ID)
		if last.Before(started) {
			last = started
		}
		switch {
		case now.Sub(task.CreatedAt) > deviceSessionLimit:
			reason = "time_limit"
		case now.Sub(last) > deviceSessionIdle:
			reason = "idle"
		}
	}
	if reason != "" {
		s.endDeviceSession(ctx, task, reason)
		if fresh, err := s.Tasks.Get(ctx, task.WorkspaceID, task.ID); err == nil {
			task = fresh
		}
	}
	if task.IsTerminal() {
		_ = s.Runtimes.EndDeviceOperations(ctx, task.ID)
	}
	return task
}

// endDeviceSession records the reason and asks the task to stop; a claimed
// session stops when the device next looks, an unclaimed one at once.
func (s *Server) endDeviceSession(ctx context.Context, task *taskqueue.Task, reason string) {
	_ = s.Runtimes.RecordDeviceSessionEnd(ctx, task.WorkspaceID, task.ID, reason)
	if !task.IsTerminal() {
		_ = s.Tasks.Cancel(ctx, task.WorkspaceID, task.ID)
	}
	if task.WorkerID == "" {
		_ = s.Runtimes.EndDeviceOperations(ctx, task.ID)
	}
}

func deviceSessionState(task *taskqueue.Task) string {
	switch task.Status {
	case taskqueue.StatusQueued:
		return "opening"
	case taskqueue.StatusRunning:
		return "ready"
	case taskqueue.StatusCancelRequested:
		return "closing"
	}
	return "ended"
}

func (s *Server) deviceSessionView(ctx context.Context, task *taskqueue.Task) map[string]any {
	var payload deviceSessionPayload
	_ = json.Unmarshal(task.Payload, &payload)
	view := map[string]any{"session_id": task.ID, "device_id": task.RuntimeID, "state": deviceSessionState(task),
		"reason": payload.Reason, "opened_at": task.CreatedAt}
	if runtime, err := s.Runtimes.Get(ctx, task.WorkspaceID, task.RuntimeID); err == nil {
		view["device_name"] = runtime.Name
	}
	if task.IsTerminal() {
		reason, _ := s.Runtimes.DeviceSessionEndReason(ctx, task.ID)
		if reason == "" {
			reason = "device_disconnected"
			if task.Status == taskqueue.StatusCompleted {
				reason = "ended_on_device"
			}
		}
		view["end_reason"] = reason
	} else {
		view["remaining_seconds"] = int(time.Until(task.CreatedAt.Add(deviceSessionLimit)).Seconds())
	}
	return view
}

func (s *Server) deviceSessionResponse(c echo.Context, id string) error {
	task, err := s.Tasks.Get(c.Request().Context(), getTenant(c), id)
	if err != nil {
		return deviceFail(c, http.StatusInternalServerError, "failed", "会话读取失败")
	}
	task = s.settleDeviceSession(c.Request().Context(), task)
	return c.JSON(http.StatusOK, s.deviceSessionView(c.Request().Context(), task))
}

func (s *Server) handleReadDeviceSession(c echo.Context) error {
	if refusal := s.deviceGuard(c); refusal != nil {
		return refusal.send(c)
	}
	task, refusal := s.ownDeviceSession(c, c.Param("id"))
	if refusal != nil {
		return refusal.send(c)
	}
	return c.JSON(http.StatusOK, s.deviceSessionView(c.Request().Context(), task))
}

func (s *Server) handleCloseDeviceSession(c echo.Context) error {
	if refusal := s.deviceGuard(c); refusal != nil {
		return refusal.send(c)
	}
	task, refusal := s.ownDeviceSession(c, c.Param("id"))
	if refusal != nil {
		return refusal.send(c)
	}
	if !task.IsTerminal() {
		s.endDeviceSession(c.Request().Context(), task, "closed")
	}
	return s.deviceSessionResponse(c, task.ID)
}

// handleDeviceOperation queues one operation and waits for its answer. A
// retry with the same op_id waits for the same operation, never a second one.
func (s *Server) handleDeviceOperation(c echo.Context) error {
	if refusal := s.deviceGuard(c); refusal != nil {
		return refusal.send(c)
	}
	task, refusal := s.ownDeviceSession(c, c.Param("id"))
	if refusal != nil {
		return refusal.send(c)
	}
	var request struct {
		OpID   string          `json:"op_id"`
		Kind   string          `json:"kind"`
		Params json.RawMessage `json:"params"`
	}
	if err := c.Bind(&request); err != nil || uuid.Validate(request.OpID) != nil {
		return deviceFail(c, http.StatusBadRequest, "invalid_arguments", "需要 UUID 形式的 op_id")
	}
	level, known := deviceOperationLevels[request.Kind]
	if !known {
		return deviceFail(c, http.StatusBadRequest, "invalid_arguments", "不支持的操作")
	}
	if len(request.Params) == 0 {
		request.Params = json.RawMessage(`{}`)
	}
	if len(request.Params) > deviceParamsLimit {
		return deviceFail(c, http.StatusRequestEntityTooLarge, "too_large", "参数超过上限")
	}
	ctx := c.Request().Context()
	runtime, err := s.Runtimes.Get(ctx, task.WorkspaceID, task.RuntimeID)
	if err != nil {
		return deviceFail(c, http.StatusInternalServerError, "failed", "设备读取失败")
	}
	if !slices.Contains(deviceFactsOf(runtime).Levels, level) {
		return deviceFail(c, http.StatusForbidden, "level_not_allowed", "这台设备的本机配置不允许这类操作")
	}
	digest := sha256.Sum256(append([]byte(request.Kind+"\n"), request.Params...))
	recorded, _, err := s.Runtimes.QueueDeviceOperation(ctx, runtimes.DeviceOperation{
		WorkspaceID: task.WorkspaceID, SessionID: task.ID, RuntimeID: task.RuntimeID, UserID: getUserID(c),
		OpID: request.OpID, Kind: request.Kind, RequestDigest: hex.EncodeToString(digest[:]),
		Audit: deviceOperationAudit(request.Kind, request.Params), Params: request.Params,
	})
	if errors.Is(err, runtimes.ErrDeviceOperationConflict) {
		return deviceFail(c, http.StatusConflict, "request_conflict", "同一个 op_id 用于了不同的内容")
	}
	if err != nil {
		return deviceFail(c, http.StatusInternalServerError, "failed", "操作登记失败")
	}
	if recorded.Status == runtimes.DeviceOperationQueued && task.IsTerminal() {
		_ = s.Runtimes.EndDeviceOperations(ctx, task.ID)
	}
	wait := deviceOperationWait
	if request.Kind == "exec" {
		var exec struct {
			Timeout int `json:"timeout_seconds"`
		}
		_ = json.Unmarshal(request.Params, &exec)
		if exec.Timeout > 0 && exec.Timeout <= deviceExecTimeoutMax {
			wait = time.Duration(exec.Timeout)*time.Second + 15*time.Second
		}
	}
	deadline := time.Now().Add(wait)
	for {
		op, err := s.Runtimes.DeviceOperation(ctx, task.ID, request.OpID)
		if err != nil {
			return deviceFail(c, http.StatusInternalServerError, "failed", "操作读取失败")
		}
		switch op.Status {
		case runtimes.DeviceOperationSucceeded, runtimes.DeviceOperationFailed:
			taken, err := s.Runtimes.TakeDeviceOperationResult(ctx, task.ID, request.OpID)
			if err != nil {
				return deviceFail(c, http.StatusInternalServerError, "failed", "结果读取失败")
			}
			if taken.Status == runtimes.DeviceOperationFailed {
				return c.JSON(http.StatusOK, map[string]any{"status": "failed", "error": taken.Error})
			}
			// Results are not kept once read; a retry learns the outcome only.
			if len(taken.Result) == 0 {
				return c.JSON(http.StatusOK, map[string]any{"status": "succeeded", "result": json.RawMessage(`{}`), "result_already_delivered": true})
			}
			return c.JSON(http.StatusOK, map[string]any{"status": "succeeded", "result": json.RawMessage(orEmptyObject(taken.Result))})
		case runtimes.DeviceOperationUnknown:
			return deviceFail(c, http.StatusOK, "unknown_result", "操作已发往设备，但会话在回执前结束；请先查看设备上的实际状态，再决定下一步")
		case runtimes.DeviceOperationStopped:
			return deviceFail(c, http.StatusConflict, "session_ended", "会话已结束，操作没有发往设备")
		}
		if time.Now().After(deadline) {
			return c.JSON(http.StatusAccepted, map[string]any{"status": op.Status, "op_id": op.OpID,
				"message": "设备还没有回执；用同一个 op_id 再次调用即可继续等待同一项操作"})
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(devicePollInterval):
		}
		if fresh, err := s.Tasks.Get(ctx, task.WorkspaceID, task.ID); err == nil {
			s.settleDeviceSession(ctx, fresh)
		}
	}
}

func orEmptyObject(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	return raw
}

// deviceOperationAudit keeps only content-free facts about a request: which
// action and on what, how much text, which program and how many arguments,
// which path. Typed text, arguments and file content are never kept.
func deviceOperationAudit(kind string, params json.RawMessage) json.RawMessage {
	var p map[string]any
	_ = json.Unmarshal(params, &p)
	audit := map[string]any{}
	switch kind {
	case "act":
		if action, ok := p["action"].(string); ok {
			audit["action"] = action
		}
		if text, ok := p["text"].(string); ok {
			audit["text_length"] = len([]rune(text))
		}
		if app, ok := p["app"].(string); ok {
			audit["app"] = app
		}
	case "exec":
		if argv, ok := p["argv"].([]any); ok && len(argv) > 0 {
			audit["program"], audit["arguments"] = argv[0], len(argv)-1
		}
	case "file_read", "file_write":
		if path, ok := p["path"].(string); ok {
			audit["path"] = path
		}
		if content, ok := p["content"].(string); ok {
			audit["bytes"] = len(content)
		}
	}
	raw, _ := json.Marshal(audit)
	return raw
}

// --- The device's side: claim, take the next operation, answer, end. ---

func (s *Server) claimDeviceSession(c echo.Context, runtime *runtimes.Runtime, workerID string, wait int) error {
	filter := taskqueue.ClaimFilter{Kind: taskqueue.KindDeviceSession, WorkspaceID: runtime.WorkspaceID,
		RuntimeID: runtime.ID, IdentityKind: taskqueue.IdentityDevice}
	deadline := time.Now().Add(time.Duration(wait) * time.Second)
	for {
		task, err := s.Tasks.Claim(c.Request().Context(), workerID, filter)
		if err != nil {
			if errors.Is(err, taskqueue.ErrRuntimeClaimUnavailable) {
				return c.JSON(http.StatusUnauthorized, map[string]string{"error": "invalid runtime token"})
			}
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
		if task != nil {
			var payload deviceSessionPayload
			_ = json.Unmarshal(task.Payload, &payload)
			lease := time.Now()
			if task.LeaseExpiresAt != nil {
				lease = *task.LeaseExpiresAt
			}
			return c.JSON(http.StatusOK, runtimeprotocol.ClaimResponse{Versioned: runtimeprotocol.NewVersioned(),
				DeviceSession: &runtimeprotocol.DeviceSessionClaim{TaskID: task.ID, ClaimEpoch: task.ClaimEpoch, Subject: task.Subject,
					LeaseExpiresAt: lease, Reason: payload.Reason, OpenedBy: payload.OpenedBy}})
		}
		if !time.Now().Before(deadline) {
			return c.NoContent(http.StatusNoContent)
		}
		select {
		case <-c.Request().Context().Done():
			return c.Request().Context().Err()
		case <-time.After(time.Second):
		}
	}
}

// deviceRuntimeSession loads the session the calling device holds.
func (s *Server) deviceRuntimeSession(c echo.Context, epoch int64) (*taskqueue.Task, string, error) {
	runtime, task, err := s.claimedRuntimeTask(c)
	if err != nil {
		return nil, "", err
	}
	if task.Kind != taskqueue.KindDeviceSession || runtime.DeviceOwnerUserID == "" || epoch != task.ClaimEpoch {
		return nil, "", echo.NewHTTPError(http.StatusConflict, "not this device's session")
	}
	return task, runtimes.RuntimeWorkerID(runtime.WorkspaceID, runtime.ID), nil
}

func (s *Server) handleDeviceNext(c echo.Context) error {
	var request runtimeprotocol.DeviceNextRequest
	if err := c.Bind(&request); err != nil || request.Versioned.Validate() != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}
	task, _, err := s.deviceRuntimeSession(c, request.ClaimEpoch)
	if err != nil {
		return err
	}
	wait := min(max(request.WaitSeconds, 0), deviceNextWaitMax)
	ctx := c.Request().Context()
	deadline := time.Now().Add(time.Duration(wait) * time.Second)
	for {
		if fresh, err := s.Tasks.Get(ctx, task.WorkspaceID, task.ID); err == nil {
			task = s.settleDeviceSession(ctx, fresh)
		}
		if task.Status != taskqueue.StatusRunning {
			return c.JSON(http.StatusOK, runtimeprotocol.DeviceNextResponse{Versioned: runtimeprotocol.NewVersioned(), Ended: true})
		}
		op, err := s.Runtimes.NextDeviceOperation(ctx, task.ID)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "device operation unavailable"})
		}
		if op != nil {
			return c.JSON(http.StatusOK, runtimeprotocol.DeviceNextResponse{Versioned: runtimeprotocol.NewVersioned(),
				Operation: &runtimeprotocol.DeviceOperation{OpID: op.OpID, Kind: op.Kind, Params: op.Params}})
		}
		if !time.Now().Before(deadline) {
			return c.JSON(http.StatusOK, runtimeprotocol.DeviceNextResponse{Versioned: runtimeprotocol.NewVersioned()})
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(devicePollInterval):
		}
	}
}

func (s *Server) handleDeviceOperationResult(c echo.Context) error {
	var request runtimeprotocol.DeviceOperationResult
	if err := c.Bind(&request); err != nil || request.Validate() != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}
	task, _, err := s.deviceRuntimeSession(c, request.ClaimEpoch)
	if err != nil {
		return err
	}
	err = s.Runtimes.CompleteDeviceOperation(c.Request().Context(), task.ID, c.Param("op"), request.Status, request.Result, request.Error, request.Audit)
	if errors.Is(err, runtimes.ErrDeviceOperationNotFound) {
		return c.JSON(http.StatusConflict, map[string]string{"error": "operation is not awaiting a result"})
	}
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "device result unavailable"})
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Server) handleDeviceSessionEnd(c echo.Context) error {
	var request runtimeprotocol.DeviceSessionEnd
	if err := c.Bind(&request); err != nil || request.Versioned.Validate() != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}
	task, workerID, err := s.deviceRuntimeSession(c, request.ClaimEpoch)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	reason := strings.TrimSpace(request.Reason)
	if reason == "" {
		reason = "ended_on_device"
	}
	_ = s.Runtimes.RecordDeviceSessionEnd(ctx, task.WorkspaceID, task.ID, reason)
	if task.Status == taskqueue.StatusCancelRequested {
		err = s.Tasks.AcknowledgeExecutionStopped(ctx, task.ID, workerID)
	} else {
		result, _ := json.Marshal(map[string]string{"reason": reason})
		err = s.Tasks.CompleteClaimed(ctx, task.ID, workerID, result, "")
	}
	if err != nil {
		return c.JSON(http.StatusConflict, map[string]string{"error": "session is no longer held"})
	}
	_ = s.Runtimes.EndDeviceOperations(ctx, task.ID)
	return c.NoContent(http.StatusNoContent)
}
