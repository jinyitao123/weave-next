package api

import (
	"context"
	"errors"

	"github.com/jinyitao123/weave/internal/app/capabilities"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/capability"
	"github.com/jinyitao123/weave/internal/kernel/capabilityruntime"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
)

// capabilityRunner is composition only; selection and execution belong to the
// kernel, and invocation persistence belongs to the capabilities service.
func (s *Server) capabilityRunner() *capabilityruntime.Runner {
	runner := &capabilityruntime.Runner{
		Remote: s.engineExecutor(), Store: s.Store, TerminalSink: s.rootTerminalSink,
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

// ConfigureCapabilityTaskHandler binds the capability business projection to
// the shared platform worker. It must run after Tasks has been configured.
func (s *Server) ConfigureCapabilityTaskHandler() (taskqueue.Handler, error) {
	if s.Pool == nil || s.Tasks == nil {
		return nil, errors.New("capability platform queue is unavailable")
	}
	store := capabilities.NewPGStoreWithTaskQueue(s.Pool, s.Tasks)
	s.Capabilities = capabilities.NewService(store, store)
	return capabilities.PlatformTaskHandler{Store: store, Executor: capabilities.RuntimeTaskExecutor{
		Runner: s.capabilityRunner(), Store: store, DirectTools: s.capabilityDirectTools(store),
	}}, nil
}

func (s *Server) capabilityDirectTools(store *capabilities.PGStore) func(context.Context, capabilities.InvocationTask) (capability.ToolStepExecutor, error) {
	return func(ctx context.Context, task capabilities.InvocationTask) (capability.ToolStepExecutor, error) {
		if s.Pool == nil || s.MCPRegistry == nil {
			return nil, errors.New("capability MCP authority is unavailable")
		}
		toolIDs := make([]string, 0, len(task.Plan.Resources.Tools))
		for _, ref := range task.Plan.Resources.Tools {
			toolIDs = append(toolIDs, ref.ToolName)
		}
		authorizeContext := func(base context.Context) context.Context {
			return frozen.WithServiceReferenceAuthorization(base, func(_ context.Context, subject execution.Subject, ref frozen.CredentialReference) error {
				if subject.UserID == "" || subject.UserID != task.ActorUserID || subject.WorkspaceID != task.WorkspaceID {
					return frozen.ErrCredentialSubjectDenied
				}
				for _, binding := range task.ToolBindings {
					if binding.AccessRef == ref {
						return nil
					}
				}
				return frozen.ErrCredentialSubjectDenied
			})
		}
		validate := func(checkCtx context.Context, ref frozen.CredentialReference) error {
			checkCtx = authorizeContext(checkCtx)
			tx, err := s.Pool.Begin(checkCtx)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback(checkCtx) }()
			if err = s.MCPRegistry.ValidateReferenceTx(checkCtx, tx, ref); err != nil {
				return err
			}
			return tx.Commit(checkCtx)
		}
		resolve := func(resolveCtx context.Context, ref frozen.CredentialReference) (map[string]string, error) {
			resolveCtx = authorizeContext(resolveCtx)
			tx, err := s.Pool.Begin(resolveCtx)
			if err != nil {
				return nil, err
			}
			defer func() { _ = tx.Rollback(resolveCtx) }()
			material, err := s.MCPRegistry.ResolveMCPAccessTx(resolveCtx, tx, ref)
			if err != nil {
				return nil, err
			}
			if err = tx.Commit(resolveCtx); err != nil {
				return nil, err
			}
			headers := map[string]string{}
			for name, value := range material.Headers() {
				headers[name] = string(value)
			}
			return headers, nil
		}
		return capabilityruntime.NewDirectToolExecutor(capabilityruntime.DirectToolConfig{
			WorkspaceID: task.WorkspaceID, Agent: &registry.AgentRecord{WorkspaceID: task.WorkspaceID, ID: task.CapabilityID, Version: int(task.Revision), Name: task.CapabilityID},
			ToolIDs: toolIDs, Bindings: task.ToolBindings, AccessFactory: s.mcpAccessFactory(), ResolveAccess: resolve,
			ValidateAccess: validate, ValidateClaim: func(claimCtx context.Context) error {
				active, err := store.TaskActive(claimCtx, task)
				if err != nil {
					return err
				}
				if !active {
					return capabilities.ErrClaimLost
				}
				return nil
			},
		})
	}
}
