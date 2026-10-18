package loomruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/base/storeext"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
)

func upgradeCacheReceipt(t *testing.T) (UsageAccumulator, string, contract.Usage) {
	t.Helper()
	a := NewUsageAccumulator()
	call, err := a.NextCall("synthetic-upgrade-run", "worker")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.StartAttempt(call, "physical-attempt"); err != nil {
		t.Fatal(err)
	}
	u := contract.Usage{InputTokens: 100, OutputTokens: 7, CacheReadTokens: 80, CacheWriteTokens: 10}
	if err := a.ConfirmAttempt(call, "physical-attempt", u, 1); err != nil {
		t.Fatal(err)
	}
	return a, call, u
}

func TestLoomUpgradeUsageCheckpointPreservesPhysicalReceipt(t *testing.T) {
	a, _, want := upgradeCacheReceipt(t)
	encoded, err := a.MarshalCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := UnmarshalUsageAccumulator(encoded)
	if err != nil {
		t.Fatal(err)
	}
	receipts := restored.ConfirmedReceipts()
	if len(receipts) != 1 || receipts[0].Usage != want {
		t.Fatalf("checkpoint changed confirmed physical receipt: got=%+v want=%+v", receipts, want)
	}
}

func TestLoomUpgradeUsageReceiptReplayAfterCheckpointIsIdempotent(t *testing.T) {
	a, call, original := upgradeCacheReceipt(t)
	encoded, err := a.MarshalCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := UnmarshalUsageAccumulator(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.ConfirmAttempt(call, "physical-attempt", original, 1); err != nil {
		t.Fatalf("identical physical usage receipt conflicts after restore: %v", err)
	}
	if restored.Totals().InputTokens != 100 {
		t.Fatalf("receipt replay counted cache or input twice: %+v", restored.Totals())
	}
}

type upgradeCacheModel struct{ inner contract.LLM }

func (m upgradeCacheModel) Chat(ctx context.Context, request contract.ChatRequest) (*contract.ChatResponse, error) {
	response, err := m.inner.Chat(ctx, request)
	if err == nil && response != nil && response.Usage.InputTokens > 0 {
		response.Usage.CacheReadTokens = 1
	}
	return response, err
}

func (m upgradeCacheModel) Stream(ctx context.Context, request contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	return m.inner.Stream(ctx, request)
}

func upgradeCachedMemberGraph(h *memberPGHarness) *loom.Graph {
	opts := InstallFrozenMemberJournal(InstallFrozenUsageTracking(compiler.FrozenBuildOpts{LLM: upgradeCacheModel{inner: h.model}, Tools: h.tools}))
	graph := loom.NewGraph("workspace:member", "chat", loom.WithCheckpointPolicy(loom.CheckpointRequired), loom.WithCheckpointHistory(-1))
	graph.SetHooks(loom.HookPoints{Before: opts.Hooks.BeforeStepHooks, After: opts.Hooks.AfterStepHooks})
	graph.AddStep("chat", stdlib.NewToolLoopStep(opts.ExecutionLLMWrapper(opts.LLM), opts.Tools, stdlib.ToolLoopOpts{Model: "fixture", MaxIterations: 3}), loom.End())
	return graph
}

func TestLoomUpgradeCachedMemberReceiptResumesRealPG(t *testing.T) {
	h := newMemberPGHarness(t)
	h.request.Graph = upgradeCachedMemberGraph(h)
	runner, err := NewMemberRunner(h.records)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h.records.afterToolReceipt = cancel
	if _, err := runner.Run(ctx, h.request); err == nil {
		t.Fatal("interruption was not injected")
	}
	if h.tools.calls.Load() != 1 || h.model.calls.Load() != 1 {
		t.Fatalf("wrong interruption boundary: tools=%d models=%d", h.tools.calls.Load(), h.model.calls.Load())
	}
	next := h.nextEpoch(t)
	next.Graph = upgradeCachedMemberGraph(h)
	restarted, err := NewMemberRunner(storeext.New(h.pool))
	if err != nil {
		t.Fatal(err)
	}
	result, err := restarted.Run(t.Context(), next)
	if err != nil {
		t.Fatalf("cached model receipt prevented actual PG member recovery: %v", err)
	}
	if result.StopReason != loom.StopCompleted || h.tools.calls.Load() != 2 || h.model.calls.Load() != 2 {
		t.Fatalf("recovery changed actual execution: stop=%v tools=%d models=%d", result.StopReason, h.tools.calls.Load(), h.model.calls.Load())
	}
}

func TestLoomUpgradeMemberPersistsCacheSubtotalsRealPG(t *testing.T) {
	h := newMemberPGHarness(t)
	h.request.Graph = upgradeCachedMemberGraph(h)
	runner, err := NewMemberRunner(h.records)
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(t.Context(), h.request)
	if err != nil {
		t.Fatal(err)
	}
	accumulator, err := LoadUsageAccumulator(result.State)
	if err != nil {
		t.Fatal(err)
	}
	cacheRead := 0
	for _, receipt := range accumulator.ConfirmedReceipts() {
		cacheRead += receipt.Usage.CacheReadTokens
	}
	if cacheRead != 2 {
		t.Fatalf("actual PG member lost cache subtotals: got=%d want=2 from two physical model calls", cacheRead)
	}
}

func TestCacheUsagePresenceVersionsAndConflict(t *testing.T) {
	for _, known := range []bool{false, true} {
		a := NewUsageAccumulator()
		id, err := a.NextCall("presence-run", "step")
		if err != nil {
			t.Fatal(err)
		}
		if err := a.StartAttempt(id, "attempt"); err != nil {
			t.Fatal(err)
		}
		u := contract.Usage{InputTokens: 100, CacheReadReported: known, CacheWriteReported: known}
		if err := a.ConfirmAttempt(id, "attempt", u, 0); err != nil {
			t.Fatal(err)
		}
		encoded, err := a.MarshalCheckpoint()
		if err != nil {
			t.Fatal(err)
		}
		var header struct {
			Schema int `json:"schema_version"`
		}
		if err := json.Unmarshal(encoded, &header); err != nil {
			t.Fatal(err)
		}
		wantVersion := 2
		if known {
			wantVersion = 3
		}
		if header.Schema != wantVersion {
			t.Fatalf("version=%d want=%d", header.Schema, wantVersion)
		}
		restored, err := UnmarshalUsageAccumulator(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if restored.Coverage().HasCacheRead != known || restored.Coverage().HasCacheWrite != known {
			t.Fatal("unknown cache became known zero")
		}
		if err := restored.ConfirmAttempt(id, "attempt", u, 0); err != nil {
			t.Fatal(err)
		}
		changed := u
		changed.CacheReadTokens = 1
		if err := restored.ConfirmAttempt(id, "attempt", changed, 0); !errors.Is(err, ErrUsageConflict) {
			t.Fatalf("changed receipt = %v", err)
		}
		if known {
			invalid := bytes.Replace(encoded, []byte(`"schema_version":3`), []byte(`"schema_version":2`), 1)
			if _, err := UnmarshalUsageAccumulator(invalid); err == nil {
				t.Fatal("cache fields disguised as old schema accepted")
			}
		}
	}
}

func TestCacheUsageTerminalSummaryRetainsSubtotalsAndCoverage(t *testing.T) {
	a, _, _ := upgradeCacheReceipt(t)
	state := loom.State{}
	if err := StoreUsageAccumulator(state, a); err != nil {
		t.Fatal(err)
	}
	exclusive, err := validatedTerminalExclusiveUsage(state)
	if err != nil {
		t.Fatal(err)
	}
	if exclusive.InputTokens != 100 || exclusive.CacheReadTokens != 80 || exclusive.CacheWriteTokens != 10 || !exclusive.CacheReadComplete || !exclusive.CacheWriteComplete {
		t.Fatalf("terminal exclusive = %+v", exclusive)
	}
	total, err := addTerminalUsage(exclusive, exclusive)
	if err != nil || total.InputTokens != 200 || total.CacheReadTokens != 160 || total.CacheWriteTokens != 20 || !total.CacheReadComplete {
		t.Fatalf("terminal aggregate = %+v %v", total, err)
	}
	partial, err := addTerminalUsage(exclusive, TerminalUsage{InputTokens: 100})
	if err != nil || partial.CacheReadTokens != 80 || partial.CacheReadComplete || partial.CacheWriteComplete {
		t.Fatalf("missing coverage became complete = %+v %v", partial, err)
	}
}
