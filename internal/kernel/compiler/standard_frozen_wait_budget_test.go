package compiler

import (
	"testing"

	"github.com/jinyitao123/weave/internal/base/frozen"
)

// A member's frozen stage and call budgets become the wait budget of each of
// its model calls; no budget leaves the call unbounded but for the step.
func TestStandardFrozenMemberWaitBudgetReachesTheCall(t *testing.T) {
	compile := func(profile *frozen.FrozenRequestProfile) CompileOpts {
		t.Helper()
		bundle := frozen.FrozenExecutionBundle{Agent: frozen.FrozenAgentRecord{AgentVersion: 1, RequestProfile: profile}}
		_, opts, err := mapStandardFrozenToLegacy(bundle, FrozenBuildOpts{})
		if err != nil {
			t.Fatal(err)
		}
		return opts
	}
	opts := compile(&frozen.FrozenRequestProfile{FirstChunkTimeoutMS: 30000, IdleTimeoutMS: 15000, CallTimeoutMS: 120000})
	if opts.WaitBudget == nil || opts.WaitBudget.FirstChunkMS != 30000 || opts.WaitBudget.IdleMS != 15000 || opts.WaitBudget.TotalMS != 120000 {
		t.Fatalf("wait budget = %+v", opts.WaitBudget)
	}
	if opts := compile(&frozen.FrozenRequestProfile{CallTimeoutMS: 90000}); opts.WaitBudget == nil || opts.WaitBudget.TotalMS != 90000 || opts.WaitBudget.FirstChunkMS != 0 {
		t.Fatalf("call budget alone = %+v", opts.WaitBudget)
	}
	if opts := compile(&frozen.FrozenRequestProfile{ReasoningEffort: "low"}); opts.WaitBudget != nil {
		t.Fatalf("no budget set, yet wait budget = %+v", opts.WaitBudget)
	}
}
