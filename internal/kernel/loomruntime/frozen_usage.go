package loomruntime

import (
	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
)

// InstallFrozenUsageTracking decorates frozen build options with the same
// logical usage tracking PreparedRun installs: a before-step binding hook
// plus the logical wrapper and the physical usage boundary. It is safe for
// graphs that already carry caller hooks and for nil LLMs (compile-time
// hosts), which keep their existing behavior.
func InstallFrozenUsageTracking(opts compiler.FrozenBuildOpts) compiler.FrozenBuildOpts {
	opts.Hooks.BeforeStepHooks = append(
		[]loom.StepHook{BindUsageBeforeStep},
		opts.Hooks.BeforeStepHooks...,
	)
	if opts.LLM != nil {
		opts.LLM = NewUsageBoundaryLLM(opts.LLM, NewRandomUsageAttemptID)
	}
	callerWrapper := opts.ExecutionLLMWrapper
	opts.ExecutionLLMWrapper = func(inner contract.LLM) contract.LLM {
		if callerWrapper != nil {
			inner = callerWrapper(inner)
		}
		return NewLogicalUsageLLM(inner)
	}
	return opts
}
