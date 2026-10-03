package compiler

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
)

func TestFrozenBusinessCompletionPolicySurvivesPauseAndRejectsChangedScope(t *testing.T) {
	llm := &schemaTestLLM{responses: []contract.ChatResponse{{Content: `{"answer":"draft"}`}, {Content: `{"answer":"done"}`}}}
	step := nodeToolLoopStep(llm, &CompileGuardDispatcher{}, stdlib.ToolLoopOpts{Model: "test", MaxIterations: 1, Control: &stdlib.ToolLoopControl{ID: "chat", InitialTotalRounds: 2}})
	graph := loom.NewGraph(t.Name(), "chat", loom.WithMergeConfig(loom.DefaultMergeConfig()), loom.WithCheckpointPolicy(loom.CheckpointRequired))
	graph.AddStep("chat", step, loom.End())
	store := loom.NewMemStore()
	checks := 0
	check := func(context.Context, string) (bool, string, error) {
		checks++
		if checks == 1 {
			return false, "required receipt missing", nil
		}
		return true, "", nil
	}
	ctx := WithNodeCompletionCheck(WithNodeOutputSchema(t.Context(), outputTestSchema), "frozen-contract-and-scope-a", check)
	paused, err := graph.Run(ctx, schemaInput(), store)
	if err != nil || !paused.Yielded {
		t.Fatalf("pause=%#v %v", paused, err)
	}
	outcome, _, err := stdlib.ReadToolLoopOutcome(paused.State)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(paused.State["__checkpoint_seq"])
	var seq int64
	_ = json.Unmarshal(raw, &seq)
	delta, err := stdlib.PrepareToolLoopResume(paused.State, stdlib.ToolLoopResumeGrant{ID: "resume", ExpectedRunID: paused.RunID, ExpectedCheckpointSeq: seq, ExpectedYieldToken: paused.State["__yield_token"].(string), ExpectedSlice: outcome.Slice, AuthorizedTotalRounds: 2})
	if err != nil {
		t.Fatal(err)
	}
	changed := WithNodeCompletionCheck(ctx, "frozen-contract-and-scope-b", check)
	if _, err := graph.Resume(changed, paused.RunID, delta, store); err == nil {
		t.Fatal("different action requirement/scope resumed old checkpoint")
	}
	if len(llm.requests) != 1 || checks != 1 {
		t.Fatal("changed policy reached model or verifier")
	}
	completed, err := graph.Resume(ctx, paused.RunID, delta, store)
	if err != nil || completed.State["output"] != `{"answer":"done"}` {
		t.Fatalf("resume=%#v %v", completed, err)
	}
	if len(llm.requests) != 2 || checks != 2 || !strings.Contains(llm.requests[1].Messages[len(llm.requests[1].Messages)-1].Content, "required receipt missing") {
		t.Fatal("same policy lost completion feedback or reran verified output")
	}
}
