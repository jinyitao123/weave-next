package machine

import "fmt"

func validateTriggerCapabilities(ctx ValidationContext) Report {
	var report Report
	sessionless := isSessionlessTrigger(ctx.Trigger.Type)

	if sessionless {
		for index, node := range ctx.Graph.Nodes {
			if node.Type != NodeWait && node.Type != NodeHandoff {
				continue
			}
			report.AddNode(
				PhaseTriggerCapabilities,
				joinPath(nodePath(index), "type"),
				node.ID,
				CodeSessionRequired,
				fmt.Sprintf("%s requires a session trigger", node.Type),
			)
		}
	}

	for _, bundle := range ExecutionBundles(ctx.Lead, ctx.Graph) {
		proof, found := ctx.Capabilities[bundle.Key]
		if !found {
			report.AddNode(
				PhaseTriggerCapabilities,
				bundle.Anchor,
				bundle.NodeID,
				CodeCapabilityProofMissing,
				"exact bundle capability proof is missing",
			)
			continue
		}

		switch proof.State {
		case ProofResolved:
			if (sessionless || bundle.ParallelBranch) && proof.hasInteractiveCapability() {
				report.AddNode(
					PhaseTriggerCapabilities,
					bundle.Anchor,
					bundle.NodeID,
					CodeInteractiveCapabilityForbidden,
					"bundle capability must be non-interactive in this execution context",
				)
			}
			if proof.hasCrossAgentCapability() {
				report.AddNode(
					PhaseTriggerCapabilities,
					bundle.Anchor,
					bundle.NodeID,
					CodeCrossAgentCapabilityForbidden,
					"team bundle must not invoke another agent",
				)
			}
		case ProofUnprovable:
			report.AddNode(
				PhaseTriggerCapabilities,
				bundle.Anchor,
				bundle.NodeID,
				CodeCapabilityUnprovable,
				"bundle capability cannot be proven",
			)
		case ProofDeferredAgent, ProofDeferredDependencies, ProofDeferredFactory,
			ProofNotFound, ProofCorrupt:
			// The owning phase reports the root Agent, dependency, or factory
			// failure. Phase 9 must not replace it with a capability symptom.
		default:
			report.AddNode(
				PhaseTriggerCapabilities,
				bundle.Anchor,
				bundle.NodeID,
				CodeCapabilityUnprovable,
				"bundle capability proof has an unknown state",
			)
		}
	}

	report.Sort()
	return report
}

func isSessionlessTrigger(triggerType TriggerType) bool {
	return triggerType == TriggerSchedule || triggerType == TriggerAPI || triggerType == TriggerEvent
}
