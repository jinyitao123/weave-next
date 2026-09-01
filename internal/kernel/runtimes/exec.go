package runtimes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/execenv"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

// Keep the engine task ceiling aligned with the teamrun agent-node ceiling.
// The caller may still cancel earlier, while a healthy CLI task gets the full
// bounded window promised by teamrun.
const engineExecTimeout = 20 * time.Minute

// Executor relays one external CLI engine execution through a bound runtime.
type Executor struct {
	tasks      *taskqueue.Store
	runtimes   *Store
	oneapiBase string
	oneapiKey  string
}

// NewExecutor creates a remote engine executor backed by the runtime and task stores.
func NewExecutor(tasks *taskqueue.Store, runtimes *Store, oneapiBase, oneapiKey string) *Executor {
	return &Executor{
		tasks:      tasks,
		runtimes:   runtimes,
		oneapiBase: oneapiBase,
		oneapiKey:  oneapiKey,
	}
}

// ExecRemote enqueues an engine_exec task for an online runtime and waits for
// its terminal result. rec must already contain any resolved skill bodies.
func (e *Executor) ExecRemote(
	ctx context.Context,
	tenant string,
	rec *registry.AgentRecord,
	stamp execution.AgentExecutionStamp,
	prompt string,
	attachments []execenv.Attachment,
) (engine.RunResult, error) {
	return e.execRemote(ctx, tenant, rec, stamp, prompt, attachments, nil)
}

// ExecRemoteStructured executes one remote CLI turn with a schema-constrained
// final message. Runtime-backed LLM adapters use this to make their envelope
// deterministic; ordinary CLI workers keep the historical free-text path.
func (e *Executor) ExecRemoteStructured(
	ctx context.Context,
	tenant string,
	rec *registry.AgentRecord,
	stamp execution.AgentExecutionStamp,
	prompt string,
	attachments []execenv.Attachment,
	outputSchema json.RawMessage,
) (engine.RunResult, error) {
	if len(outputSchema) == 0 || !json.Valid(outputSchema) {
		return engine.RunResult{}, errors.New("remote engine executor: valid output schema is required")
	}
	return e.execRemote(ctx, tenant, rec, stamp, prompt, attachments, outputSchema)
}

func (e *Executor) execRemote(
	ctx context.Context,
	tenant string,
	rec *registry.AgentRecord,
	stamp execution.AgentExecutionStamp,
	prompt string,
	attachments []execenv.Attachment,
	outputSchema json.RawMessage,
) (engine.RunResult, error) {
	if err := validateAgentExecutionStamp(tenant, rec, stamp); err != nil {
		return engine.RunResult{}, fmt.Errorf("remote engine executor: %w", err)
	}
	if !engine.IsCLIEngine(rec.Engine) {
		return engine.RunResult{}, errors.New("员工远程执行仅支持 claude、codex 或 opencode，未入队")
	}
	if e == nil || e.runtimes == nil {
		return engine.RunResult{}, errors.New("员工所在运行时不存在，未入队")
	}
	if e.tasks == nil {
		return engine.RunResult{}, errors.New("remote engine executor: task queue is unavailable")
	}

	// Frozen workflow workers predate the policy field. Explicit Pool
	// membership is the administrator's opt-in for those records; a record on
	// an unpooled runtime preserves the historical single-attempt behavior.
	if rec.RuntimePolicyMode == "" {
		selected, getErr := e.runtimes.Get(ctx, tenant, rec.RuntimeID)
		if getErr != nil || selected.PoolID == "" {
			result, taskID, err := e.execRemoteAttempt(ctx, tenant, rec, stamp, prompt, attachments, outputSchema, "", "")
			result.Attempts = appendUsageAttempt(result.Attempts, taskID, rec.RuntimeID, rec.Engine, result)
			return result, err
		}
		resolved := *rec
		resolved.RuntimePolicyMode = "engine_pool"
		resolved.RuntimePoolID = selected.PoolID
		rec = &resolved
	}

	candidates, err := e.runtimeCandidates(ctx, tenant, rec)
	if err != nil {
		return engine.RunResult{}, err
	}
	traceID := "runtime-attempts-" + uuid.NewString()
	parentTaskID := ""
	var lastErr error
	var lastResult engine.RunResult
	var attempts []engine.UsageAttempt
	for _, runtimeID := range candidates {
		for sameRuntimeAttempt := 0; sameRuntimeAttempt < 2; sameRuntimeAttempt++ {
			attemptRecord := *rec
			attemptRecord.RuntimeID = runtimeID
			result, taskID, attemptErr := e.execRemoteAttempt(
				ctx, tenant, &attemptRecord, stamp, prompt, attachments, outputSchema, traceID, parentTaskID,
			)
			attempts = appendUsageAttempt(attempts, taskID, runtimeID, rec.Engine, result)
			result.Attempts = append([]engine.UsageAttempt(nil), attempts...)
			if attemptErr == nil {
				_ = e.runtimes.RecordExecutionSuccess(context.WithoutCancel(ctx), tenant, runtimeID)
				return result, nil
			}
			lastResult = result
			lastErr = attemptErr
			if taskID != "" {
				parentTaskID = taskID
			}
			if ctx.Err() != nil || !retryableRuntimeFailure(attemptErr) {
				return result, attemptErr
			}
			_ = e.runtimes.RecordInfrastructureFailure(
				context.WithoutCancel(ctx), tenant, runtimeID, attemptErr.Error(),
			)
		}
	}
	if rec.RuntimePolicyMode == "strict_pin" {
		return lastResult, fmt.Errorf("runtime_pinned_unavailable: %w", lastErr)
	}
	if rec.RuntimePolicyMode == "engine_pool" {
		return lastResult, fmt.Errorf("runtime_pool_exhausted: %w", lastErr)
	}
	return lastResult, fmt.Errorf("runtime_temporarily_unavailable: %w", lastErr)
}

func appendUsageAttempt(attempts []engine.UsageAttempt, taskID, runtimeID, engineName string, result engine.RunResult) []engine.UsageAttempt {
	if taskID == "" {
		return attempts
	}
	for _, attempt := range attempts {
		if attempt.AttemptID == taskID {
			return attempts
		}
	}
	return append(attempts, engine.UsageAttempt{
		AttemptID: taskID, RuntimeID: runtimeID, Engine: engineName,
		Status: result.Status, Usage: result.Usage,
		Diagnostics: append([]engine.Diagnostic(nil), result.Diagnostics...),
		Events:      append([]engine.Event(nil), result.Events...),
	})
}

func (e *Executor) runtimeCandidates(ctx context.Context, tenant string, rec *registry.AgentRecord) ([]string, error) {
	candidates := []string{rec.RuntimeID}
	if rec.RuntimePolicyMode != "engine_pool" || strings.TrimSpace(rec.RuntimePoolID) == "" {
		return candidates, nil
	}
	stored, err := e.runtimes.List(ctx, tenant)
	if err != nil {
		return nil, fmt.Errorf("list runtime pool candidates: %w", err)
	}
	sort.SliceStable(stored, func(i, j int) bool {
		leftHealthy := stored[i].HealthStatus == "healthy"
		rightHealthy := stored[j].HealthStatus == "healthy"
		if leftHealthy != rightHealthy {
			return leftHealthy
		}
		leftFree := stored[i].TotalSlots - stored[i].ActiveSlots
		rightFree := stored[j].TotalSlots - stored[j].ActiveSlots
		if leftFree != rightFree {
			return leftFree > rightFree
		}
		return stored[i].ID < stored[j].ID
	})
	for _, runtime := range stored {
		if len(candidates) >= 3 {
			break
		}
		if runtime.ID == rec.RuntimeID || runtime.PoolID != rec.RuntimePoolID ||
			!runtime.Online || runtime.HealthStatus == "quarantined" ||
			!slices.Contains(runtime.Engines, rec.Engine) {
			continue
		}
		candidates = append(candidates, runtime.ID)
	}
	return candidates, nil
}

func retryableRuntimeFailure(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{
		" 502", " 503", " 504", "status 502", "status 503", "status 504",
		"service unavailable", "temporarily unavailable", "rate limit", "too many requests",
		"connection reset", "connection refused", "broken pipe", "unexpected eof",
		"stream disconnected", "upstream request failed", "tls handshake eof",
		"transport error", "network error", "error decoding response body",
		"deadline exceeded", "timed out", "timeout", "lease lost",
		"failed to start", "spawn", "process exited", "runtime offline", "运行时离线",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func (e *Executor) execRemoteAttempt(
	ctx context.Context,
	tenant string,
	rec *registry.AgentRecord,
	stamp execution.AgentExecutionStamp,
	prompt string,
	attachments []execenv.Attachment,
	outputSchema json.RawMessage,
	traceID, parentTaskID string,
) (engine.RunResult, string, error) {
	tx, err := e.runtimes.pool.Begin(ctx)
	if err != nil {
		return engine.RunResult{}, "", fmt.Errorf("begin runtime admission: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var enginesJSON, capabilitiesJSON []byte
	var lastHeartbeatAt *time.Time
	err = tx.QueryRow(ctx, `
		SELECT engines, engine_capabilities, last_heartbeat_at
		FROM weave_runtimes
		WHERE workspace_id=$1 AND id=$2
		  AND enabled=true AND revoked_at IS NULL AND deleted_at IS NULL
		FOR SHARE
	`, tenant, rec.RuntimeID).Scan(&enginesJSON, &capabilitiesJSON, &lastHeartbeatAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return engine.RunResult{}, "", errors.New("员工所在运行时不存在，未入队")
	}
	if err != nil {
		return engine.RunResult{}, "", fmt.Errorf("lock runtime admission: %w", err)
	}
	if lastHeartbeatAt == nil || lastHeartbeatAt.Before(e.runtimes.now().Add(-onlineWindow)) {
		return engine.RunResult{}, "", errors.New("员工所在运行时离线，未入队")
	}
	var advertisedEngines []string
	if err := json.Unmarshal(enginesJSON, &advertisedEngines); err != nil {
		return engine.RunResult{}, "", fmt.Errorf("decode runtime engines: %w", err)
	}
	if !slices.Contains(advertisedEngines, rec.Engine) {
		return engine.RunResult{}, "", fmt.Errorf("员工所在运行时未上报引擎 %q，未入队", rec.Engine)
	}
	var capabilities map[string]EngineCapability
	if err := json.Unmarshal(capabilitiesJSON, &capabilities); err != nil {
		return engine.RunResult{}, "", fmt.Errorf("decode runtime engine capabilities: %w", err)
	}
	capability, exists := capabilities[rec.Engine]
	if !exists || strings.TrimSpace(capability.BinaryVersion) == "" {
		return engine.RunResult{}, "", fmt.Errorf("员工所在运行时未上报引擎 %q 版本，未入队", rec.Engine)
	}

	payload := e.buildExecPayloadWithSchema(tenant, rec, prompt, attachments, outputSchema)
	payload.EngineVersion = capability.BinaryVersion
	encoded, err := json.Marshal(payload)
	if err != nil {
		return engine.RunResult{}, "", fmt.Errorf("encode remote engine task: %w", err)
	}
	task := &taskqueue.Task{
		ID:                    "task-" + uuid.NewString(),
		WorkspaceID:           tenant,
		Agent:                 rec.Name,
		AgentID:               stamp.AgentID,
		AgentVersion:          stamp.AgentVersion,
		IdentityKind:          taskqueue.IdentityAgent,
		IdentitySchemaVersion: 2,
		ExecutionScope:        stamp.ExecutionScope,
		RunSnapshotID:         stamp.RunSnapshotID,
		Source:                "dispatch",
		Kind:                  "engine_exec",
		RuntimeID:             rec.RuntimeID,
		TraceID:               traceID,
		ParentTaskID:          parentTaskID,
		Payload:               encoded,
	}
	if traceID != "" {
		task.RuntimeAssignment, _ = json.Marshal(map[string]any{
			"attempt_id": task.ID, "parent_attempt_id": parentTaskID,
			"runtime_id": rec.RuntimeID, "engine": rec.Engine,
			"mode": rec.RuntimePolicyMode, "pool_id": rec.RuntimePoolID,
		})
	}
	if err := e.tasks.EnqueueTx(ctx, tx, task); err != nil {
		return engine.RunResult{}, "", fmt.Errorf("enqueue remote engine task: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return engine.RunResult{}, "", fmt.Errorf("commit remote engine task: %w", err)
	}
	terminal, err := e.tasks.AwaitTerminal(ctx, tenant, task.ID, engineExecTimeout)
	if err != nil {
		// The caller owns the business-node lifetime. Once that context or the
		// bounded wait ends, leaving the engine task runnable creates an orphan:
		// a later candidate attempt may execute beside work whose parent TeamRun
		// is already terminal. Cancelling the durable task also makes the daemon's
		// next renew return lease-lost, which terminates the CLI child process.
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = e.tasks.Cancel(cleanupCtx, tenant, task.ID)
		cleanupCancel()
		return engine.RunResult{}, task.ID, err
	}
	if terminal.Status != taskqueue.StatusCompleted {
		if terminal.Error != "" {
			return engine.RunResult{}, task.ID, errors.New(terminal.Error)
		}
		return engine.RunResult{}, task.ID, fmt.Errorf("remote engine task ended with status %q", terminal.Status)
	}
	var result EngineExecResult
	if err := json.Unmarshal(terminal.Result, &result); err != nil {
		return engine.RunResult{}, task.ID, fmt.Errorf("decode remote engine result: %w", err)
	}
	engineResult := result.EngineRunResult()
	if engineResult.Usage != nil && engineResult.Usage.EngineVersion != payload.EngineVersion {
		return engine.RunResult{}, task.ID, errors.New("remote engine usage receipt version does not match admitted binary")
	}
	if engineResult.Status != "completed" {
		message := engineResult.Err
		if message == "" {
			message = fmt.Sprintf("engine run ended with status %q", engineResult.Status)
		}
		return engineResult, task.ID, errors.New(message)
	}
	return engineResult, task.ID, nil
}

func validateAgentExecutionStamp(
	tenant string,
	rec *registry.AgentRecord,
	stamp execution.AgentExecutionStamp,
) error {
	if tenant == "" {
		return errors.New("tenant is required")
	}
	if rec == nil {
		return errors.New("agent record is required")
	}
	if rec.Name == "" || rec.ID == "" || rec.Version < 1 || rec.WorkspaceID == "" {
		return errors.New("agent record requires exact workspace, name, ID, and version")
	}
	if rec.WorkspaceID != tenant {
		return errors.New("agent record workspace does not match tenant")
	}
	if stamp.AgentID == "" || stamp.AgentVersion < 1 || !stamp.ExecutionScope.Valid() {
		return errors.New("invalid agent execution stamp")
	}
	if stamp.LegacyScope && stamp.ExecutionScope != execution.ScopeLegacyOrchestrator {
		return errors.New("legacy execution stamp must use legacy orchestrator scope")
	}
	if stamp.AgentID != rec.ID || stamp.AgentVersion != rec.Version {
		return errors.New("agent execution stamp does not match agent record")
	}
	return nil
}

func (e *Executor) buildExecPayloadWithSchema(
	tenant string,
	rec *registry.AgentRecord,
	prompt string,
	attachments []execenv.Attachment,
	outputSchema json.RawMessage,
) EngineExecRequest {
	if CanonicalEngine(rec.Engine) == EngineLoom {
		return EngineExecRequest{
			Agent:          rec.Name,
			Engine:         EngineLoom,
			Model:          rec.Model,
			Record:         rec,
			TimeoutSeconds: int(engineExecTimeout / time.Second),
			Loom: &LoomExecInput{
				Messages:        []contract.Message{{Role: "user", Content: prompt}},
				LastUserMessage: prompt,
			},
		}
	}
	return EngineExecRequest{
		Agent:          rec.Name,
		Engine:         CanonicalEngine(rec.Engine),
		Model:          rec.Model,
		Prompt:         promptWithAttachmentNotice(prompt, attachments),
		OutputSchema:   append(json.RawMessage(nil), outputSchema...),
		Record:         rec,
		TimeoutSeconds: int(engineExecTimeout / time.Second),
		Attachments:    engineExecAttachments(attachments),
	}
}

func engineExecAttachments(attachments []execenv.Attachment) []EngineExecAttachment {
	if len(attachments) == 0 {
		return nil
	}
	result := make([]EngineExecAttachment, 0, len(attachments))
	for _, attachment := range attachments {
		result = append(result, EngineExecAttachment{
			ID:       filepath.Base(filepath.Dir(filepath.Clean(attachment.Path))),
			Filename: attachment.Filename,
		})
	}
	return result
}

func promptWithAttachmentNotice(prompt string, attachments []execenv.Attachment) string {
	if len(attachments) == 0 {
		return prompt
	}
	var notice strings.Builder
	notice.WriteString("\n\n已上传附件(在工作目录 attachments/ 下):")
	for _, attachment := range attachments {
		fmt.Fprintf(&notice, "\n- attachments/%s", attachment.Filename)
	}
	return prompt + notice.String()
}
