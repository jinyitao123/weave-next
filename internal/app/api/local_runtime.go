package api

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"

	"github.com/jinyitao123/weave/internal/app/daemon"
	"github.com/jinyitao123/weave/internal/app/localruntime"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/runtimeprotocol"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
)

// ConfigureRuntimeExecution is called once by the Server composition root,
// before any worker captures an executor. Getters never construct execution.
func (s *Server) ConfigureRuntimeExecution(ctx context.Context) (*localruntime.Manager, error) {
	if s.Config == nil || s.Tasks == nil || s.Runtimes == nil {
		return nil, errors.New("runtime execution assembly is incomplete")
	}
	remote := runtimes.NewExecutor(s.Tasks, s.Runtimes, s.Config.OneAPIBase, s.Config.OneAPIKey)
	s.RemoteExec = remote
	if !s.Config.LocalRuntimeEnabled {
		return nil, nil
	}
	host, err := localruntime.Start(ctx, localruntime.Config{Root: filepath.Join(s.Config.WorkspacesRoot, ".local-runtime"), Handler: s.Echo,
		Runtimes: s.Runtimes, Executor: remote, WorkspaceIDs: s.localRuntimeWorkspaces, Credentials: s.localRuntimeCredentials, Concurrency: 2})
	if err != nil {
		return nil, err
	}
	s.RemoteExec = host
	return host, nil
}

func (s *Server) localRuntimeWorkspaces(ctx context.Context) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id FROM weave_workspaces ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		result = append(result, id)
	}
	return result, rows.Err()
}

func (s *Server) localRuntimeCredentials(ctx context.Context, runtimeID string, claim *runtimeprotocol.ExecutionClaim) (daemon.ProviderCredentials, error) {
	if claim == nil || s.Models == nil {
		return daemon.ProviderCredentials{}, errors.New("runtime credentials unavailable")
	}
	task, err := s.Tasks.Get(ctx, claim.WorkspaceID, claim.TaskID)
	if err != nil {
		return daemon.ProviderCredentials{}, err
	}
	if task.RuntimeID != runtimeID || task.Subject != claim.Subject || task.ClaimEpoch != claim.ClaimEpoch {
		return daemon.ProviderCredentials{}, execution.ErrCurrentTaskMismatch
	}
	bound, err := taskqueue.BindTaskExecution(ctx, task, s.Tasks)
	if err != nil {
		return daemon.ProviderCredentials{}, err
	}
	tx, err := s.Pool.Begin(bound)
	if err != nil {
		return daemon.ProviderCredentials{}, err
	}
	defer tx.Rollback(bound)
	if err := s.Tasks.ValidateCurrentTaskTx(bound, tx); err != nil {
		return daemon.ProviderCredentials{}, err
	}
	if err := tx.Commit(bound); err != nil {
		return daemon.ProviderCredentials{}, err
	}
	var payload runtimes.EngineExecRequest
	if json.Unmarshal(task.Payload, &payload) != nil || payload.Model != claim.Request.Model || payload.Engine != claim.Request.Engine {
		return daemon.ProviderCredentials{}, execution.ErrCurrentTaskMismatch
	}
	// Operator configuration explicitly grants these workspace-service
	// providers to the managed Host. An absent grant never falls back to them.
	bound = frozen.WithServiceReferenceAuthorization(bound, func(_ context.Context, subject execution.Subject, ref frozen.CredentialReference) error {
		if subject == task.Subject && ref.WorkspaceID == task.WorkspaceID && ref.Kind == frozen.CredentialProviderAPIKey && slices.Contains(s.Config.LocalRuntimeServices, ref.ServiceID) {
			return nil
		}
		return errors.New("workspace service provider is not authorized for this runtime")
	})
	provider, err := s.Models.ResolveProviderConfiguration(bound, task.WorkspaceID, payload.Model)
	if err != nil {
		return daemon.ProviderCredentials{}, err
	}
	return daemon.ProviderCredentials{BaseURL: provider.BaseURL, APIKey: provider.APIKey}, nil
}
