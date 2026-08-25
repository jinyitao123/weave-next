package machine

type agentOutputContractResult struct {
	contract parsedOutputContract
	valid    bool
}

func validateAgentVersions(ctx ValidationContext) Report {
	var report Report
	bundles := ReferencedBundles(ctx.Lead, ctx.Graph)

	for index, bundle := range bundles {
		path := bundle.Anchor
		if index == 0 {
			path = "/lead_agent/agent_version"
		}
		proof, found := ctx.Agents[bundle.Key]
		if !found {
			report.AddNode(
				PhaseAgentVersions,
				path,
				bundle.NodeID,
				CodeAgentVersionProofMissing,
				"exact AgentVersion proof is missing",
			)
			continue
		}

		switch proof.State() {
		case AgentProofResolved:
			if !bundle.RequiresLeaf {
				continue
			}
			if proof.HasSubAgents() {
				report.AddNode(
					PhaseAgentVersions,
					bundle.LeafAnchor,
					bundle.LeafNodeID,
					CodeAgentSubAgentsForbidden,
					"team worker and handoff AgentVersion must not define sub_agents",
				)
			}
			if proof.HasInternalWorkerStep() {
				report.AddNode(
					PhaseAgentVersions,
					bundle.LeafAnchor,
					bundle.LeafNodeID,
					CodeAgentWorkerStepForbidden,
					"team worker and handoff AgentVersion must not contain an internal worker step",
				)
			}
		case AgentProofNotFound:
			report.AddNode(
				PhaseAgentVersions,
				path,
				bundle.NodeID,
				CodeAgentVersionNotFound,
				"exact AgentVersion was not found",
			)
		case AgentProofCorrupt:
			report.AddNode(
				PhaseAgentVersions,
				path,
				bundle.NodeID,
				CodeAgentVersionCorrupt,
				"exact AgentVersion is corrupt",
			)
		case AgentProofUnprovable, AgentProofDeferred:
			report.AddNode(
				PhaseAgentVersions,
				path,
				bundle.NodeID,
				CodeAgentVersionUnprovable,
				"exact AgentVersion cannot be proven",
			)
		default:
			report.AddNode(
				PhaseAgentVersions,
				path,
				bundle.NodeID,
				CodeAgentVersionProofMissing,
				"exact AgentVersion proof state is missing",
			)
		}
	}

	validateHandoffOutputContracts(&report, ctx)
	report.Sort()
	return report
}

func validateHandoffOutputContracts(report *Report, ctx ValidationContext) {
	target, targetValid := parseOutputContractProblems(ctx.Graph.OutputContract)
	if !targetValid {
		// Phase 8 owns declared workflow contract errors.
		return
	}

	cache := make(map[AgentVersionKey]agentOutputContractResult)
	for index, node := range ctx.Graph.Nodes {
		config, ok := node.Config.(HandoffConfig)
		if node.Type != NodeHandoff || !ok {
			continue
		}
		key := AgentVersionKey{AgentID: config.AgentID, AgentVersion: config.AgentVersion}
		proof, found := ctx.Agents[key]
		if !found || proof.State() != AgentProofResolved {
			continue
		}

		result, cached := cache[key]
		if !cached {
			result = deriveAgentOutputContract(proof)
			cache[key] = result
		}
		path := nodeConfigFieldPath(index, "agent_version")
		if !result.valid {
			report.AddNode(
				PhaseAgentVersions,
				path,
				node.ID,
				CodeAgentOutputSchemaInvalid,
				"exact AgentVersion output_schema is invalid for the fixed v1 schema subset",
			)
			continue
		}
		if !outputContractsCompatible(result.contract, target) {
			report.AddNode(
				PhaseAgentVersions,
				path,
				node.ID,
				CodeHandoffOutputContractIncompatible,
				"handoff AgentVersion output contract is incompatible with workflow output_contract",
			)
		}
	}
}

func deriveAgentOutputContract(proof AgentVersionProof) agentOutputContractResult {
	raw := proof.OutputSchema()
	if raw == nil {
		return agentOutputContractResult{
			contract: parsedOutputContract{Type: ValueText},
			valid:    true,
		}
	}
	schema, problems := parseFixedJSONSchemaProblems(raw, "", nil)
	if len(problems) != 0 || schema == nil {
		return agentOutputContractResult{}
	}
	return agentOutputContractResult{
		contract: parsedOutputContract{Type: ValueJSON, Schema: schema},
		valid:    true,
	}
}

func parseOutputContractProblems(contract OutputContract) (parsedOutputContract, bool) {
	result := parsedOutputContract{Type: contract.Type}
	if contract.Type != ValueJSON || len(contract.Schema) == 0 {
		return result, true
	}
	schema, problems := parseFixedJSONSchemaProblems(contract.Schema, "", nil)
	if len(problems) != 0 || schema == nil {
		return parsedOutputContract{}, false
	}
	result.Schema = schema
	return result, true
}
