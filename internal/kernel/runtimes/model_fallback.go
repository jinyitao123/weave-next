package runtimes

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/execspec"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

func (e *Executor) execRemote(ctx context.Context, tenant string, rec *registry.AgentRecord, stamp execution.AgentExecutionStamp, prompt string, attachments []execspec.Attachment, schema json.RawMessage) (engine.RunResult, error) {
	if rec == nil {
		return engine.RunResult{}, fmt.Errorf("agent record is required")
	}
	models := []string{rec.Model}
	for _, model := range rec.FallbackModels {
		if len(models) == 3 {
			break
		}
		if model != "" && !slices.Contains(models, model) {
			models = append(models, model)
		}
	}
	retries := max(0, min(2, rec.FallbackRetries))
	originalID := execution.InvocationID(ctx)
	root, _ := execution.AttemptLineage(ctx)
	if root == "" {
		root = execution.EngineTaskID(tenant, originalID)
	}
	var attempts []engine.UsageAttempt
	var result engine.RunResult
	var lastErr error
	parentAttemptID := ""
	ordinal := 0
	for _, model := range models {
		for repeat := 0; repeat <= retries; repeat++ {
			attemptCtx := ctx
			if ordinal > 0 && originalID != "" {
				attemptCtx = execution.WithInvocationID(ctx, fmt.Sprintf("%s/attempt-%d", originalID, ordinal))
			}
			attemptCtx = execution.WithAttemptLineage(attemptCtx, root, parentAttemptID)
			record := *rec
			record.Model = model
			result, lastErr = e.execRemoteOnce(attemptCtx, tenant, &record, stamp, prompt, attachments, schema)
			for _, attempt := range result.Attempts {
				if !slices.ContainsFunc(attempts, func(old engine.UsageAttempt) bool { return old.AttemptID == attempt.AttemptID }) {
					attempts = append(attempts, attempt)
				}
			}
			result.Attempts = append([]engine.UsageAttempt(nil), attempts...)
			if lastErr == nil {
				return result, nil
			}
			if ctx.Err() != nil || !safeModelRetryResult(result) || !modelRetryableFailure(lastErr) {
				return result, lastErr
			}
			if len(attempts) > 0 {
				parentAttemptID = attempts[len(attempts)-1].AttemptID
			}
			ordinal++
			if modelUnavailable(lastErr) {
				break
			}
		}
	}
	if len(models) > 1 || retries > 0 {
		return result, fmt.Errorf("runtime_model_fallback_exhausted: %w", lastErr)
	}
	return result, lastErr
}
func modelUnavailable(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "model") && (strings.Contains(message, "not supported") || strings.Contains(message, "not available") || strings.Contains(message, "not found") || strings.Contains(message, "does not exist")) {
		return true
	}
	for _, marker := range []string{"model not found", "model_not_found", "unsupported model", "model is not supported", "model is not available", "model does not exist", "invalid model"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}
func modelRetryableFailure(err error) bool {
	return modelUnavailable(err) || retryableRuntimeFailure(err)
}

func safeModelRetryResult(result engine.RunResult) bool {
	if !result.RetrySafeBeforeExecution || len(result.Artifacts) > 0 {
		return false
	}
	return !slices.ContainsFunc(result.Events, func(event engine.Event) bool { return event.Kind == "tool_call" || event.Kind == "tool_result" })
}
