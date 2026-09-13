package api

import (
	"context"

	"github.com/jinyitao123/weave/internal/app/capabilities"
	"github.com/jinyitao123/weave/internal/kernel/capabilityruntime"
)

// capabilityRunner is composition only; selection and execution belong to the
// kernel, and invocation persistence belongs to the capabilities service.
func (s *Server) capabilityRunner() *capabilityruntime.Runner {
	runner := &capabilityruntime.Runner{
		Remote: s.engineExecutorFor(true), Store: s.Store, TerminalSink: s.rootTerminalSink,
	}
	if s.Models != nil {
		runner.CanResolveModel = s.Models.CanResolve
		runner.ResolveModel = s.Models.ForWorkspace
	}
	if s.Runtimes != nil {
		runner.ListRuntimes = s.Runtimes.List
	}
	return runner
}

func (s *Server) serveCapabilityTasks(ctx context.Context) {
	store := capabilities.NewPGStore(s.Pool)
	capabilities.ServeTasks(ctx, store, capabilities.RuntimeTaskExecutor{Runner: s.capabilityRunner(), Store: store})
}
