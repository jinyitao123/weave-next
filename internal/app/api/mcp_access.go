package api

import (
	"context"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/mcphost"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

func (s *Server) buildAuditedMCPDispatcher(
	rec *registry.AgentRecord,
	tenant, conversationID string,
) contract.ToolDispatcher {
	dispatcher, err := s.mcpAccessFactory().Build(
		context.Background(), tenant, rec, conversationID,
	)
	if err != nil {
		return mcphost.NewRejectedMCPDispatcher(err)
	}
	return dispatcher
}

func (s *Server) mcpAccessFactory() *mcphost.MCPAccessFactory {
	var auditRecorder mcphost.GovernanceAuditRecorder
	if s.Audit != nil {
		auditRecorder = s.Audit
	}
	return mcphost.NewMCPAccessFactory(
		s.agentMCPResolver(), auditRecorder,
	)
}

// compilerAgentRunner runs the members a declarative graph's worker steps name.
func (s *Server) compilerAgentRunner(tenant string, llm contract.LLM) compiler.AgentRunner {
	broker := mcphost.NewToolBroker(s.mcpAccessFactory())
	runner := mcphost.NewAgentRunner(s.Registry, tenant, llm, s.Store, nil)
	runner.Broker = broker
	runner.RemoteExec = s.engineExecutor()
	runner.RunLifecycleHook = s.RunLifecycleHook
	runner.SkillVersionReader = s.Skills
	return mcphost.NewCompilerAgentRunner(runner)
}
