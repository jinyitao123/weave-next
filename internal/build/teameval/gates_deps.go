package teameval

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
)

// resolveTx opens every evaluator transaction at repeatable read. The
// evaluator never issues writes (评测语境隔离); concurrency safety comes
// from the platform's FOR SHARE locking inside the resolvers
// (credentials.ResolveModelRevisionTx, registry share reads), exactly like
// the freeze pipeline. An explicit read-only access mode is deliberately
// avoided because PostgreSQL forbids SELECT ... FOR SHARE inside read-only
// transactions (SQLSTATE 25006).
var resolveTx = pgx.TxOptions{
	IsoLevel: pgx.RepeatableRead,
}

// gateDepsFreezable resolves every named dependency of the evaluated agents
// with the real platform resolvers: model bindings through
// credentials.ResolveModelRevisionTx, runtimes through runtimes.Store.Get,
// MCP servers through mcpregistry, and skills through the legacy skill
// namespace when not inlined into the agent version (plan §8.1).
func (e *GateEvaluator) gateDepsFreezable(ctx context.Context, ev EvaluatedTeam) GateResult {
	evidence := fmt.Sprintf("team=%s", ev.TeamID())
	if ev.Team == nil {
		return Fail(GateDepsFreezable, evidence, "team not found")
	}
	agents := append([]*registry.AgentRecord{}, ev.Lead)
	for _, worker := range ev.Workers {
		agents = append(agents, worker.Agent)
	}
	for _, record := range agents {
		if record == nil {
			continue
		}
		ref := fmt.Sprintf("%s agent=%s@%d", evidence, record.Name, record.Version)
		for _, model := range recordModels(record) {
			if err := e.resolveModel(ctx, ev.WorkspaceID(), model); err != nil {
				return Fail(
					GateDepsFreezable,
					fmt.Sprintf("%s model=%s", ref, model),
					"model binding is not freezable: "+err.Error(),
				)
			}
		}
		if record.RuntimeID != "" {
			runtime, err := e.resolveRuntime(ctx, ev.WorkspaceID(), record.RuntimeID, record.Engine)
			if err != nil {
				return Fail(
					GateDepsFreezable,
					fmt.Sprintf("%s runtime=%s", ref, record.RuntimeID),
					"runtime binding is not freezable: "+err.Error(),
				)
			}
			if runtime.FunctionalRevision < 1 {
				return Fail(
					GateDepsFreezable,
					fmt.Sprintf("%s runtime=%s", ref, record.RuntimeID),
					"runtime has no functional revision to freeze",
				)
			}
		}
		for _, server := range record.MCPServers {
			if server.ServerID == "" {
				return Fail(
					GateDepsFreezable,
					fmt.Sprintf("%s mcp=legacy:%s", ref, server.URL),
					"legacy inline MCP config is not freezable; reference a registry MCP server",
				)
			}
			view, err := e.deps.MCPs.Get(ctx, ev.WorkspaceID(), server.ServerID)
			if err != nil {
				return Fail(
					GateDepsFreezable,
					fmt.Sprintf("%s mcp=%s", ref, server.ServerID),
					"MCP server is not resolvable: "+err.Error(),
				)
			}
			if !view.Enabled || view.RevokedAt != nil || view.DeletedAt != nil ||
				view.FunctionalRevision < 1 {
				return Fail(
					GateDepsFreezable,
					fmt.Sprintf("%s mcp=%s", ref, server.ServerID),
					"MCP server is closed or has no functional revision",
				)
			}
		}
		for _, skill := range record.Spec.Skills {
			if strings.TrimSpace(skill.Body) != "" {
				continue // inlined into the agent version, frozen by construction
			}
			ok, err := e.skillInNamespace(ctx, ev.WorkspaceID(), skill.Name)
			if err != nil {
				return Fail(
					GateDepsFreezable,
					fmt.Sprintf("%s skill=%s", ref, skill.Name),
					"skill namespace read failed: "+err.Error(),
				)
			}
			if !ok {
				return Fail(
					GateDepsFreezable,
					fmt.Sprintf("%s skill=%s", ref, skill.Name),
					"skill is neither inlined nor present in the workspace skill namespace",
				)
			}
		}
	}
	return Pass(GateDepsFreezable, evidence, "every model, runtime, MCP, and skill reference resolves")
}

// gateDepsPinned checks every asset reference pins an exact version instead
// of resolving latest at run time (plan §8.1 / v2.2): workflow nodes must
// carry exact agent versions, runtimes and MCP servers must resolve to a
// functional revision, and skills must be inlined into the agent version
// rather than resolved live from the skill namespace.
func (e *GateEvaluator) gateDepsPinned(ctx context.Context, ev EvaluatedTeam) GateResult {
	evidence := fmt.Sprintf("team=%s", ev.TeamID())
	if ev.Team == nil {
		return Fail(GateDepsPinned, evidence, "team not found")
	}
	agents := append([]*registry.AgentRecord{}, ev.Lead)
	for _, worker := range ev.Workers {
		agents = append(agents, worker.Agent)
	}
	for _, record := range agents {
		if record == nil {
			continue
		}
		ref := fmt.Sprintf("%s agent=%s@%d", evidence, record.Name, record.Version)
		if record.RuntimeID != "" {
			runtime, err := e.deps.Runtimes.Get(ctx, ev.WorkspaceID(), record.RuntimeID)
			if err != nil {
				return Fail(
					GateDepsPinned,
					fmt.Sprintf("%s runtime=%s", ref, record.RuntimeID),
					"runtime reference is unresolvable: "+err.Error(),
				)
			}
			if runtime.FunctionalRevision < 1 {
				return Fail(
					GateDepsPinned,
					fmt.Sprintf("%s runtime=%s", ref, record.RuntimeID),
					"runtime reference has no pinned functional revision (resolves latest)",
				)
			}
		}
		for _, server := range record.MCPServers {
			if server.ServerID == "" {
				return Fail(
					GateDepsPinned,
					fmt.Sprintf("%s mcp=legacy:%s", ref, server.URL),
					"legacy inline MCP config is an unversioned (latest) reference",
				)
			}
			view, err := e.deps.MCPs.Get(ctx, ev.WorkspaceID(), server.ServerID)
			if err != nil {
				return Fail(
					GateDepsPinned,
					fmt.Sprintf("%s mcp=%s", ref, server.ServerID),
					"MCP reference is unresolvable: "+err.Error(),
				)
			}
			if view.FunctionalRevision < 1 {
				return Fail(
					GateDepsPinned,
					fmt.Sprintf("%s mcp=%s", ref, server.ServerID),
					"MCP reference has no pinned functional revision (resolves latest)",
				)
			}
		}
		for _, skill := range record.Spec.Skills {
			if strings.TrimSpace(skill.Body) == "" {
				return Fail(
					GateDepsPinned,
					fmt.Sprintf("%s skill=%s", ref, skill.Name),
					"skill reference has no inlined body and resolves latest from the live skill namespace",
				)
			}
		}
	}
	for _, wf := range ev.Workflows {
		if wf.Version == nil || wf.Graph.DecodeErr != "" {
			continue // reported by gate_graph_schema
		}
		for _, key := range referencedAgentKeys(wf.Graph.Graph) {
			if key.AgentVersion < 1 {
				return Fail(
					GateDepsPinned,
					fmt.Sprintf("%s workflow=%s agent=%s@%d",
						evidence, wf.Workflow.ID, key.AgentID, key.AgentVersion),
					"workflow references agent_version 0 (latest)",
				)
			}
		}
	}
	return Pass(GateDepsPinned, evidence, "every asset reference pins an exact version")
}

// resolveModel resolves one model to its exact provider revision inside a
// read-only transaction.
func (e *GateEvaluator) resolveModel(ctx context.Context, workspaceID, modelID string) error {
	if strings.TrimSpace(modelID) == "" {
		return nil
	}
	if e.deps.Pool == nil {
		return fmt.Errorf("model resolver transaction store is unavailable")
	}
	resolver := e.deps.Models
	if resolver == nil {
		return fmt.Errorf("model resolver is unavailable")
	}
	tx, err := e.deps.Pool.BeginTx(ctx, resolveTx)
	if err != nil {
		return fmt.Errorf("begin model resolution: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := resolver.ResolveModelRevisionTx(ctx, tx, workspaceID, modelID); err != nil {
		return err
	}
	return nil
}

// resolveRuntime reads one runtime and validates it is open and hosts the
// agent's engine.
func (e *GateEvaluator) resolveRuntime(
	ctx context.Context,
	workspaceID, runtimeID, agentEngine string,
) (*runtimes.Runtime, error) {
	if e.deps.Runtimes == nil {
		return nil, fmt.Errorf("runtime store is unavailable")
	}
	runtime, err := e.deps.Runtimes.Get(ctx, workspaceID, runtimeID)
	if err != nil {
		return nil, err
	}
	if runtime == nil || !runtime.Enabled || runtime.RevokedAt != nil || runtime.DeletedAt != nil {
		return nil, fmt.Errorf("runtime is closed")
	}
	effectiveEngine := agentEngine
	if effectiveEngine == "" {
		effectiveEngine = "loom"
	}
	if !stringInSlice(effectiveEngine, runtime.Engines) {
		return nil, fmt.Errorf("runtime engines %v do not include agent engine %q",
			runtime.Engines, effectiveEngine)
	}
	return runtime, nil
}

// skillInNamespace reports whether the legacy skill namespace carries the
// named skill record.
func (e *GateEvaluator) skillInNamespace(ctx context.Context, workspaceID, name string) (bool, error) {
	if e.deps.SkillsNamespace == nil {
		return false, fmt.Errorf("skill namespace is unavailable")
	}
	_, err := e.deps.SkillsNamespace.Get(ctx, skillNamespacePrefix+workspaceID, name)
	if err != nil {
		return false, nil
	}
	return true, nil
}

// recordModels returns the distinct non-empty model references of one agent.
func recordModels(record *registry.AgentRecord) []string {
	var models []string
	seen := make(map[string]bool)
	for _, model := range append([]string{record.Model}, record.FallbackModels...) {
		if model == "" || seen[model] {
			continue
		}
		seen[model] = true
		models = append(models, model)
	}
	return models
}
