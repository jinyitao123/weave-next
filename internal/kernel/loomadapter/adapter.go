// Package loomadapter defines the storage-free boundary for assembling and
// running a Loom graph on a runtime Host. Platform leases, journals, budgets,
// reconciliation and terminal state stay outside this package.
package loomadapter

import (
	"context"
	"errors"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/kernel/runtimeprotocol"
)

// Ports are the only effects available to a Host-side Loom graph. A platform
// may proxy both ports so model credentials and MCP endpoints remain server-side.
type Ports struct {
	Model contract.LLM
	Tools contract.ToolDispatcher
}

type RunRequest struct {
	Claim runtimeprotocol.ExecutionClaim
	Ports Ports
}

type RunResult struct {
	Output     string
	RunID      string
	StopReason string
	Usage      *contract.Usage
}

// Graph is one assembled, process-local Loom execution. It has no authority to
// claim, renew, retry, cancel or settle the platform task.
type Graph interface {
	Run(context.Context) (RunResult, error)
}

// Assembler turns the exact frozen Host claim into one Loom graph. Implementors
// may use an in-memory Loom checkpoint store; durable member journals remain a
// platform-side concern.
type Assembler interface {
	Assemble(context.Context, runtimeprotocol.ExecutionClaim, Ports) (Graph, error)
}

func Execute(ctx context.Context, assembler Assembler, request RunRequest) (RunResult, error) {
	if assembler == nil || request.Ports.Model == nil || request.Ports.Tools == nil {
		return RunResult{}, errors.New("loom adapter dependencies are incomplete")
	}
	if err := request.Claim.Validate(); err != nil {
		return RunResult{}, err
	}
	if request.Claim.Request.Engine != "loom" {
		return RunResult{}, errors.New("loom adapter received another engine")
	}
	graph, err := assembler.Assemble(ctx, request.Claim, request.Ports)
	if err != nil {
		return RunResult{}, err
	}
	if graph == nil {
		return RunResult{}, errors.New("loom adapter assembled no graph")
	}
	return graph.Run(ctx)
}
