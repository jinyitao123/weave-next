package loomruntime

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
)

func memberControlledGraph(h *memberPGHarness, total uint64) *loom.Graph {
	opts := InstallFrozenMemberJournal(InstallFrozenUsageTracking(compiler.FrozenBuildOpts{LLM: h.model, Tools: h.tools}))
	graph := loom.NewGraph("workspace:member", "chat", loom.WithCheckpointPolicy(loom.CheckpointRequired), loom.WithCheckpointHistory(-1))
	graph.SetHooks(loom.HookPoints{Before: opts.Hooks.BeforeStepHooks, After: opts.Hooks.AfterStepHooks})
	graph.AddStep("chat", stdlib.NewToolLoopStep(opts.ExecutionLLMWrapper(opts.LLM), opts.Tools, stdlib.ToolLoopOpts{Model: "fixture", MaxIterations: 1, Control: &stdlib.ToolLoopControl{ID: "chat", InitialTotalRounds: total}}), loom.End())
	return graph
}

func TestMemberControlledBudgetGrantRealPG(t *testing.T) {
	for _, total := range []uint64{1, 2} {
		t.Run(string(rune('0'+total)), func(t *testing.T) {
			h := newMemberPGHarness(t)
			h.request.Graph = memberControlledGraph(h, total)
			runner, err := NewMemberRunner(h.records)
			if err != nil {
				t.Fatal(err)
			}
			result, err := runner.Run(t.Context(), h.request)
			if err != nil {
				t.Fatal(err)
			}
			pause, err := ReadMemberBudgetPause(result, h.request.Graph.Name)
			if err != nil || pause == nil || pause.RoundsUsed != 1 || pause.AuthorizedTotalRounds != total {
				t.Fatalf("pause=%+v err=%v", pause, err)
			}
			if h.tools.calls.Load() != 2 || h.model.calls.Load() != 1 {
				t.Fatal("unexpected initial effects")
			}
			var terminal []byte
			if err := h.pool.QueryRow(t.Context(), `SELECT result FROM weave_workflow_member_runs`).Scan(&terminal); err != nil || len(terminal) > 0 {
				t.Fatalf("pause cached as terminal: %v", err)
			}
			next := h.nextEpoch(t)
			next.Graph = memberControlledGraph(h, total)
			repeated, err := runner.Run(t.Context(), next)
			if err != nil {
				t.Fatal(err)
			}
			same, err := ReadMemberBudgetPause(repeated, next.Graph.Name)
			if err != nil || *same != *pause {
				t.Fatal("bare resume moved pause")
			}
			if h.tools.calls.Load() != 2 || h.model.calls.Load() != 1 {
				t.Fatal("bare resume gained budget")
			}
			tx, err := h.pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			progress, progressErr := MemberSliceProgressTx(t.Context(), tx, next.WorkspaceID, *pause)
			if progressErr != nil || progress {
				t.Fatalf("ordinary tool output treated as automatic progress: %v %v", progress, progressErr)
			}
			if err := next.ParentGuard(t.Context(), tx); err != nil {
				t.Fatal(err)
			}
			if err := AuthorizeMemberBudgetTx(t.Context(), tx, next.WorkspaceID, next.ParentRunID, next.CallID, "grant-1", *pause, 2); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			next.ResumeGrantID = "grant-1"
			// Failure while saving the new segment must roll back grant consumption.
			h.records.failHistory = true
			if _, err := runner.Run(t.Context(), next); err == nil {
				t.Fatal("checkpoint failure ignored")
			}
			var consumed int
			if err := h.pool.QueryRow(t.Context(), `SELECT count(*) FROM loom_store WHERE namespace=$1`, budgetGrantNS(next.WorkspaceID)+":consumed").Scan(&consumed); err != nil || consumed != 0 {
				t.Fatal("grant consumed without segment")
			}
			if h.model.calls.Load() != 1 {
				t.Fatal("effect before grant transaction")
			}
			h.records.failHistory = false
			h.request = next
			next = h.nextEpoch(t)
			next.Graph = memberControlledGraph(h, total)
			result, err = runner.Run(t.Context(), next)
			if err != nil {
				t.Fatal(err)
			}
			outcome, present, err := stdlib.ReadToolLoopOutcome(result.State)
			if err != nil || !present || result.StopReason != loom.StopCompleted || outcome.TotalRoundsUsed != 2 || outcome.AuthorizedTotalRounds != 2 || outcome.Slice != 2 {
				t.Fatalf("completion=%+v err=%v stop=%v", outcome, err, result.StopReason)
			}
			if h.model.calls.Load() != 2 || h.tools.calls.Load() != 2 {
				t.Fatal("continuation replayed effects")
			}
			usage, err := LoadUsageAccumulator(result.State)
			if err != nil {
				t.Fatal(err)
			}
			if usage.Totals().InputTokens != 6 || usage.Totals().OutputTokens != 8 {
				t.Fatalf("duplicated/lost usage: %+v", usage.Totals())
			}
			_, err = runner.Run(t.Context(), next)
			if err != nil {
				t.Fatal(err)
			}
			if h.model.calls.Load() != 2 || h.tools.calls.Load() != 2 {
				t.Fatal("cached completion repeated effects")
			}
			var raw []byte
			if err := h.pool.QueryRow(t.Context(), `SELECT value FROM loom_store WHERE namespace=$1 AND key=$2`, budgetGrantNS(next.WorkspaceID)+":consumed", budgetGrantKey(result.RunID, "grant-1")).Scan(&raw); err != nil || !json.Valid(raw) {
				t.Fatal("missing grant receipt")
			}
		})
	}
}

func TestMemberControlledCrashReplaysSameSliceRealPG(t *testing.T) {
	h := newMemberPGHarness(t)
	h.request.Graph = memberControlledGraph(h, 2)
	runner, err := NewMemberRunner(h.records)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h.records.afterToolReceipt = cancel
	if _, err := runner.Run(ctx, h.request); err == nil {
		t.Fatal("expected interruption")
	}
	next := h.nextEpoch(t)
	next.Graph = memberControlledGraph(h, 2)
	result, err := runner.Run(t.Context(), next)
	if err != nil {
		t.Fatal(err)
	}
	pause, err := ReadMemberBudgetPause(result, next.Graph.Name)
	if err != nil || pause == nil || pause.Slice != 1 || pause.RoundsUsed != 1 {
		t.Fatalf("pause=%+v err=%v", pause, err)
	}
	if h.model.calls.Load() != 1 || h.tools.calls.Load() != 2 {
		t.Fatalf("repeated effects model=%d tool=%d", h.model.calls.Load(), h.tools.calls.Load())
	}
}

func TestMemberConsumedGrantCrashKeepsNewSliceRealPG(t *testing.T) {
	h := newMemberPGHarness(t)
	h.model.exhaust = true
	h.request.Graph = memberControlledGraph(h, 3)
	runner, err := NewMemberRunner(h.records)
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(t.Context(), h.request)
	if err != nil {
		t.Fatal(err)
	}
	pause, err := ReadMemberBudgetPause(result, h.request.Graph.Name)
	if err != nil {
		t.Fatal(err)
	}
	next := h.nextEpoch(t)
	next.Graph = memberControlledGraph(h, 3)
	next.ResumeGrantID = "only-once"
	tx, err := h.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := next.ParentGuard(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	if err := AuthorizeMemberBudgetTx(t.Context(), tx, next.WorkspaceID, next.ParentRunID, next.CallID, next.ResumeGrantID, *pause, 3); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h.records.afterToolReceipt = cancel
	if _, err := runner.Run(ctx, next); err == nil {
		t.Fatal("expected interrupted second slice")
	}
	if h.model.calls.Load() != 2 || h.tools.calls.Load() != 3 {
		t.Fatal("unexpected interrupted effects")
	}
	h.request = next
	next = h.nextEpoch(t)
	next.Graph = memberControlledGraph(h, 3)
	result, err = runner.Run(t.Context(), next)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ReadMemberBudgetPause(result, next.Graph.Name)
	if err != nil || second == nil || second.Slice != 2 || second.RoundsUsed != 2 {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	if h.model.calls.Load() != 2 || h.tools.calls.Load() != 4 {
		t.Fatal("second slice replay repeated effects")
	}
	h.request = next
	next = h.nextEpoch(t)
	next.Graph = memberControlledGraph(h, 3)
	result, err = runner.Run(t.Context(), next)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := ReadMemberBudgetPause(result, next.Graph.Name)
	if err != nil || *repeated != *second || h.model.calls.Load() != 2 || h.tools.calls.Load() != 4 {
		t.Fatal("consumed grant granted another slice")
	}
}
