package teameval

import (
	"context"
	"fmt"
	"strings"

	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

// gateAgentRolesExist checks that the lead avatar and every roster worker
// carry the correct role at an exact, resolvable version, and that every
// workflow-referenced agent version exists (plan §8.1: "Agent 角色、版本和
// 依赖存在").
func (e *GateEvaluator) gateAgentRolesExist(ctx context.Context, ev EvaluatedTeam) GateResult {
	evidence := fmt.Sprintf("team=%s", ev.TeamID())
	if ev.Team == nil {
		return Fail(GateAgentRolesExist, evidence, "team not found")
	}
	if strings.TrimSpace(ev.Team.LeadAvatarID) == "" {
		return Fail(GateAgentRolesExist, evidence, "team has no lead avatar")
	}
	if ev.Lead == nil {
		return Fail(
			GateAgentRolesExist,
			fmt.Sprintf("%s lead=%s", evidence, ev.Team.LeadAvatarID),
			"lead avatar is not resolvable at its exact version: "+ev.LeadErr,
		)
	}
	if ev.Lead.Role != "avatar" {
		return Fail(
			GateAgentRolesExist,
			fmt.Sprintf("%s lead=%s@%d", evidence, ev.Lead.Name, ev.Lead.Version),
			fmt.Sprintf("lead agent role is %q, want avatar", ev.Lead.Role),
		)
	}
	leadEvidence := fmt.Sprintf("%s lead=%s@%d", evidence, ev.Lead.Name, ev.Lead.Version)
	for _, worker := range ev.Workers {
		if worker.Agent == nil {
			return Fail(
				GateAgentRolesExist,
				fmt.Sprintf("%s worker=%s", leadEvidence, worker.WorkerAgentID()),
				"worker agent is not resolvable at its exact version: "+worker.Err,
			)
		}
		if worker.Agent.Role != "worker" {
			return Fail(
				GateAgentRolesExist,
				fmt.Sprintf("%s worker=%s@%d", leadEvidence, worker.Agent.Name, worker.Agent.Version),
				fmt.Sprintf("worker agent role is %q, want worker", worker.Agent.Role),
			)
		}
	}
	return Pass(GateAgentRolesExist, leadEvidence, "lead and worker roles resolve at exact versions")
}

// gateTeamShape checks the active team has exactly one lead avatar and at
// least one enabled worker (plan §8.1).
func (e *GateEvaluator) gateTeamShape(ctx context.Context, ev EvaluatedTeam) GateResult {
	evidence := fmt.Sprintf("team=%s", ev.TeamID())
	if ev.Team == nil {
		return Fail(GateTeamShape, evidence, "team not found")
	}
	if ev.Team.Status != "active" {
		return Fail(GateTeamShape, evidence, fmt.Sprintf("team status is %q, want active", ev.Team.Status))
	}
	if ev.Lead == nil || strings.TrimSpace(ev.Team.LeadAvatarID) == "" {
		return Fail(GateTeamShape, evidence, "team must have exactly one lead avatar")
	}
	enabled := 0
	for _, worker := range ev.Workers {
		if worker.TeamWorker.Enabled {
			enabled++
		}
	}
	if enabled < 1 {
		return Fail(GateTeamShape, evidence, "active team requires at least one enabled worker")
	}
	return Pass(GateTeamShape, evidence, fmt.Sprintf("one lead avatar and %d enabled worker(s)", enabled))
}

// gateWorkerKinds checks every enabled worker's allowed_kinds is non-empty
// and its default_kind belongs to the set (plan §8.1).
func (e *GateEvaluator) gateWorkerKinds(ctx context.Context, ev EvaluatedTeam) GateResult {
	evidence := fmt.Sprintf("team=%s", ev.TeamID())
	if ev.Team == nil {
		return Fail(GateWorkerKinds, evidence, "team not found")
	}
	for _, worker := range ev.Workers {
		if !worker.TeamWorker.Enabled {
			continue
		}
		ref := fmt.Sprintf("%s worker=%s", evidence, worker.WorkerAgentID())
		if len(worker.TeamWorker.AllowedKinds) == 0 {
			return Fail(GateWorkerKinds, ref, "allowed_kinds must not be empty")
		}
		if !stringInSlice(worker.TeamWorker.DefaultKind, worker.TeamWorker.AllowedKinds) {
			return Fail(
				GateWorkerKinds,
				ref,
				fmt.Sprintf("default_kind %q is not in allowed_kinds %v",
					worker.TeamWorker.DefaultKind, worker.TeamWorker.AllowedKinds),
			)
		}
	}
	return Pass(GateWorkerKinds, evidence, "every enabled worker declares non-empty kinds with a valid default")
}

// gateWorkflowRefs checks every workflow only references this team's enabled
// workers, with an allowed kind and an exact (non-latest) agent version
// (plan §8.1).
func (e *GateEvaluator) gateWorkflowRefs(ctx context.Context, ev EvaluatedTeam) GateResult {
	evidence := fmt.Sprintf("team=%s", ev.TeamID())
	if ev.Team == nil {
		return Fail(GateWorkflowRefs, evidence, "team not found")
	}
	rosterByID := make(map[string]registry.TeamWorker, len(ev.Workers))
	for _, worker := range ev.Workers {
		rosterByID[worker.WorkerAgentID()] = worker.TeamWorker
	}
	for _, wf := range ev.Workflows {
		if wf.Version == nil || wf.Graph.DecodeErr != "" {
			continue // reported by gate_graph_schema
		}
		for _, node := range wf.Graph.Graph.Nodes {
			agentID, version, kind, ok := nodeAgentRef(node)
			if !ok {
				continue
			}
			ref := fmt.Sprintf("%s workflow=%s node=%s agent=%s@%d",
				evidence, wf.Workflow.ID, node.ID, agentID, version)
			if version < 1 {
				return Fail(
					GateWorkflowRefs,
					ref,
					"workflow references agent_version 0 (latest); pin an exact version",
				)
			}
			roster, inRoster := rosterByID[agentID]
			if !inRoster || !roster.Enabled {
				return Fail(
					GateWorkflowRefs,
					ref,
					"workflow references an agent that is not an enabled worker of this team",
				)
			}
			if kind != "" && !stringInSlice(string(kind), roster.AllowedKinds) {
				return Fail(
					GateWorkflowRefs,
					ref,
					fmt.Sprintf("workflow kind %q is not in the worker's allowed_kinds %v",
						kind, roster.AllowedKinds),
				)
			}
		}
	}
	return Pass(GateWorkflowRefs, evidence, "all workflow node references point at enabled roster workers with allowed kinds and exact versions")
}

// gateNoCrossEmployee checks employee graphs contain no worker step and the
// team-referenced agents carry no sub_agents (plan §8.1).
func (e *GateEvaluator) gateNoCrossEmployee(ctx context.Context, ev EvaluatedTeam) GateResult {
	evidence := fmt.Sprintf("team=%s", ev.TeamID())
	if ev.Team == nil {
		return Fail(GateNoCrossEmployee, evidence, "team not found")
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
		if len(record.SubAgents) != 0 {
			return Fail(
				GateNoCrossEmployee,
				ref,
				fmt.Sprintf("agent defines sub_agents %v; cross-agent orchestration belongs to the team workflow",
					subAgentNames(record.SubAgents)),
			)
		}
		if record.GraphDefinition != nil {
			for _, step := range record.GraphDefinition.Steps {
				if step.Type == "worker" {
					return Fail(
						GateNoCrossEmployee,
						ref,
						fmt.Sprintf("employee graph contains forbidden worker step %q", step.Name),
					)
				}
			}
		}
	}
	return Pass(GateNoCrossEmployee, evidence, "no employee graph contains a worker step and no team agent defines sub_agents")
}

// gateGraphSchema runs the shared validators: every employee graph through
// ValidateEmployeeGraph (T04) and every team workflow through the shared
// machine.Validate assembly (trigger + graph, T05).
func (e *GateEvaluator) gateGraphSchema(ctx context.Context, ev EvaluatedTeam) GateResult {
	evidence := fmt.Sprintf("team=%s", ev.TeamID())
	if ev.Team == nil {
		return Fail(GateGraphSchema, evidence, "team not found")
	}
	for _, worker := range ev.Workers {
		if worker.Agent == nil || worker.Agent.GraphDefinition == nil {
			continue // capability gates report the missing graph
		}
		validation := ValidateEmployeeGraph(worker.Agent.GraphDefinition)
		if !validation.Valid {
			return Fail(
				GateGraphSchema,
				fmt.Sprintf("%s worker=%s@%d", evidence, worker.Agent.Name, worker.Agent.Version),
				fmt.Sprintf("employee graph validation failed: %s", problemCodes(validation.Errors)),
			)
		}
	}
	for _, wf := range ev.Workflows {
		ref := fmt.Sprintf("%s workflow=%s", evidence, wf.Workflow.ID)
		if wf.Version == nil {
			return Fail(GateGraphSchema, ref, "workflow has no readable version")
		}
		if wf.Trigger.DecodeErr != "" {
			return Fail(GateGraphSchema, ref, "trigger decode failed: "+wf.Trigger.DecodeErr)
		}
		if wf.Graph.DecodeErr != "" {
			return Fail(GateGraphSchema, ref, "graph decode failed: "+wf.Graph.DecodeErr)
		}
		report, err := ValidateWorkflowDraft(
			ctx,
			WorkflowValidateDeps{
				Teams:     e.deps.Teams,
				Roster:    e.deps.Roster,
				Agents:    e.deps.Agents,
				Workflows: e.deps.Workflows,
			},
			ev.WorkspaceID(),
			wf.Workflow.ID,
			wf.Trigger.Trigger,
			wf.Graph.Graph,
		)
		if err != nil {
			return Fail(GateGraphSchema, ref, "workflow validation unavailable: "+err.Error())
		}
		if len(report.Issues) != 0 {
			return Fail(
				GateGraphSchema,
				ref,
				fmt.Sprintf("workflow machine.Validate failed: %s",
					workflowIssueCodes(report.Issues)),
			)
		}
	}
	return Pass(GateGraphSchema, evidence, "every employee graph and team workflow passes the shared validators")
}

// gateEngineGraphMatch checks an agent with an internal graph stays on the
// in-process loom engine (""/loom) and its model binding is freezable (F13
// positive gate, plan §8.1).
func (e *GateEvaluator) gateEngineGraphMatch(ctx context.Context, ev EvaluatedTeam) GateResult {
	evidence := fmt.Sprintf("team=%s", ev.TeamID())
	if ev.Team == nil {
		return Fail(GateEngineGraphMatch, evidence, "team not found")
	}
	agents := append([]*registry.AgentRecord{}, ev.Lead)
	for _, worker := range ev.Workers {
		agents = append(agents, worker.Agent)
	}
	for _, record := range agents {
		if record == nil || record.GraphDefinition == nil {
			continue
		}
		ref := fmt.Sprintf("%s agent=%s@%d", evidence, record.Name, record.Version)
		if engine.IsCLIEngine(record.Engine) {
			return Fail(
				GateEngineGraphMatch,
				ref,
				fmt.Sprintf("agent carries an internal graph but engine is %q; loom engine required", record.Engine),
			)
		}
		if record.Model != "" {
			if err := e.resolveModel(ctx, ev.WorkspaceID(), record.Model); err != nil {
				return Fail(GateEngineGraphMatch, ref, "model binding is not freezable: "+err.Error())
			}
		}
	}
	return Pass(GateEngineGraphMatch, evidence, "every graphed agent uses the loom engine and its model binding is freezable")
}

// gateScopeCompliance checks every evaluated asset sits inside the build
// run's frozen asset scope (plan §8.1: 权限不超出 BuildBrief).
func (e *GateEvaluator) gateScopeCompliance(ctx context.Context, ev EvaluatedTeam) GateResult {
	evidence := fmt.Sprintf("build_run=%s", ev.BuildRun.BuildRunID)
	scope := ev.BuildRun.AssetScope
	if briefTeam := ev.BuildRun.Brief.TeamID; briefTeam != "" && briefTeam != ev.TeamID() {
		return Fail(
			GateScopeCompliance,
			evidence,
			fmt.Sprintf("build brief targets team %q but evaluation targets team %q", briefTeam, ev.TeamID()),
		)
	}
	if ev.Team == nil {
		return Fail(GateScopeCompliance, evidence, "team not found")
	}
	refs := []teambuild.AssetRef{
		{Kind: "team", ID: ev.Team.ID, Name: ev.Team.Name},
	}
	if ev.Lead != nil {
		refs = append(refs, teambuild.AssetRef{Kind: "agent", ID: ev.Lead.ID, Name: ev.Lead.Name})
	}
	for _, worker := range ev.Workers {
		ref := teambuild.AssetRef{Kind: "agent", ID: worker.WorkerAgentID()}
		if worker.Agent != nil {
			ref.Name = worker.Agent.Name
		}
		refs = append(refs, ref)
	}
	for _, wf := range ev.Workflows {
		refs = append(refs, teambuild.AssetRef{Kind: "workflow", ID: wf.Workflow.ID, Name: wf.Workflow.Name})
	}
	for _, ref := range refs {
		if !scope.Contains(ref) {
			return Fail(
				GateScopeCompliance,
				fmt.Sprintf("%s ref=%s", evidence, assetRefText(ref)),
				"evaluated asset is outside the build run's frozen asset scope",
			)
		}
	}
	return Pass(GateScopeCompliance, evidence, "every evaluated asset is inside the build run's asset scope")
}

// gateL1WorkerCapabilities keeps the legacy stable gate ID while evaluating
// the redesigned two-layer execution contract. A worker is executable through
// exactly one persisted platform path: an external CLI runtime, a valid
// declarative graph, or the standard in-process ToolLoop. Internal graphs are
// optional and no LLM-authored capability proof is accepted.
func (e *GateEvaluator) gateL1WorkerCapabilities(ctx context.Context, ev EvaluatedTeam) GateResult {
	evidence := fmt.Sprintf("team=%s", ev.TeamID())
	if ev.Team == nil {
		return Fail(GateL1WorkerCapabilities, evidence, "team not found")
	}
	for _, worker := range ev.Workers {
		if worker.Agent == nil || !worker.TeamWorker.Enabled {
			continue
		}
		ref := fmt.Sprintf("%s worker=%s@%d", evidence, worker.Agent.Name, worker.Agent.Version)
		if engine.IsCLIEngine(worker.Agent.Engine) {
			if strings.TrimSpace(worker.Agent.RuntimeID) == "" {
				return Fail(GateL1WorkerCapabilities, ref, "CLI worker has no pinned runtime_id")
			}
			continue
		}
		if worker.Agent.GraphDefinition != nil {
			validation := ValidateEmployeeGraph(worker.Agent.GraphDefinition)
			if !validation.Valid {
				return Fail(GateL1WorkerCapabilities, ref,
					"worker internal graph is not executable: "+strings.Join(problemCodes(validation.Errors), ","))
			}
			continue
		}
		if worker.Agent.Engine != "" && worker.Agent.Engine != "loom" {
			return Fail(GateL1WorkerCapabilities, ref,
				fmt.Sprintf("worker engine %q has no supported execution path", worker.Agent.Engine))
		}
		if strings.TrimSpace(worker.Agent.Spec.Identity.Core) == "" {
			return Fail(GateL1WorkerCapabilities, ref, "standard ToolLoop worker has no system prompt")
		}
	}
	return Pass(GateL1WorkerCapabilities, evidence, "every enabled worker has a platform-derived ToolLoop, CLI runtime, or declarative-graph execution path")
}

// gateL1FlowCapabilities keeps the legacy stable gate ID while evaluating the
// universal workflow layer: real worker execution plus deliver. The full
// machine validator proves route, parallel/join, and loop legality when those
// optional shapes exist. F14 therefore forbids illegal rework; it does not
// require every workflow to fabricate a rework loop or fanout.
func (e *GateEvaluator) gateL1FlowCapabilities(ctx context.Context, ev EvaluatedTeam) GateResult {
	evidence := fmt.Sprintf("team=%s", ev.TeamID())
	if ev.Team == nil {
		return Fail(GateL1FlowCapabilities, evidence, "team not found")
	}
	if len(ev.Workflows) == 0 {
		return Fail(GateL1FlowCapabilities, evidence, "team has no fixed workflow (L1 requires one)")
	}
	for _, wf := range ev.Workflows {
		if wf.Version == nil || wf.Graph.DecodeErr != "" {
			return Fail(
				GateL1FlowCapabilities,
				fmt.Sprintf("%s workflow=%s", evidence, wf.Workflow.ID),
				"workflow has no readable graph",
			)
		}
		warnings := WorkflowCapabilityWarnings(wf.Graph.Graph)
		if len(warnings) != 0 {
			return Fail(
				GateL1FlowCapabilities,
				fmt.Sprintf("%s workflow=%s", evidence, wf.Workflow.ID),
				fmt.Sprintf("workflow misses L1 capability points: %s",
					workflowWarningCodes(warnings)),
			)
		}
	}
	return Pass(GateL1FlowCapabilities, evidence, "every fixed workflow composes real worker execution into deliver; optional fanout and rework shapes are machine-valid")
}

// --- helpers ---

func (ev EvaluatedTeam) TeamID() string {
	if ev.Team == nil {
		return ""
	}
	return ev.Team.ID
}

func (ev EvaluatedTeam) WorkspaceID() string {
	if ev.Team == nil {
		return ev.BuildRun.WorkspaceID
	}
	return ev.Team.WorkspaceID
}

func stringInSlice(value string, options []string) bool {
	for _, option := range options {
		if value == option {
			return true
		}
	}
	return false
}

func subAgentNames(refs []registry.SubAgentRef) []string {
	names := make([]string, 0, len(refs))
	for _, ref := range refs {
		names = append(names, ref.Name)
	}
	return names
}

func problemCodes(problems []GraphProblem) []string {
	codes := make([]string, 0, len(problems))
	for _, problem := range problems {
		codes = append(codes, problem.Code)
	}
	return codes
}

func workflowIssueCodes(issues []machine.ValidationIssue) []string {
	codes := make([]string, 0, len(issues))
	for _, issue := range issues {
		codes = append(codes, issue.Code)
	}
	return codes
}

func workflowWarningCodes(warnings []WorkflowCapabilityWarning) []string {
	codes := make([]string, 0, len(warnings))
	for _, warning := range warnings {
		codes = append(codes, warning.Code)
	}
	return codes
}

func assetRefText(ref teambuild.AssetRef) string {
	if ref.Name != "" {
		return ref.Kind + "=" + ref.Name
	}
	return ref.Kind + "=" + ref.ID
}

// nodeAgentRef extracts the agent reference from a worker or handoff node.
func nodeAgentRef(node machine.Node) (agentID string, version int64, kind machine.WorkerKind, ok bool) {
	switch node.Type {
	case machine.NodeWorker:
		config, ok := node.Config.(machine.WorkerConfig)
		if !ok {
			return "", 0, "", false
		}
		return config.AgentID, config.AgentVersion, config.Kind, true
	case machine.NodeHandoff:
		config, ok := node.Config.(machine.HandoffConfig)
		if !ok {
			return "", 0, "", false
		}
		return config.AgentID, config.AgentVersion, "", true
	default:
		return "", 0, "", false
	}
}
