package machine

import (
	"encoding/json"
	"fmt"
	"sort"
)

// AgentVersionKey is the exact, immutable identity used by every team bundle.
// Workspace scoping is supplied once by ValidationContext.WorkspaceID.
type AgentVersionKey struct {
	AgentID      string
	AgentVersion int64
}

// FactoryKey is the complete team compiler identity. Team validation and
// compilation must never resolve a factory by ID alone.
type FactoryKey struct {
	FactoryID      string
	FactoryVersion string
	CompilerABI    string
}

type ProofState string

const (
	ProofResolved             ProofState = "resolved"
	ProofNotFound             ProofState = "not_found"
	ProofCorrupt              ProofState = "corrupt"
	ProofUnprovable           ProofState = "unprovable"
	ProofDeferredAgent        ProofState = "deferred_agent"
	ProofDeferredDependencies ProofState = "deferred_dependencies"
	ProofDeferredFactory      ProofState = "deferred_factory"
)

// CapabilityProof is a caller-supplied static capability result for one exact
// bundle. Identifier slices are private and copied so later caller mutation
// cannot change a validation report.
type CapabilityProof struct {
	State          ProofState
	MayYield       bool
	MayInvokeAgent bool

	interactiveStepIDs []string
	interactiveToolIDs []string
	agentStepIDs       []string
}

func NewCapabilityProof(
	state ProofState,
	mayYield bool,
	interactiveStepIDs []string,
	interactiveToolIDs []string,
	mayInvokeAgent bool,
	agentStepIDs []string,
) CapabilityProof {
	return CapabilityProof{
		State:              state,
		MayYield:           mayYield,
		MayInvokeAgent:     mayInvokeAgent,
		interactiveStepIDs: canonicalProofIDs(interactiveStepIDs),
		interactiveToolIDs: canonicalProofIDs(interactiveToolIDs),
		agentStepIDs:       canonicalProofIDs(agentStepIDs),
	}
}

func (p CapabilityProof) InteractiveStepIDs() []string {
	return append([]string(nil), p.interactiveStepIDs...)
}

func (p CapabilityProof) InteractiveToolIDs() []string {
	return append([]string(nil), p.interactiveToolIDs...)
}

func (p CapabilityProof) AgentStepIDs() []string {
	return append([]string(nil), p.agentStepIDs...)
}

func (p CapabilityProof) hasInteractiveCapability() bool {
	return p.MayYield || len(p.interactiveStepIDs) != 0 || len(p.interactiveToolIDs) != 0
}

func (p CapabilityProof) hasCrossAgentCapability() bool {
	return p.MayInvokeAgent || len(p.agentStepIDs) != 0
}

func canonicalProofIDs(values []string) []string {
	copied := append([]string(nil), values...)
	sort.Strings(copied)
	return copied
}

type AuthorizationKind string

const (
	AuthorizationConsult  AuthorizationKind = "consult"
	AuthorizationDispatch AuthorizationKind = "dispatch"
	AuthorizationHandoff  AuthorizationKind = "handoff"
)

type TeamWorkerProof struct {
	State   ProofState
	Enabled bool

	allowedKinds map[AuthorizationKind]struct{}
}

func NewTeamWorkerProof(
	state ProofState,
	enabled bool,
	allowedKinds []AuthorizationKind,
) TeamWorkerProof {
	kinds := make(map[AuthorizationKind]struct{}, len(allowedKinds))
	for _, kind := range allowedKinds {
		kinds[kind] = struct{}{}
	}
	return TeamWorkerProof{
		State:        state,
		Enabled:      enabled,
		allowedKinds: kinds,
	}
}

func (p TeamWorkerProof) AllowedKinds() []AuthorizationKind {
	kinds := make([]AuthorizationKind, 0, len(p.allowedKinds))
	for kind := range p.allowedKinds {
		kinds = append(kinds, kind)
	}
	sort.Slice(kinds, func(i, j int) bool {
		return kinds[i] < kinds[j]
	})
	return kinds
}

func (p TeamWorkerProof) allows(kind AuthorizationKind) bool {
	_, ok := p.allowedKinds[kind]
	return ok
}

func (p TeamWorkerProof) clone() TeamWorkerProof {
	return NewTeamWorkerProof(p.State, p.Enabled, p.AllowedKinds())
}

type ScopedReferenceKind string

const (
	ScopedCatalog        ScopedReferenceKind = "catalog"
	ScopedSchedule       ScopedReferenceKind = "schedule"
	ScopedDeliveryTarget ScopedReferenceKind = "delivery_target"
)

type ScopedReferenceKey struct {
	Kind ScopedReferenceKind
	ID   string
}

type AuthorizationSnapshot struct {
	WorkspaceID string
	TeamID      string
	TeamState   ProofState

	workers    map[string]TeamWorkerProof
	references map[ScopedReferenceKey]ProofState
}

func NewAuthorizationSnapshot(
	workspaceID string,
	teamID string,
	teamState ProofState,
	workers map[string]TeamWorkerProof,
	references map[ScopedReferenceKey]ProofState,
) AuthorizationSnapshot {
	workerCopy := make(map[string]TeamWorkerProof, len(workers))
	for agentID, proof := range workers {
		workerCopy[agentID] = proof.clone()
	}
	referenceCopy := make(map[ScopedReferenceKey]ProofState, len(references))
	for key, state := range references {
		referenceCopy[key] = state
	}
	return AuthorizationSnapshot{
		WorkspaceID: workspaceID,
		TeamID:      teamID,
		TeamState:   teamState,
		workers:     workerCopy,
		references:  referenceCopy,
	}
}

func (s AuthorizationSnapshot) TeamWorker(agentID string) (TeamWorkerProof, bool) {
	proof, ok := s.workers[agentID]
	return proof.clone(), ok
}

func (s AuthorizationSnapshot) Reference(key ScopedReferenceKey) (ProofState, bool) {
	state, ok := s.references[key]
	return state, ok
}

func (s AuthorizationSnapshot) Workers() map[string]TeamWorkerProof {
	workers := make(map[string]TeamWorkerProof, len(s.workers))
	for agentID, proof := range s.workers {
		workers[agentID] = proof.clone()
	}
	return workers
}

func (s AuthorizationSnapshot) References() map[ScopedReferenceKey]ProofState {
	references := make(map[ScopedReferenceKey]ProofState, len(s.references))
	for key, state := range s.references {
		references[key] = state
	}
	return references
}

type AgentProofState string

const (
	AgentProofResolved   AgentProofState = "resolved"
	AgentProofNotFound   AgentProofState = "not_found"
	AgentProofCorrupt    AgentProofState = "corrupt"
	AgentProofUnprovable AgentProofState = "unprovable"
	AgentProofDeferred   AgentProofState = "deferred_agent"
)

type AgentVersionProof struct {
	state                 AgentProofState
	factoryKey            FactoryKey
	outputSchema          json.RawMessage
	hasSubAgents          bool
	hasInternalWorkerStep bool
}

func NewAgentVersionProof(
	state AgentProofState,
	factoryKey FactoryKey,
	outputSchema json.RawMessage,
	hasSubAgents bool,
	hasInternalWorkerStep bool,
) AgentVersionProof {
	return AgentVersionProof{
		state:                 state,
		factoryKey:            factoryKey,
		outputSchema:          cloneRawMessage(outputSchema),
		hasSubAgents:          hasSubAgents,
		hasInternalWorkerStep: hasInternalWorkerStep,
	}
}

func (p AgentVersionProof) State() AgentProofState {
	return p.state
}

func (p AgentVersionProof) FactoryKey() FactoryKey {
	return p.factoryKey
}

func (p AgentVersionProof) OutputSchema() json.RawMessage {
	return cloneRawMessage(p.outputSchema)
}

func (p AgentVersionProof) HasSubAgents() bool {
	return p.hasSubAgents
}

func (p AgentVersionProof) HasInternalWorkerStep() bool {
	return p.hasInternalWorkerStep
}

func cloneRawMessage(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	cloned := make(json.RawMessage, len(raw))
	copy(cloned, raw)
	return cloned
}

type DependencyProofState string

const (
	DependencyProofResolved   DependencyProofState = "resolved"
	DependencyProofUnprovable DependencyProofState = "unprovable"
)

type DependencyFailureReason string

const (
	DependencyUnenumerable                 DependencyFailureReason = "dependency_unenumerable"
	DependencySkillVersionRequired         DependencyFailureReason = "skill_version_required"
	DependencyProviderRevisionRequired     DependencyFailureReason = "provider_revision_required"
	DependencyCredentialUnavailable        DependencyFailureReason = "credential_unavailable"
	DependencyCredentialVersionUnsupported DependencyFailureReason = "credential_version_unsupported"
	DependencyVersionRequired              DependencyFailureReason = "dependency_version_required"
	DependencyNotFound                     DependencyFailureReason = "dependency_not_found"
	DependencyCorrupt                      DependencyFailureReason = "dependency_corrupt"
	DependencyFrozenIncomplete             DependencyFailureReason = "frozen_dependency_incomplete"
)

type DependencyFailure struct {
	Reason            DependencyFailureReason
	DependencyType    string
	DependencyKey     string
	DependencyVersion *int64
}

type DependencyProof struct {
	state    DependencyProofState
	failures []DependencyFailure
}

func NewDependencyProof(
	state DependencyProofState,
	failures []DependencyFailure,
) DependencyProof {
	copied := cloneDependencyFailures(failures)
	sort.Slice(copied, func(i, j int) bool {
		left, right := copied[i], copied[j]
		if left.DependencyType != right.DependencyType {
			return left.DependencyType < right.DependencyType
		}
		if left.DependencyKey != right.DependencyKey {
			return left.DependencyKey < right.DependencyKey
		}
		if comparison := compareOptionalInt64(
			left.DependencyVersion,
			right.DependencyVersion,
		); comparison != 0 {
			return comparison < 0
		}
		return left.Reason < right.Reason
	})
	return DependencyProof{state: state, failures: copied}
}

func (p DependencyProof) State() DependencyProofState {
	return p.state
}

func (p DependencyProof) Failures() []DependencyFailure {
	return cloneDependencyFailures(p.failures)
}

func (p DependencyProof) clone() DependencyProof {
	return NewDependencyProof(p.state, p.failures)
}

type DependencySnapshot struct {
	agents          map[AgentVersionKey]DependencyProof
	workflow        DependencyProof
	workflowPresent bool
}

func NewDependencySnapshot(
	agents map[AgentVersionKey]DependencyProof,
	workflow DependencyProof,
) DependencySnapshot {
	copied := make(map[AgentVersionKey]DependencyProof, len(agents))
	for key, proof := range agents {
		copied[key] = proof.clone()
	}
	return DependencySnapshot{
		agents:          copied,
		workflow:        workflow.clone(),
		workflowPresent: true,
	}
}

func (s DependencySnapshot) Agent(key AgentVersionKey) (DependencyProof, bool) {
	proof, ok := s.agents[key]
	return proof.clone(), ok
}

func (s DependencySnapshot) Agents() map[AgentVersionKey]DependencyProof {
	copied := make(map[AgentVersionKey]DependencyProof, len(s.agents))
	for key, proof := range s.agents {
		copied[key] = proof.clone()
	}
	return copied
}

func (s DependencySnapshot) Workflow() (DependencyProof, bool) {
	return s.workflow.clone(), s.workflowPresent
}

type FactoryProofState string

const (
	FactoryProofResolved   FactoryProofState = "resolved"
	FactoryProofUnprovable FactoryProofState = "unprovable"
)

type FactoryFailureReason string

const (
	FactoryUnknown                    FactoryFailureReason = "factory_unknown"
	FactoryABIIncompatible            FactoryFailureReason = "factory_abi_incompatible"
	FactoryInputInvalid               FactoryFailureReason = "factory_input_invalid"
	FactoryFrozenDependencyUndeclared FactoryFailureReason = "frozen_dependency_undeclared"
	FactoryFrozenManifestMismatch     FactoryFailureReason = "frozen_manifest_mismatch"
	FactoryFrozenCapabilityMismatch   FactoryFailureReason = "frozen_capability_mismatch"
)

type FactoryFailure struct {
	Reason FactoryFailureReason
	Detail string
}

type FactoryProof struct {
	state    FactoryProofState
	failures []FactoryFailure
}

func NewFactoryProof(state FactoryProofState, failures []FactoryFailure) FactoryProof {
	copied := append([]FactoryFailure(nil), failures...)
	sort.Slice(copied, func(i, j int) bool {
		if copied[i].Reason != copied[j].Reason {
			return copied[i].Reason < copied[j].Reason
		}
		return copied[i].Detail < copied[j].Detail
	})
	return FactoryProof{state: state, failures: copied}
}

func (p FactoryProof) State() FactoryProofState {
	return p.state
}

func (p FactoryProof) Failures() []FactoryFailure {
	return append([]FactoryFailure(nil), p.failures...)
}

func (p FactoryProof) clone() FactoryProof {
	return NewFactoryProof(p.state, p.failures)
}

type FactorySnapshot struct {
	factories map[FactoryKey]FactoryProof
}

func NewFactorySnapshot(factories map[FactoryKey]FactoryProof) FactorySnapshot {
	copied := make(map[FactoryKey]FactoryProof, len(factories))
	for key, proof := range factories {
		copied[key] = proof.clone()
	}
	return FactorySnapshot{factories: copied}
}

func (s FactorySnapshot) Factory(key FactoryKey) (FactoryProof, bool) {
	proof, ok := s.factories[key]
	return proof.clone(), ok
}

func (s FactorySnapshot) Factories() map[FactoryKey]FactoryProof {
	copied := make(map[FactoryKey]FactoryProof, len(s.factories))
	for key, proof := range s.factories {
		copied[key] = proof.clone()
	}
	return copied
}

func cloneDependencyFailures(failures []DependencyFailure) []DependencyFailure {
	copied := make([]DependencyFailure, len(failures))
	for index, failure := range failures {
		copied[index] = failure
		if failure.DependencyVersion != nil {
			version := *failure.DependencyVersion
			copied[index].DependencyVersion = &version
		}
	}
	return copied
}

func compareOptionalInt64(left, right *int64) int {
	switch {
	case left == nil && right == nil:
		return 0
	case left == nil:
		return -1
	case right == nil:
		return 1
	case *left < *right:
		return -1
	case *left > *right:
		return 1
	default:
		return 0
	}
}

// ReferencedBundle is a deterministic index entry for a graph-referenced
// bundle.
type ReferencedBundle struct {
	Key            AgentVersionKey
	Anchor         string
	NodeID         string
	Executable     bool
	ParallelBranch bool
	RequiresLeaf   bool
	LeafAnchor     string
	LeafNodeID     string
}

// ReferencedBundles returns one item per exact bundle in deterministic
// anchor order. The lead is always first; repeated node references retain the
// first node anchor while accumulating parallel-branch restrictions.
func ReferencedBundles(lead AgentVersionKey, graph GraphDefinition) []ReferencedBundle {
	branchTargets := make(map[string]struct{})
	for _, edge := range graph.Edges {
		if edge.Route == RouteBranch {
			branchTargets[edge.ToNodeID] = struct{}{}
		}
	}

	leadExecutable := false
	for _, node := range graph.Nodes {
		if node.Type == NodeLead {
			leadExecutable = true
			break
		}
	}
	bundles := []ReferencedBundle{{
		Key:        lead,
		Anchor:     "/lead_agent",
		Executable: leadExecutable,
	}}
	byKey := map[AgentVersionKey]int{lead: 0}

	for index, node := range graph.Nodes {
		var key AgentVersionKey
		switch config := node.Config.(type) {
		case WorkerConfig:
			if node.Type != NodeWorker {
				continue
			}
			key = AgentVersionKey{AgentID: config.AgentID, AgentVersion: config.AgentVersion}
		case HandoffConfig:
			if node.Type != NodeHandoff {
				continue
			}
			key = AgentVersionKey{AgentID: config.AgentID, AgentVersion: config.AgentVersion}
		default:
			continue
		}

		_, parallelBranch := branchTargets[node.ID]
		if existing, ok := byKey[key]; ok {
			bundles[existing].Executable = true
			bundles[existing].ParallelBranch = bundles[existing].ParallelBranch || parallelBranch
			bundles[existing].RequiresLeaf = true
			if bundles[existing].LeafAnchor == "" {
				bundles[existing].LeafAnchor = nodeConfigFieldPath(index, "agent_version")
				bundles[existing].LeafNodeID = node.ID
			}
			continue
		}
		byKey[key] = len(bundles)
		bundles = append(bundles, ReferencedBundle{
			Key:            key,
			Anchor:         nodeConfigFieldPath(index, "agent_version"),
			NodeID:         node.ID,
			Executable:     true,
			ParallelBranch: parallelBranch,
			RequiresLeaf:   true,
			LeafAnchor:     nodeConfigFieldPath(index, "agent_version"),
			LeafNodeID:     node.ID,
		})
	}
	return bundles
}

// ExecutionBundles returns only the exact AgentVersions that the graph can
// execute. The team lead remains part of ReferencedBundles for immutable
// identity and authorization proof, but it is not an execution dependency
// unless the graph contains a lead node or explicitly targets that same agent.
func ExecutionBundles(lead AgentVersionKey, graph GraphDefinition) []ReferencedBundle {
	referenced := ReferencedBundles(lead, graph)
	executable := make([]ReferencedBundle, 0, len(referenced))
	for _, bundle := range referenced {
		if bundle.Executable {
			executable = append(executable, bundle)
		}
	}
	return executable
}

func nodeConfigFieldPath(index int, field string) string {
	return joinPath(joinPath(nodePath(index), "config"), field)
}

func nodePath(index int) string {
	return fmt.Sprintf("/nodes/%d", index)
}
