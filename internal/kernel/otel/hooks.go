package otel

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
)

// TraceEntry is a single step execution record persisted for run detail views.
type TraceEntry struct {
	Step       string  `json:"step"`
	DurationMs int64   `json:"duration_ms"`
	TokensIn   int     `json:"tokens_in"`
	TokensOut  int     `json:"tokens_out"`
	CostUSD    float64 `json:"cost_usd"`
	Timestamp  string  `json:"timestamp"`
}

// spanEntry tracks an active step span.
type spanEntry struct {
	Tenant  string
	Step    string
	RunID   string
	StartAt time.Time
}

// activeSpans stores spans that TraceEnd needs to close.
// Key: "{run_id}:{step_name}"
var activeSpans sync.Map

func spanKey(runID, step string) string {
	return runID + ":" + step
}

// TraceStart creates a Before hook that records step start time and metadata.
func TraceStart(tenant string) loom.StepHook {
	return func(_ context.Context, step string, state loom.State) error {
		runID := stdlib.GetString(state, "__run_id", "unknown")
		activeSpans.Store(spanKey(runID, step), &spanEntry{
			Tenant:  tenant,
			Step:    step,
			RunID:   runID,
			StartAt: time.Now(),
		})
		slog.Debug("step.start",
			"tenant", tenant,
			"step", step,
			"run_id", runID,
		)
		return nil
	}
}

// TraceEnd creates an After hook that logs step completion with duration and usage.
// If store is non-nil, it also persists per-step trace entries for the run detail view.
func TraceEnd(store loom.Store) loom.StepHook {
	return func(ctx context.Context, step string, state loom.State) error {
		runID := stdlib.GetString(state, "__run_id", "unknown")
		key := spanKey(runID, step)

		raw, ok := activeSpans.LoadAndDelete(key)
		if !ok {
			return nil
		}
		entry := raw.(*spanEntry)
		duration := time.Since(entry.StartAt)

		var usage contract.Usage
		if u, ok := state["usage"].(contract.Usage); ok {
			usage = u
		}

		fields := []any{
			"tenant", entry.Tenant,
			"step", step,
			"run_id", runID,
			"duration_ms", duration.Milliseconds(),
		}
		if usage.InputTokens > 0 {
			fields = append(fields,
				"tokens_in", usage.InputTokens,
				"tokens_out", usage.OutputTokens,
				"cost_usd", usage.CostUSD,
			)
		}
		slog.Info("step.end", fields...)

		// Persist trace entry to store for run detail visualization.
		if store != nil {
			te := TraceEntry{
				Step:       step,
				DurationMs: duration.Milliseconds(),
				TokensIn:   usage.InputTokens,
				TokensOut:  usage.OutputTokens,
				CostUSD:    usage.CostUSD,
				Timestamp:  time.Now().Format(time.RFC3339),
			}
			data, err := json.Marshal(te)
			if err == nil {
				traceNS := "trace:" + entry.Tenant + ":" + runID
				traceKey := fmt.Sprintf("%d_%s", time.Now().UnixMilli(), step)
				if putErr := store.Put(ctx, traceNS, traceKey, data); putErr != nil {
					slog.Warn("trace write failed", "error", putErr)
				}
			}
		}

		return nil
	}
}

// AuditHook remains as a compatibility hook for compiled graphs. Terminal
// audit persistence is owned by loomruntime.RecordRunTerminal after Graph.Run.
func AuditHook(_ loom.Store, _ string, _ string) loom.StepHook {
	return func(context.Context, string, loom.State) error { return nil }
}
