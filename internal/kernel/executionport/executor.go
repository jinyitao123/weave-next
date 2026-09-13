// Package executionport defines invocation ports for an already selected,
// authorized execution attempt. Implementations do not acquire authority here;
// platform admission and lifecycle ownership remain outside this contract.
package executionport

import (
	"context"
	"encoding/json"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/execspec"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

// RemoteEngineExecutor executes an already-resolved CLI agent record on its
// bound runtime.
type RemoteEngineExecutor interface {
	ExecRemote(
		ctx context.Context,
		tenant string,
		rec *registry.AgentRecord,
		stamp execution.AgentExecutionStamp,
		prompt string,
		attachments []execspec.Attachment,
	) (result engine.RunResult, err error)
}

// StructuredRemoteEngineExecutor is an optional extension for callers that
// require the CLI's final message to satisfy a JSON Schema. Ordinary worker
// dispatch keeps using RemoteEngineExecutor; protocol adapters opt in.
type StructuredRemoteEngineExecutor interface {
	ExecRemoteStructured(
		ctx context.Context,
		tenant string,
		rec *registry.AgentRecord,
		stamp execution.AgentExecutionStamp,
		prompt string,
		attachments []execspec.Attachment,
		outputSchema json.RawMessage,
	) (result engine.RunResult, err error)
}
