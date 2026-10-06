package businessaction_test

import (
	"context"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/businessaction"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

// This opt-in cross-component regression uses only a short-lived task credential
// from an isolated native Forge test runtime. The handoff must not contain a
// desktop session, operator credential or production endpoint.
func TestNativeConfirmationForgeRuntimeInteropRealPG(t *testing.T) {
	path := os.Getenv("FORGE_NATIVE_CONFIRMATION_HANDOFF")
	if path == "" {
		t.Skip("isolated native Forge task handoff is not set")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("native task handoff must be a private 0600 file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("private native task handoff unavailable")
	}
	defer clear(raw)
	var handoff struct {
		Issuer       string                             `json:"issuer"`
		Token        string                             `json:"token"`
		Grant        businessaction.TaskDelegationGrant `json:"grant"`
		CapabilityID string                             `json:"capabilityId"`
		Params       map[string]any                     `json:"params"`
		Completion   string                             `json:"completion"`
	}
	if json.Unmarshal(raw, &handoff) != nil || handoff.Issuer == "" || handoff.Token == "" || handoff.CapabilityID == "" || handoff.Completion == "" || handoff.Grant.Scope.BusinessRecord == nil {
		t.Fatal("private native task handoff is incomplete")
	}
	endpoint, err := url.Parse(handoff.Issuer)
	if err != nil || (endpoint.Hostname() != "localhost" && endpoint.Hostname() != "127.0.0.1" && endpoint.Hostname() != "::1") {
		t.Fatal("native interop must use the isolated local Forge runtime")
	}
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	key := []byte(strings.Repeat("k", 32))
	token := []byte(handoff.Token)
	defer clear(token)
	seedMemberActionRuntime(t, pool, handoff.Issuer, key, handoff.Grant, token)
	ctx := confirmationTaskContext(t, pool)
	factory := businessaction.Factory{Inner: workflow.RuntimeHostFactoryFunc(func(context.Context, frozen.FrozenExecutionBundle, workflow.RuntimeCredentialResolver) (compiler.FrozenBuildOpts, io.Closer, error) {
		return compiler.FrozenBuildOpts{Tools: memberEmptyTools{}}, nil, nil
	}), Store: businessaction.NewStore(pool, taskqueue.New(pool, nil, time.Minute), key)}
	opts, _, err := factory.Build(ctx, frozen.FrozenExecutionBundle{Agent: frozen.FrozenAgentRecord{WorkspaceID: "workspace", AgentID: "agent", AgentVersion: 1, BusinessCapabilityIDs: []string{handoff.CapabilityID}}}, nil)
	if err != nil {
		t.Fatal("native task authority/catalog/protocol binding failed", err)
	}
	tools, err := opts.Tools.ListTools(ctx)
	if err != nil || len(tools) != 1 || strings.Contains(string(tools[0].InputSchema), "confirm") || strings.Contains(string(tools[0].InputSchema), "idempotency_key") {
		t.Fatal("native system fields were exposed to the model")
	}
	args, _ := json.Marshal(map[string]any{"params": handoff.Params})
	call := contract.ToolCall{ID: "native-interop", Name: tools[0].Name, Args: string(args)}
	result, err := opts.Tools.Dispatch(ctx, call)
	if err != nil || result == nil || result.IsError || result.StopLoop {
		t.Fatal("Weave production dispatcher did not obtain a native action success receipt", err)
	}
	call.ID = "native-interop-recovery"
	replayed, err := opts.Tools.Dispatch(ctx, call)
	if err != nil || replayed == nil || replayed.IsError || replayed.Content != businessaction.SanitizeActionOutcomeResult(result).Content {
		t.Fatal("native action receipt was not recovered from the original operation")
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM weave_team_run_activity_events WHERE kind='business_action_started'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("native receipt recovery created a second business operation")
	}
	completion, _ := json.Marshal(map[string]any{"complete": true, "ok": true, "operationStarts": count, "nativeReceiptRecovered": true})
	if os.WriteFile(handoff.Completion, completion, 0o600) != nil {
		t.Fatal("native runtime completion notice could not be saved privately")
	}
}
