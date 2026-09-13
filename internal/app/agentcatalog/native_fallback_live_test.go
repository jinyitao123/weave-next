package agentcatalog_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/kernel/runtimes"

	"github.com/jinyitao123/weave/internal/app/agentcatalog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
)

// Explicit opt-in uses an already connected native worker and its authenticated
// provider. The requested output has no tool or filesystem side effect.
func TestNativeModelFallbackLive(t *testing.T) {
	if os.Getenv("WEAVE_LIVE_MODEL_FALLBACK_TEST") != "1" {
		t.Skip("native fallback live probe disabled")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, os.Getenv("WEAVE_LIVE_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	workspace := os.Getenv("WEAVE_LIVE_WORKSPACE")
	record, err := agentcatalog.New(pool).Get(ctx, workspace, os.Getenv("WEAVE_LIVE_AGENT"))
	if err != nil {
		t.Fatal(err)
	}
	record.Model = "weave-intentionally-unavailable-model"
	record.FallbackModels = []string{os.Getenv("WEAVE_LIVE_FALLBACK_MODEL")}
	record.FallbackRetries = 0
	record.RuntimeID = os.Getenv("WEAVE_LIVE_RUNTIME_ID")
	record.RuntimePolicyMode = "strict_pin"
	stamp := execution.AgentExecutionStamp{AgentID: record.ID, AgentVersion: record.Version, ExecutionScope: execution.ScopeLegacyOrchestrator}
	callCtx := execution.WithInvocationID(ctx, "native-fallback-proof/"+uuid.NewString())
	executor := runtimes.NewExecutor(taskqueue.New(pool, nil, time.Minute), runtimes.NewStore(pool), "", "")
	result, err := executor.ExecRemote(callCtx, workspace, record, stamp, "Reply exactly WEAVE_NATIVE_FALLBACK_OK. Do not use any tools, access files, or perform any other action.", nil)
	t.Logf("status=%s output=%q attempts=%d reported_models=%v error=%v", result.Status, result.Output, len(result.Attempts), result.ReportedModels, err)
	for _, attempt := range result.Attempts {
		t.Logf("physical_attempt=%s", attempt.AttemptID)
	}
	if err != nil || !strings.Contains(result.Output, "WEAVE_NATIVE_FALLBACK_OK") || len(result.Attempts) != 2 {
		t.Fatal("native fallback did not complete the two-attempt proof")
	}
}
