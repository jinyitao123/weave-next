package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/loomadapter"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	"github.com/jinyitao123/weave/internal/kernel/runtimeagent"
	"github.com/jinyitao123/weave/internal/kernel/runtimeprotocol"
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
func (d *service) executeLoomTask(ctx context.Context, task *runtimeprotocol.ExecutionClaim, request runtimeprotocol.ExecutionRequest) (runtimeprotocol.ExecutionReceipt, error) {
	ctx, err := execution.BindSubject(ctx, task.Subject)
	if err != nil {
		return runtimeprotocol.ExecutionReceipt{}, err
	}
	ctx = withTaskProof(ctx, task.Subject, task.ClaimEpoch)
	logs := startTaskLogs(ctx, d.client, task)
	defer logs.close()
	mcpServers := 0
	if request.Loom != nil {
		mcpServers = request.Loom.MCPServerCount
	}
	result, err := loomadapter.Execute(ctx, daemonLoomAssembler{}, loomadapter.RunRequest{
		Claim: *task,
		Ports: loomadapter.Ports{
			Model: loggedLoomModel{next: newRuntimeLLMClient(d.client, task.TaskID), logs: logs},
			Tools: loggedLoomTools{next: newRuntimeToolDispatcher(d.client, task.TaskID, mcpServers), logs: logs},
		},
	})
	if err != nil {
		return runtimeprotocol.ExecutionReceipt{}, err
	}
	return runtimeprotocol.ExecutionReceipt{Versioned: runtimeprotocol.NewVersioned(), SchemaVersion: runtimeprotocol.ReceiptSchemaV1, TaskID: task.TaskID, ClaimEpoch: task.ClaimEpoch, Subject: task.Subject, Status: "completed", Output: result.Output, RunID: result.RunID, StopReason: result.StopReason, Usage: result.Usage}, nil
}

type daemonLoomAssembler struct{}

func (daemonLoomAssembler) Assemble(_ context.Context, claim runtimeprotocol.ExecutionClaim, ports loomadapter.Ports) (loomadapter.Graph, error) {
	request := claim.Request
	stamp, err := agentExecutionStampForTask(&claim, request)
	if err != nil {
		return nil, err
	}
	stamp.ExecutionScope = execution.ScopeLegacyOrchestrator
	stamp.LegacyScope = false
	record, err := runtimeagent.Decode(claim)
	if err != nil {
		return nil, err
	}
	loomInput := request.Loom
	if loomInput == nil {
		loomInput = &runtimeprotocol.LoomInput{}
	}
	messages := loomInput.Messages
	if len(messages) == 0 && loomInput.LastUserMessage != "" {
		messages = []contract.Message{{Role: "user", Content: loomInput.LastUserMessage}}
	}

	store := loom.NewMemStore()
	terminalAttribution, err := loomruntime.NewTerminalAttribution(
		loomruntime.TerminalAttributionInput{
			Scope:       loomruntime.TerminalAttributionLegacyUnattributed,
			WorkspaceID: claim.WorkspaceID,
		},
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("runtime: construct loom task terminal attribution: %w", err)
	}
	terminalSink, err := loomruntime.NewLineageTerminalSink(
		daemonTerminalRecordStore{store: store},
	)
	if err != nil {
		return nil, fmt.Errorf("runtime: construct loom task terminal sink: %w", err)
	}
	deps := loomruntime.Dependencies{
		LLM:          ports.Model,
		Tools:        ports.Tools,
		Store:        store,
		TerminalSink: terminalSink,
		CompileOpts: compiler.CompileOpts{
			Profile: loomInput.Profile,
			Effort:  loomInput.Effort,
			Context: loomInput.Context,
		},
	}

	prepared, err := loomruntime.Prepare(loomruntime.RunRequest{
		Tenant:              claim.WorkspaceID,
		Agent:               record,
		Stamp:               &stamp,
		TerminalAttribution: &terminalAttribution,
		Dependencies:        deps,
	})
	if err != nil {
		return nil, errors.New("runtime: loom task compilation failed: " + err.Error())
	}
	state := loomruntime.BuildState(claim.WorkspaceID, record, loomruntime.Input{
		Messages:        messages,
		LastUserMessage: loomInput.LastUserMessage,
		SessionID:       loomInput.SessionID,
		UserID:          claim.Subject.UserID,
		Profile:         loomInput.Profile,
		Context:         loomInput.Context,
	})
	return daemonLoomGraph{prepared: prepared, state: state}, nil
}

type daemonLoomGraph struct {
	prepared loomruntime.PreparedRun
	state    loom.State
}

func (graph daemonLoomGraph) Run(ctx context.Context) (loomadapter.RunResult, error) {
	result, err := graph.prepared.Run(ctx, graph.state)
	if err != nil {
		return loomadapter.RunResult{}, err
	}
	return loomadapter.RunResult{Output: result.Output, RunID: result.RunID, StopReason: string(result.StopReason), Usage: &result.Usage}, nil
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
