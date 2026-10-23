package api

import (
	"context"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/execspec"
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
func (s *Server) compilerAgentRunner(
	tenant, userID string,
	llm contract.LLM,
	attachments []execspec.Attachment,
) compiler.AgentRunner {
	broker := mcphost.NewToolBroker(s.mcpAccessFactory())
	runner := mcphost.NewAgentRunner(
		s.Registry, tenant, llm, s.Store,
		attachments,
	)
	runner.Broker = broker
	runner.RemoteExec = s.engineExecutor()
	runner.RunLifecycleHook = s.RunLifecycleHook
	runner.SkillVersionReader = s.Skills
	if s.Fanout != nil && s.Tasks != nil {
		runner.InnerPlatformTools = func(innerRec *registry.AgentRecord) []contract.ToolDispatcher {
			return []contract.ToolDispatcher{s.subAgentTaskStatusDispatcher(tenant, innerRec.Name, userID)}
		}
	}
	return mcphost.NewCompilerAgentRunner(runner)
}
