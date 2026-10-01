package loomruntime

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"testing"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/storeext"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
)

type authorizationRefusingTools struct {
	inner   *memberTestTools
	renewed bool
	slots   []string
}

func (tools *authorizationRefusingTools) ListTools(ctx context.Context) ([]contract.ToolDef, error) {
	return tools.inner.ListTools(ctx)
}
func (tools *authorizationRefusingTools) Dispatch(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	tools.slots = append(tools.slots, execution.OperationID(ctx))
	if !tools.renewed {
		return nil, execution.NewAuthorizationRefusal("original-input", 1, errors.New("native authority expired before dispatch"))
	}
	return tools.inner.Dispatch(ctx, call)
}
func TestMemberAuthorizationRefusalResumesSameSlotOnlyAfterRenewalRealPG(t *testing.T) {
	h := newMemberPGHarness(t)
	tools := &authorizationRefusingTools{inner: h.tools}
	graph := func() *loom.Graph {
		opts := InstallFrozenMemberJournal(InstallFrozenUsageTracking(compiler.FrozenBuildOpts{LLM: h.model, Tools: tools}))
		g := loom.NewGraph("workspace:member", "chat", loom.WithCheckpointPolicy(loom.CheckpointRequired), loom.WithCheckpointHistory(-1))
		g.SetHooks(loom.HookPoints{Before: opts.Hooks.BeforeStepHooks, After: opts.Hooks.AfterStepHooks})
		g.AddStep("chat", stdlib.NewToolLoopStep(opts.ExecutionLLMWrapper(opts.LLM), opts.Tools, stdlib.ToolLoopOpts{Model: "fixture", MaxIterations: 3}), loom.End())
		return g
	}
	h.request.Graph = graph()
	h.request.RetryableFailure = func(err error) bool { _, found := execution.AuthorizationRefusalFromError(err); return found }
	runner, _ := NewMemberRunner(h.records)
	_, err := runner.Run(t.Context(), h.request)
	proof, trusted := execution.AuthorizationRefusalFromError(err)
	if !trusted || !proof.NoEffect || errors.Is(err, ErrMemberOutcomeUnknown) || h.tools.calls.Load() != 0 {
		t.Fatalf("safe refusal was lost or effect occurred: proof=%+v err=%v effects=%d", proof, err, h.tools.calls.Load())
	}
	firstSlot := tools.slots[0]
	var raw []byte
	if err := h.pool.QueryRow(t.Context(), `SELECT value FROM loom_store WHERE namespace='member-operation:workspace' AND key=$1`, firstSlot).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var operation memberOperation
	if json.Unmarshal(raw, &operation) != nil || operation.AuthorizationRefusal == nil || len(operation.Response) > 0 {
		t.Fatal("no-effect proof did not persist at original tool intent")
	}
	denied := h.nextEpoch(t)
	denied.Graph = graph()
	denied.RetryableFailure = h.request.RetryableFailure
	deniedCtx := execution.WithAuthorizationRetryAuthorizer(t.Context(), func(context.Context, execution.AuthorizationRefusal) (bool, error) { return false, nil })
	fresh, _ := NewMemberRunner(storeext.New(h.pool))
	_, err = fresh.Run(deniedCtx, denied)
	if _, trusted := execution.AuthorizationRefusalFromError(err); !trusted || h.tools.calls.Load() != 0 || len(tools.slots) != 1 {
		t.Fatalf("unrenewed intent was dispatched: err=%v effects=%d slots=%v", err, h.tools.calls.Load(), tools.slots)
	}
	tools.renewed = true
	accepted := h.nextEpoch(t)
	accepted.ParentGeneration = 3
	if _, err := h.pool.Exec(t.Context(), `UPDATE member_test_parent SET epoch=3`); err != nil {
		t.Fatal(err)
	}
	accepted.ParentGuard = func(ctx context.Context, tx pgx.Tx) error {
		var epoch int
		if err := tx.QueryRow(ctx, `SELECT epoch FROM member_test_parent FOR UPDATE`).Scan(&epoch); err != nil {
			return err
		}
		if epoch != 3 {
			return ErrAttemptLeaseOwnerConflict
		}
		return nil
	}
	accepted.Graph = graph()
	accepted.RetryableFailure = h.request.RetryableFailure
	acceptedCtx := execution.WithAuthorizationRetryAuthorizer(t.Context(), func(_ context.Context, proof execution.AuthorizationRefusal) (bool, error) {
		return proof.InputRevisionID == "original-input" && proof.Generation == 1 && tools.renewed, nil
	})
	fresh, _ = NewMemberRunner(storeext.New(h.pool))
	result, err := fresh.Run(acceptedCtx, accepted)
	if err != nil || result == nil || result.StopReason != loom.StopCompleted || h.tools.calls.Load() != 2 || h.model.calls.Load() != 2 {
		t.Fatalf("renewed original member did not complete exactly once: result=%+v err=%v effects=%d models=%d", result, err, h.tools.calls.Load(), h.model.calls.Load())
	}
	if len(tools.slots) != 3 || tools.slots[0] != firstSlot || tools.slots[1] != firstSlot || tools.slots[2] == firstSlot {
		t.Fatalf("renewal lost original operation identity or next intent: %v", tools.slots)
	}
}

type nonRenewableAuthorizationTools struct {
	inner  *memberTestTools
	denied bool
	slots  []string
}

func (tools *nonRenewableAuthorizationTools) ListTools(ctx context.Context) ([]contract.ToolDef, error) {
	return tools.inner.ListTools(ctx)
}
func (tools *nonRenewableAuthorizationTools) Dispatch(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	tools.slots = append(tools.slots, execution.OperationID(ctx))
	if tools.denied {
		return nil, execution.NewAuthorizationDenialBeforeDispatch("original-input", 1, "FORGE_TASK_ORGANIZATION_FORBIDDEN", errors.New("Forge refused organization authorization"))
	}
	return tools.inner.Dispatch(ctx, call)
}

func TestMemberOrganizationDenialCannotRenewOrRedispatchRealPG(t *testing.T) {
	h := newMemberPGHarness(t)
	tools := &nonRenewableAuthorizationTools{inner: h.tools, denied: true}
	graph := func() *loom.Graph {
		opts := InstallFrozenMemberJournal(InstallFrozenUsageTracking(compiler.FrozenBuildOpts{LLM: h.model, Tools: tools}))
		g := loom.NewGraph("workspace:member", "chat", loom.WithCheckpointPolicy(loom.CheckpointRequired), loom.WithCheckpointHistory(-1))
		g.SetHooks(loom.HookPoints{Before: opts.Hooks.BeforeStepHooks, After: opts.Hooks.AfterStepHooks})
		g.AddStep("chat", stdlib.NewToolLoopStep(opts.ExecutionLLMWrapper(opts.LLM), opts.Tools, stdlib.ToolLoopOpts{Model: "fixture", MaxIterations: 3}), loom.End())
		return g
	}
	h.request.Graph = graph()
	h.request.RetryableFailure = func(err error) bool {
		proof, found := execution.AuthorizationRefusalFromError(err)
		return found && proof.Renewable()
	}
	runner, _ := NewMemberRunner(h.records)
	_, err := runner.Run(t.Context(), h.request)
	proof, found := execution.AuthorizationRefusalFromError(err)
	if !found || proof.Renewable() || proof.ReasonCode != "FORGE_TASK_ORGANIZATION_FORBIDDEN" || errors.Is(err, ErrMemberOutcomeUnknown) || h.tools.calls.Load() != 0 || len(tools.slots) != 1 {
		t.Fatalf("organization denial was not retained as a terminal no-effect proof: proof=%+v found=%v err=%v effects=%d slots=%v", proof, found, err, h.tools.calls.Load(), tools.slots)
	}
	tools.denied = false
	authorizerCalls := 0
	ctx := execution.WithAuthorizationRetryAuthorizer(t.Context(), func(context.Context, execution.AuthorizationRefusal) (bool, error) {
		authorizerCalls++
		return true, nil
	})
	next := h.nextEpoch(t)
	next.Graph = graph()
	next.RetryableFailure = h.request.RetryableFailure
	fresh, _ := NewMemberRunner(storeext.New(h.pool))
	_, err = fresh.Run(ctx, next)
	if replayed, trusted := execution.AuthorizationRefusalFromError(err); !trusted || replayed != proof || replayed.Renewable() || authorizerCalls != 0 || h.tools.calls.Load() != 0 || len(tools.slots) != 1 {
		t.Fatalf("a later authorization change renewed a denied operation: proof=%+v trusted=%v err=%v authorizer_calls=%d effects=%d slots=%v", replayed, trusted, err, authorizerCalls, h.tools.calls.Load(), tools.slots)
	}
}
