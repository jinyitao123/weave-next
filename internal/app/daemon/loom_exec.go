package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
)

// executeLoomTask runs one loom turn in the daemon process. This is the edge
// half of loom-on-runtime: the model and every MCP tool are proxied back to the
// server (provider keys and upstream MCP endpoints never leave it), but the
// graph the daemon compiles is the same one the server would build from the
// frozen record — so an edge loom turn is shaped and governed identically to a
// server one.
//
// The daemon owns no persistence: a per-run in-memory store backs loom
// checkpoints and the process-local terminal record, and the turn's output is
// reported through the normal task-completion path. Skills must already be
// inlined in the record (there is no durable store here to resolve empty bodies
// from); the server bakes them in before enqueue.
func (d *service) executeLoomTask(ctx context.Context, task *taskqueue.Task, request runtimes.EngineExecRequest) (string, error) {
	stamp, err := agentExecutionStampForTask(task, request)
	if err != nil {
		return "", err
	}
	stamp.ExecutionScope = execution.ScopeLegacyOrchestrator
	stamp.LegacyScope = false
	loomInput := request.Loom
	if loomInput == nil {
		loomInput = &runtimes.LoomExecInput{}
	}
	messages := loomInput.Messages
	if len(messages) == 0 && loomInput.LastUserMessage != "" {
		messages = []contract.Message{{Role: "user", Content: loomInput.LastUserMessage}}
	}

	store := loom.NewMemStore()
	terminalAttribution, err := loomruntime.NewTerminalAttribution(
		loomruntime.TerminalAttributionInput{
			Scope:       loomruntime.TerminalAttributionLegacyUnattributed,
			WorkspaceID: task.WorkspaceID,
		},
		nil,
	)
	if err != nil {
		return "", fmt.Errorf("runtime: construct loom task terminal attribution: %w", err)
	}
	terminalSink, err := loomruntime.NewLineageTerminalSink(
		daemonTerminalRecordStore{store: store},
	)
	if err != nil {
		return "", fmt.Errorf("runtime: construct loom task terminal sink: %w", err)
	}
	deps := loomruntime.Dependencies{
		LLM:          newRuntimeLLMClient(d.client, task.ID),
		Tools:        newRuntimeToolDispatcher(d.client, task.ID, loomInput.MCPServerCount),
		Store:        store,
		TerminalSink: terminalSink,
		CompileOpts: compiler.CompileOpts{
			Profile: loomInput.Profile,
			Effort:  loomInput.Effort,
			Context: loomInput.Context,
		},
	}

	prepared, err := loomruntime.Prepare(loomruntime.RunRequest{
		Tenant:              task.WorkspaceID,
		Agent:               request.Record,
		Stamp:               &stamp,
		TerminalAttribution: &terminalAttribution,
		Dependencies:        deps,
	})
	if err != nil {
		return "", errors.New("runtime: loom task compilation failed: " + err.Error())
	}
	result, err := prepared.Run(ctx, loomruntime.BuildState(task.WorkspaceID, request.Record, loomruntime.Input{
		Messages:        messages,
		LastUserMessage: loomInput.LastUserMessage,
		SessionID:       loomInput.SessionID,
		UserID:          loomInput.UserID,
		Profile:         loomInput.Profile,
		Context:         loomInput.Context,
	}))
	if err != nil {
		return "", err
	}
	return result.Output, nil
}

type daemonTerminalRecordStore struct {
	store loom.Store
}

func (adapter daemonTerminalRecordStore) ReadValue(
	ctx context.Context,
	namespace string,
	key string,
) ([]byte, bool, error) {
	return readDaemonTerminalStoreValue(ctx, adapter.store, namespace, key)
}

func (adapter daemonTerminalRecordStore) ListKeys(
	ctx context.Context,
	namespace string,
) ([]string, error) {
	if adapter.store == nil {
		return nil, fmt.Errorf("terminal store is unavailable")
	}
	return adapter.store.List(ctx, namespace, "")
}

func (adapter daemonTerminalRecordStore) MutateValue(
	ctx context.Context,
	namespace string,
	key string,
	mutate func(current []byte, present bool) (next []byte, err error),
) error {
	if adapter.store == nil {
		return fmt.Errorf("terminal store is unavailable")
	}
	return adapter.store.Tx(ctx, func(tx loom.Store) error {
		current, present, err := readDaemonTerminalStoreValue(ctx, tx, namespace, key)
		if err != nil {
			return err
		}
		next, err := mutate(current, present)
		if err != nil {
			return err
		}
		if next == nil {
			return tx.Delete(ctx, namespace, key)
		}
		return tx.Put(ctx, namespace, key, next)
	})
}

func readDaemonTerminalStoreValue(
	ctx context.Context,
	store loom.Store,
	namespace string,
	key string,
) ([]byte, bool, error) {
	if store == nil {
		return nil, false, fmt.Errorf("terminal store is unavailable")
	}
	value, err := store.Get(ctx, namespace, key)
	if err == nil {
		return bytes.Clone(value), true, nil
	}
	keys, listErr := store.List(ctx, namespace, key)
	if listErr != nil {
		return nil, false, fmt.Errorf(
			"read terminal %q/%q: %v (verify absence: %w)",
			namespace,
			key,
			err,
			listErr,
		)
	}
	for _, listedKey := range keys {
		if listedKey == key {
			return nil, false, fmt.Errorf(
				"read existing terminal %q/%q: %w",
				namespace,
				key,
				err,
			)
		}
	}
	return nil, false, nil
}
