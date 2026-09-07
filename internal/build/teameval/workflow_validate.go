package teameval

// Team-workflow validation bridge (plan §10.2.3). This package owns the
// single machine.Validate assembly (校验器复用纪律): the teamforge tool
// layer and the hard-gate evaluator both consume it, so the proof assembly
// exists exactly once:
//
//   - authorization snapshot: owning team row + full team-worker roster;
//   - agent version proofs: exact immutable versions of the lead avatar and
//     every worker/handoff agent referenced by the graph;
//   - dependency snapshot: resolved for every agent version that could be
//     read at its exact key;
//   - capability proofs: a conservative static derivation from the agent
//     record (sub-agents / internal worker steps / yield steps); the exact
//     frozen bundle capability belongs to the frozen-candidate pipeline
//     (T06) and is deliberately not re-implemented here;
//   - factory proofs: resolved against the same descriptor registry the
//     platform compiles with (standard frozen + declarative).
//
// Scoped-reference proofs (catalog / schedule / delivery targets) are
// intentionally empty: they can only be proven by the frozen-candidate
// pipeline, so non-session triggers report CodeReferenceProofMissing and are
// treated as L2 (the plan excludes them from the L1 hard gate).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/org"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

// PlannedWorkerBinding is a platform-derived create-mode AgentVersion pin.
// It is never accepted from a planner-authored workflow spec.
type PlannedWorkerBinding struct {
	StableRef    string
	AgentID      string
	AgentVersion int64
}

// WorkflowValidateDeps aggregates the narrow read-only store interfaces
// backing the shared workflow validation assembly. Production
// implementations are the existing stores.
type WorkflowValidateDeps struct {
	// Teams lists workspace teams (the existing team read path).
	Teams TeamReader
	// Roster reads the complete team-worker roster of one team.
	Roster RosterReader
	// Agents lists workspace agents and reads exact immutable versions.
	Agents AgentReader
	// Workflows reads one workflow row (its owning team).
	Workflows WorkflowReader
}

// TeamReader is satisfied by *org.Store.
type TeamReader interface {
	ListTeams(ctx context.Context, workspaceID string) ([]org.Team, error)
}

// RosterReader is satisfied by *registry.TeamWorkerRepository.
type RosterReader interface {
	ListByTeam(ctx context.Context, workspaceID, teamID string) ([]registry.TeamWorker, error)
}

// AgentReader is satisfied by *registry.AgentRegistry.
type AgentReader interface {
	List(ctx context.Context, workspaceID string) ([]registry.AgentRecord, error)
	GetVersion(ctx context.Context, workspaceID, agentID string, version int) (*registry.AgentRecord, error)
}

// WorkflowReader is satisfied by *workflow.Store.
type WorkflowReader interface {
	Get(ctx context.Context, workspaceID, workflowID string) (*workflow.TeamWorkflow, error)
}

// WorkflowCapabilityWarning is one composition warning on a team workflow:
// a fixed workflow must execute at least one real worker and terminate at a
// deliver node. Optional fanout, join, and rework shapes are validated by the
// machine when present; they are not universal business requirements.
type WorkflowCapabilityWarning struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint"`
}

// WorkflowCapabilityWarnings reports missing universal composition points.
// F14 is enforced by machine.Validate: any rework that exists must be a
// machine-native loop, but a workflow without a business rework need does not
// need to invent one. The L1 hard gate promotes these warnings to failures.
func WorkflowCapabilityWarnings(graph machine.GraphDefinition) []WorkflowCapabilityWarning {
	var hasWorker, hasDeliver bool
	for _, node := range graph.Nodes {
		switch node.Type {
		case machine.NodeWorker, machine.NodeHandoff:
			hasWorker = true
		case machine.NodeDeliver:
			hasDeliver = true
		}
	}
	var warnings []WorkflowCapabilityWarning
	if !hasWorker {
		warnings = append(warnings, WorkflowCapabilityWarning{
			Path:    "/nodes",
			Code:    "workflow_warning_no_worker_execution",
			Message: "workflow has no worker or handoff execution node",
			Hint:    "compose at least one exact roster worker version before deliver; do not invent fanout when the business flow is sequential",
		})
	}
	if !hasDeliver {
		warnings = append(warnings, WorkflowCapabilityWarning{
			Path:    "/nodes",
			Code:    "workflow_warning_no_deliver",
			Message: "workflow has no deliver capability point (no deliver node)",
			Hint:    "add a deliver node that references the final worker, loop, join, or lead output",
		})
	}
	return warnings
}

// ValidateWorkflowDraft runs the complete machine validator over one
// workflow shape (trigger + graph) with proofs assembled from the real
// stores. The workflow row is read to discover the owning team. It never
// writes.
func ValidateWorkflowDraft(
	ctx context.Context,
	deps WorkflowValidateDeps,
	workspaceID, workflowID string,
	trigger machine.TriggerConfig,
	graph machine.GraphDefinition,
) (machine.Report, error) {
	return validateWorkflowDraft(ctx, deps, workspaceID, workflowID, trigger, graph, false)
}

// ValidateTemplateWorkflowDraft is the template-instantiation variant of
// ValidateWorkflowDraft. A template team is deliberately kept in building
// until its first workflow is frozen and published in the same transaction,
// so this validator accepts that one transitional team state. Ordinary
// workflow authoring continues to require an active team.
func ValidateTemplateWorkflowDraft(
	ctx context.Context,
	deps WorkflowValidateDeps,
	workspaceID, workflowID string,
	trigger machine.TriggerConfig,
	graph machine.GraphDefinition,
) (machine.Report, error) {
	return validateWorkflowDraft(ctx, deps, workspaceID, workflowID, trigger, graph, true)
}

func validateWorkflowDraft(
	ctx context.Context,
	deps WorkflowValidateDeps,
	workspaceID, workflowID string,
	trigger machine.TriggerConfig,
	graph machine.GraphDefinition,
	allowBuildingTeam bool,
) (machine.Report, error) {
	var out machine.Report
	if deps.Workflows == nil {
		return out, errors.New("workflow read is unavailable")
	}

	workflowRow, err := deps.Workflows.Get(ctx, workspaceID, workflowID)
	if err != nil {
		return out, fmt.Errorf("read workflow %q: %w", workflowID, err)
	}
	return validateWorkflowForTeam(ctx, deps, workspaceID, workflowRow.TeamID, trigger, graph, allowBuildingTeam)
}

// ValidateWorkflowForTeam runs the same proof assembly as
// ValidateWorkflowDraft when the platform is compiling a not-yet-persisted
// workflow for a known existing team. It is used by the declarative_v1 plan
// tool before a ChangeSet is stored.
func ValidateWorkflowForTeam(
	ctx context.Context,
	deps WorkflowValidateDeps,
	workspaceID, teamID string,
	trigger machine.TriggerConfig,
	graph machine.GraphDefinition,
) (machine.Report, error) {
	return validateWorkflowForTeam(ctx, deps, workspaceID, teamID, trigger, graph, false)
}

func validateWorkflowForTeam(
	ctx context.Context,
	deps WorkflowValidateDeps,
	workspaceID, teamID string,
	trigger machine.TriggerConfig,
	graph machine.GraphDefinition,
	allowBuildingTeam bool,
) (machine.Report, error) {
	var out machine.Report
	if deps.Teams == nil || deps.Roster == nil || deps.Agents == nil {
		return out, errors.New("team/roster/agent read is unavailable")
	}
	if teamID == "" {
		return out, errors.New("team_id is required")
	}

	teamState := machine.ProofResolved
	var team *org.Team
	teams, err := deps.Teams.ListTeams(ctx, workspaceID)
	if err != nil {
		return out, fmt.Errorf("list teams: %w", err)
	}
	for i := range teams {
		if teams[i].ID == teamID {
			team = &teams[i]
			break
		}
	}
	if team == nil || (team.Status != "active" && !(allowBuildingTeam && team.Status == "building")) {
		teamState = machine.ProofNotFound
	}

	workers, err := deps.Roster.ListByTeam(ctx, workspaceID, teamID)
	if err != nil {
		return out, fmt.Errorf("read roster for team %q: %w", teamID, err)
	}
	workerProofs := make(map[string]machine.TeamWorkerProof, len(workers))
	for _, worker := range workers {
		kinds := make([]machine.AuthorizationKind, len(worker.AllowedKinds))
		for i, kind := range worker.AllowedKinds {
			kinds[i] = machine.AuthorizationKind(kind)
		}
		workerProofs[worker.WorkerAgentID] = machine.NewTeamWorkerProof(
			machine.ProofResolved, worker.Enabled, kinds,
		)
	}

	agentsList, err := deps.Agents.List(ctx, workspaceID)
	if err != nil {
		return out, fmt.Errorf("list agents: %w", err)
	}
	lead := machine.AgentVersionKey{}
	if team != nil {
		for i := range agentsList {
			if agentsList[i].ID == team.LeadAvatarID {
				lead = machine.AgentVersionKey{
					AgentID:      team.LeadAvatarID,
					AgentVersion: int64(agentsList[i].Version),
				}
				break
			}
		}
	}

	agents := make(map[machine.AgentVersionKey]machine.AgentVersionProof)
	capabilities := make(map[machine.AgentVersionKey]machine.CapabilityProof)
	dependencyProofs := make(map[machine.AgentVersionKey]machine.DependencyProof)
	factoryProofs := make(map[machine.FactoryKey]machine.FactoryProof)
	for _, bundle := range machine.ReferencedBundles(lead, graph) {
		key := bundle.Key
		if _, seen := agents[key]; seen {
			continue
		}
		record, err := deps.Agents.GetVersion(ctx, workspaceID, key.AgentID, int(key.AgentVersion))
		if err != nil {
			agents[key] = machine.NewAgentVersionProof(
				machine.AgentProofNotFound, machine.FactoryKey{}, nil, false, false,
			)
			capabilities[key] = machine.NewCapabilityProof(
				machine.ProofNotFound, false, nil, nil, false, nil,
			)
			continue
		}

		selectedFactory, factoryErr := workflowFactoryRegistry().SelectAgentFactoryKey(*record)
		factoryKey := machine.FactoryKey{
			FactoryID:      selectedFactory.FactoryID,
			FactoryVersion: selectedFactory.FactoryVersion,
			CompilerABI:    selectedFactory.CompilerABI,
		}
		proofState := machine.AgentProofResolved
		if factoryErr != nil {
			proofState = machine.AgentProofUnprovable
		}
		hasSubAgents := len(record.SubAgents) > 0
		hasWorkerStep := false
		if record.GraphDefinition != nil {
			for _, step := range record.GraphDefinition.Steps {
				if step.Type == "worker" {
					hasWorkerStep = true
					break
				}
			}
		}
		var outputSchema json.RawMessage
		if record.OutputSchema != nil {
			outputSchema = append(json.RawMessage(nil), (*record.OutputSchema)...)
		}
		agents[key] = machine.NewAgentVersionProof(
			proofState, factoryKey, outputSchema, hasSubAgents, hasWorkerStep,
		)
		capabilities[key] = staticCapabilityProof(record)
		dependencyProofs[key] = machine.NewDependencyProof(machine.DependencyProofResolved, nil)
		if factoryErr == nil {
			factoryProofs[factoryKey] = machine.NewFactoryProof(machine.FactoryProofResolved, nil)
		}
	}

	validationContext := machine.ValidationContext{
		WorkspaceID: workspaceID,
		TeamID:      teamID,
		Lead:        lead,
		Trigger:     trigger,
		Graph:       graph,
		Authorization: machine.NewAuthorizationSnapshot(
			workspaceID, teamID, teamState, workerProofs, nil,
		),
		Agents: agents, Capabilities: capabilities,
		Dependencies: machine.NewDependencySnapshot(
			dependencyProofs,
			machine.NewDependencyProof(machine.DependencyProofResolved, nil),
		),
		Factories: machine.NewFactorySnapshot(factoryProofs),
	}
	return machine.Validate(validationContext), nil
}

// ValidateWorkflowForBlueprint validates a create-mode graph against the
// immutable roster and first AgentVersions that its frozen Blueprint and
// ChangeSet will materialize. This preserves the same authorization, agent,
// dependency, capability, and factory proof shape as live-team validation
// without requiring not-yet-created rows to exist in the database.
func ValidateWorkflowForBlueprint(
	workspaceID string,
	blueprint teambuild.TeamBlueprintV1,
	bindings []PlannedWorkerBinding,
	trigger machine.TriggerConfig,
	graph machine.GraphDefinition,
) (machine.Report, error) {
	var out machine.Report
	if blueprint.Mode != teambuild.ModeCreate || workspaceID == "" || blueprint.NewTeamName == "" {
		return out, errors.New("create-mode workspace and Blueprint identity are required")
	}
	members := make(map[string]teambuild.BlueprintMemberV1, len(blueprint.Members))
	for _, member := range blueprint.Members {
		members[member.StableRef] = member
	}
	leadMember, ok := members[blueprint.LeadRef]
	if !ok || leadMember.Role != teambuild.BlueprintMemberRoleAvatar {
		return out, errors.New("create Blueprint lead is unavailable")
	}
	lead := machine.AgentVersionKey{AgentID: leadMember.Name, AgentVersion: 1}
	standardFactory, factoryErr := workflowFactoryRegistry().SelectAgentFactoryKey(registry.AgentRecord{})
	if factoryErr != nil {
		return out, fmt.Errorf("resolve standard workflow factory: %w", factoryErr)
	}
	factoryKey := machine.FactoryKey{
		FactoryID: standardFactory.FactoryID, FactoryVersion: standardFactory.FactoryVersion,
		CompilerABI: standardFactory.CompilerABI,
	}
	resolvedAgent := machine.NewAgentVersionProof(machine.AgentProofResolved, factoryKey, nil, false, false)
	agents := map[machine.AgentVersionKey]machine.AgentVersionProof{lead: resolvedAgent}
	capabilities := map[machine.AgentVersionKey]machine.CapabilityProof{
		lead: machine.NewCapabilityProof(machine.ProofResolved, false, nil, nil, false, nil),
	}
	dependencies := map[machine.AgentVersionKey]machine.DependencyProof{
		lead: machine.NewDependencyProof(machine.DependencyProofResolved, nil),
	}
	workers := make(map[string]machine.TeamWorkerProof, len(bindings))
	for _, binding := range bindings {
		member, exists := members[binding.StableRef]
		if !exists || member.Role != teambuild.BlueprintMemberRoleWorker ||
			binding.AgentID != member.Name || binding.AgentVersion != 1 {
			return out, fmt.Errorf("planned worker binding %q does not match the frozen create Blueprint", binding.StableRef)
		}
		key := machine.AgentVersionKey{AgentID: binding.AgentID, AgentVersion: binding.AgentVersion}
		agents[key] = resolvedAgent
		capabilities[key] = machine.NewCapabilityProof(machine.ProofResolved, false, nil, nil, false, nil)
		dependencies[key] = machine.NewDependencyProof(machine.DependencyProofResolved, nil)
		workers[binding.AgentID] = machine.NewTeamWorkerProof(
			machine.ProofResolved, true,
			[]machine.AuthorizationKind{machine.AuthorizationConsult, machine.AuthorizationDispatch},
		)
	}
	validationContext := machine.ValidationContext{
		WorkspaceID: workspaceID, TeamID: blueprint.NewTeamName, Lead: lead, Trigger: trigger, Graph: graph,
		Authorization: machine.NewAuthorizationSnapshot(
			workspaceID, blueprint.NewTeamName, machine.ProofResolved, workers, nil,
		),
		Agents: agents, Capabilities: capabilities,
		Dependencies: machine.NewDependencySnapshot(
			dependencies, machine.NewDependencyProof(machine.DependencyProofResolved, nil),
		),
		Factories: machine.NewFactorySnapshot(map[machine.FactoryKey]machine.FactoryProof{
			factoryKey: machine.NewFactoryProof(machine.FactoryProofResolved, nil),
		}),
	}
	return machine.Validate(validationContext), nil
}

// staticCapabilityProof derives a conservative capability manifest from the
// exact AgentRecord: sub-agents and internal worker steps are cross-agent
// capabilities, yield steps are interactive. The exact frozen bundle
// capability is produced by the candidate pipeline at freeze time (T06);
// this static derivation only needs to be safe for draft-phase validation.
func staticCapabilityProof(record *registry.AgentRecord) machine.CapabilityProof {
	mayYield := false
	var interactiveSteps []string
	mayInvokeAgent := len(record.SubAgents) > 0
	agentSteps := make([]string, 0, len(record.SubAgents))
	for _, sub := range record.SubAgents {
		agentSteps = append(agentSteps, sub.Name)
	}
	if record.GraphDefinition != nil {
		for _, step := range record.GraphDefinition.Steps {
			switch step.Type {
			case "yield":
				mayYield = true
				interactiveSteps = append(interactiveSteps, step.Name)
			case "worker":
				mayInvokeAgent = true
				agentSteps = append(agentSteps, step.Name)
			}
		}
	}
	return machine.NewCapabilityProof(
		machine.ProofResolved, mayYield, interactiveSteps, nil, mayInvokeAgent, agentSteps,
	)
}

// DeclarativeFactoryKey is the draft-phase declarative factory identity
// (plan §10.2.3, ticket T08). internal/teameval cannot import
// internal/declarative — that package imports internal/api for context
// helpers, and api must be able to import teamforge for the PlatformTools
// wiring — so the workflow draft validator resolves the declarative factory
// key from the process-global descriptor registry (which cmd/weave seeds
// with the real descriptor) and falls back to a locally defined stand-in
// with the exact same factory identity.
var DeclarativeFactoryKey = frozen.FactoryKey{
	FactoryID:      "declarative",
	FactoryVersion: "1",
	CompilerABI:    "weave-graph-abi-v1",
}

type declarativeStandinEnumerator struct{}

func (declarativeStandinEnumerator) FreezeSchema() compiler.FreezeSchema {
	return compiler.FreezeSchema{
		SchemaID:      "weave-declarative-factory-input",
		SchemaVersion: frozen.FrozenSchemaVersion,
	}
}

func (declarativeStandinEnumerator) EncodeFactoryInput(
	context.Context,
	registry.AgentRecord,
	compiler.CredentialRefEncoder,
) (json.RawMessage, error) {
	return nil, errors.New("declarative factory input encoding is not available in draft-phase validation")
}

func (declarativeStandinEnumerator) EnumerateDependencies(
	context.Context,
	frozen.FrozenAgentRecord,
	compiler.MetadataResolver,
) (frozen.EnumeratedDependencyManifest, error) {
	return frozen.EnumeratedDependencyManifest{},
		errors.New("declarative dependency enumeration is not available in draft-phase validation")
}

func declarativeStandinDescriptor() compiler.GraphFactoryDescriptor {
	return compiler.GraphFactoryDescriptor{
		FactoryID:             DeclarativeFactoryKey.FactoryID,
		FactoryVersion:        DeclarativeFactoryKey.FactoryVersion,
		CompilerABI:           DeclarativeFactoryKey.CompilerABI,
		EnumerateDependencies: declarativeStandinEnumerator{},
		Compile: func(
			context.Context,
			frozen.FrozenExecutionBundle,
			compiler.FrozenResolver,
			compiler.FrozenBuildOpts,
		) (*loom.Graph, frozen.CapabilityManifest, error) {
			return nil, frozen.CapabilityManifest{},
				errors.New("declarative compilation is not available in draft-phase validation")
		},
	}
}

// workflowFactoryRegistry mirrors cmd/weave/main.go's descriptor
// registration so draft-phase factory selection matches the platform. The
// declarative descriptor is resolved from the process-global registry first
// (cmd/weave seeds it with the real descriptor) and falls back to the local
// stand-in with the identical factory key when the global registry is empty
// (tests and embedded runs).
var (
	workflowFactoryRegistryOnce sync.Once
	workflowFactoryRegistryVar  *compiler.DescriptorRegistry
)

func workflowFactoryRegistry() *compiler.DescriptorRegistry {
	return WorkflowFactoryRegistry()
}

// WorkflowFactoryRegistry returns the process-global descriptor registry the
// shared validation assembly selects factory keys from. Exported for the
// teamforge tool layer, which must select keys with the exact same registry.
func WorkflowFactoryRegistry() *compiler.DescriptorRegistry {
	workflowFactoryRegistryOnce.Do(func() {
		registry := compiler.NewDescriptorRegistry()
		_ = registry.Register(compiler.NewStandardFrozenDescriptor())
		_ = registry.Register(compiler.NewStandardFrozenToolsDescriptor())
		if descriptor, err := compiler.LookupDescriptor(DeclarativeFactoryKey); err == nil {
			_ = registry.Register(descriptor)
		} else {
			_ = registry.Register(declarativeStandinDescriptor())
		}
		workflowFactoryRegistryVar = registry
	})
	return workflowFactoryRegistryVar
}
