package compiler

import (
	"testing"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

// A member's frozen thinking switch, temperature and top_p become the options
// of each of its model calls; an unset one stays unset.
func TestStandardFrozenMemberSamplingReachesTheCall(t *testing.T) {
	compile := func(profile *frozen.FrozenRequestProfile) CompileOpts {
		t.Helper()
		bundle := frozen.FrozenExecutionBundle{Agent: frozen.FrozenAgentRecord{AgentVersion: 1, RequestProfile: profile}}
		_, opts, err := mapStandardFrozenToLegacy(bundle, FrozenBuildOpts{})
		if err != nil {
			t.Fatal(err)
		}
		return opts
	}
	temperature, topP := 0.0, 0.8
	opts := compile(&frozen.FrozenRequestProfile{Thinking: "disabled", Temperature: &temperature, TopP: &topP})
	if opts.Thinking != contract.ThinkingDisabled || opts.Temperature == nil || *opts.Temperature != 0 || opts.TopP == nil || *opts.TopP != 0.8 {
		t.Fatalf("thinking %q temperature %v top_p %v", opts.Thinking, opts.Temperature, opts.TopP)
	}
	if opts := compile(&frozen.FrozenRequestProfile{ReasoningEffort: "low"}); opts.Thinking != "" || opts.Temperature != nil || opts.TopP != nil {
		t.Fatalf("unset settings were filled in: %+v", opts)
	}
}
