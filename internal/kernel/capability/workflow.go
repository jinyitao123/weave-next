package capability

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

// ValidateWorkflowSource admits only a frozen, local workflow that executes
// referenced Loom model members and deterministic graph nodes. Resource and
// team authorization remain the caller's responsibility.
func ValidateWorkflowSource(envelope frozen.ArtifactEnvelopeV1) error {
	payload, err := frozen.DecodeArtifactEnvelopeV1(envelope)
	if err != nil {
		return fmt.Errorf("decode frozen workflow source: %w", err)
	}
	if payload.Team.WorkspaceID != envelope.WorkspaceID || payload.Team.TeamID == "" || payload.Team.LeadAgentID == "" {
		return errors.New("workflow source identity is incomplete")
	}

	trigger, triggerReport := machine.DecodeTriggerConfigV1(payload.TriggerConfig)
	if triggerReport != nil && len(triggerReport.Issues) != 0 {
		return errors.New("workflow source trigger is invalid")
	}
	if trigger.Type != machine.TriggerConversationExplicit || trigger.Delivery != nil {
		return errors.New("workflow source requires an explicit conversation trigger without delivery")
	}
	if len(payload.DeliveryTargets) != 0 {
		return errors.New("workflow source cannot declare external delivery targets")
	}

	graph, graphReport := machine.DecodeGraphDefinitionV1(payload.GraphDefinition)
	if graphReport != nil && len(graphReport.Issues) != 0 {
		return errors.New("workflow source graph is invalid")
	}
	if graph.OutputContract.Type == machine.ValueJSON {
		if len(graph.OutputContract.Schema) == 0 || len(machine.ValidateRuntimeInputSchema(graph.OutputContract.Schema)) != 0 {
			return errors.New("workflow source JSON output contract must have a valid schema")
		}
	}

	allowedNode := map[machine.NodeType]bool{
		machine.NodeLead: true, machine.NodeWorker: true, machine.NodeTransform: true,
		machine.NodeCondition: true, machine.NodeParallel: true, machine.NodeJoin: true,
		machine.NodeDeliver: true,
	}
	deliveries := 0
	for _, node := range graph.Nodes {
		if !allowedNode[node.Type] {
			return fmt.Errorf("workflow source cannot contain %s nodes", node.Type)
		}
		switch node.Type {
		case machine.NodeDeliver:
			deliveries++
		}
	}
	if deliveries != 1 {
		return errors.New("workflow source requires exactly one local delivery")
	}

	lead := machine.AgentVersionKey{AgentID: payload.Team.LeadAgentID, AgentVersion: payload.Team.LeadAgentVersion}
	for _, node := range graph.Nodes {
		if node.Type == machine.NodeLead && lead.AgentVersion < 1 {
			return errors.New("workflow source lead version is missing")
		}
	}
	bundles := make(map[machine.AgentVersionKey]frozen.FrozenExecutionBundle, len(payload.Bundles))
	for _, bundle := range payload.Bundles {
		key := machine.AgentVersionKey{AgentID: bundle.Agent.AgentID, AgentVersion: bundle.Agent.AgentVersion}
		bundles[key] = bundle
	}

	executable := machine.ExecutionBundles(lead, graph)
	capabilities := make(map[machine.AgentVersionKey]machine.CapabilityProof, len(executable))
	for _, reference := range executable {
		bundle, ok := bundles[reference.Key]
		if !ok {
			return errors.New("workflow source references a member without a frozen bundle")
		}
		if err := validatePureModelBundle(envelope.WorkspaceID, bundle); err != nil {
			return err
		}
		manifest := bundle.Capability
		capabilities[reference.Key] = machine.NewCapabilityProof(
			machine.ProofResolved,
			manifest.MayYield,
			manifest.InteractiveStepIDs,
			manifest.InteractiveToolIDs,
			manifest.MayInvokeAgent,
			manifest.AgentStepIDs,
		)
	}

	// Machine validation owns topology, routes, joins, conditions, value
	// references, and the executable output-schema subset. Its final resource
	// authorization phase needs live caller proofs, which this pure source check
	// intentionally does not manufacture.
	report := machine.Validate(machine.ValidationContext{
		WorkspaceID:  envelope.WorkspaceID,
		TeamID:       payload.Team.TeamID,
		Lead:         lead,
		Trigger:      trigger,
		Graph:        graph,
		Capabilities: capabilities,
	})
	if len(report.Issues) == 0 {
		return nil
	}
	if len(report.Issues) == 1 && report.Issues[0].Phase == machine.PhaseAuthorization &&
		report.Issues[0].Path == "/authorization" && report.Issues[0].Code == machine.CodeAuthorizationProofMissing {
		return nil
	}
	return errors.New("workflow source graph fails machine validation")
}

func validatePureModelBundle(workspaceID string, bundle frozen.FrozenExecutionBundle) error {
	agent := bundle.Agent
	if agent.WorkspaceID != workspaceID || agent.Engine != "loom" || agent.Model == "" ||
		agent.RuntimeID != "" || bundle.Runtime != nil || bundle.PrimaryModel.ModelID != agent.Model ||
		bundle.PrimaryModel.ProviderID == "" || bundle.PrimaryModel.ProviderRevision < 1 ||
		bundle.PrimaryModel.CredentialRef.Scope != frozen.CredentialScopeWorkspaceService ||
		bundle.PrimaryModel.CredentialRef.UserID != "" || bundle.PrimaryModel.CredentialRef.ServiceID == "" ||
		agent.GraphType != "standard" || bundle.FactoryKey.FactoryID != "standard" ||
		bundle.FactoryKey.CompilerABI != "weave-graph-abi-v1" {
		return errors.New("workflow source members must use the standard Loom model engine and a workspace service credential")
	}
	switch bundle.FactoryKey.FactoryVersion {
	case "1":
		if !bytes.Equal(agent.FactoryInput, []byte(`{}`)) {
			return errors.New("workflow source member has unsupported factory input")
		}
	case "2":
		input, err := frozen.DecodeStandardFactoryInputV2(agent.FactoryInput)
		if err != nil || len(input.MCPServers) != 0 {
			return errors.New("workflow source member cannot declare MCP servers")
		}
	default:
		return errors.New("workflow source member factory version is unsupported")
	}
	if len(agent.Fallback.Models) != 0 || len(bundle.FallbackModels) != 0 || agent.Fallback.Retries != 0 {
		return errors.New("workflow source members cannot have fallback models")
	}
	if len(bundle.Skills) != 0 || len(bundle.MCPBindings) != 0 {
		return errors.New("workflow source members cannot use skills or MCP")
	}
	if agent.MemoryConfig != nil && (agent.MemoryConfig.Enabled || agent.MemoryConfig.AutoRemember) || len(agent.MemorySlots) != 0 {
		return errors.New("workflow source members cannot use memory")
	}
	denyAll := false
	for _, denied := range agent.Permissions.Deny {
		denyAll = denyAll || denied == "*"
	}
	if !denyAll || len(agent.Permissions.Allow) != 0 || len(agent.Permissions.Ask) != 0 {
		return errors.New("workflow source members require deny-all permissions without allow or ask rules")
	}
	if len(agent.BusinessCapabilityIDs) != 0 || len(agent.BusinessCapabilityBindings) != 0 {
		return errors.New("workflow source members cannot have business capabilities")
	}
	if agent.Limits.ToolLoopControl != nil {
		return errors.New("workflow source members cannot have tool-loop execution")
	}
	manifest := bundle.Capability
	if manifest.Role != agent.Role || manifest.MayYield || manifest.MayInvokeAgent ||
		len(manifest.InteractiveStepIDs) != 0 || len(manifest.InteractiveToolIDs) != 0 || len(manifest.AgentStepIDs) != 0 {
		return errors.New("workflow source members cannot yield, invoke agents, or use interactive capabilities")
	}
	if err := validateModelDependencies(agent, bundle); err != nil {
		return err
	}
	for _, reference := range bundle.Credentials {
		if !sameCredential(reference, bundle.PrimaryModel.CredentialRef) {
			return errors.New("workflow source member has a non-model credential")
		}
	}
	return nil
}

func validateModelDependencies(agent frozen.FrozenAgentRecord, bundle frozen.FrozenExecutionBundle) error {
	modelDependency := false
	for _, dependency := range bundle.Dependencies.Dependencies {
		if dependency.OwnerType != "agent" || dependency.OwnerID != agent.AgentID || dependency.OwnerAgentVersion == nil || *dependency.OwnerAgentVersion != agent.AgentVersion {
			return errors.New("workflow source member dependency owner is invalid")
		}
		switch dependency.DependencyType {
		case "agent":
			if dependency.DependencyKey != agent.AgentID || dependency.DependencyVersion == nil || *dependency.DependencyVersion != agent.AgentVersion {
				return errors.New("workflow source member cannot depend on another agent")
			}
		case "model_binding":
			if dependency.DependencyKey != bundle.PrimaryModel.ModelID || dependency.DependencyVersion == nil || *dependency.DependencyVersion != bundle.PrimaryModel.ProviderRevision {
				return errors.New("workflow source member model dependency is invalid")
			}
			modelDependency = true
		default:
			return errors.New("workflow source members cannot use additional dependencies")
		}
	}
	if !modelDependency {
		return errors.New("workflow source member provider dependency is missing")
	}
	return nil
}

func sameCredential(left, right frozen.CredentialReference) bool {
	if left.SchemaVersion != right.SchemaVersion || left.Scope != right.Scope || left.UserID != right.UserID ||
		left.ServiceID != right.ServiceID || left.WorkspaceID != right.WorkspaceID || left.Kind != right.Kind ||
		left.ResourceID != right.ResourceID || left.Slot != right.Slot {
		return false
	}
	if left.CredentialVersion == nil || right.CredentialVersion == nil {
		return left.CredentialVersion == nil && right.CredentialVersion == nil
	}
	return *left.CredentialVersion == *right.CredentialVersion
}
