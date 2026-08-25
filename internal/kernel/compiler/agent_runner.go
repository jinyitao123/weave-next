package compiler

import (
	"context"

	"github.com/jinyitao123/loom"
)

// SubAgentStepResolver resolves a child to a host-managed execution step.
type SubAgentStepResolver func(tenant, agentName string) (loom.Step, error)

// AgentRunResult is the portable result of running an agent by name.
type AgentRunResult struct {
	Output    string
	Yielded   bool
	YieldType string
	RunID     string
}

// AgentRunner runs a registered agent by name.
type AgentRunner interface {
	Run(ctx context.Context, agentName, message string) (AgentRunResult, error)
}
