package machine

import (
	"sort"
)

func validateFactories(ctx ValidationContext) Report {
	var report Report
	keys := resolvedExecutionFactoryKeys(ctx)

	for _, key := range keys {
		path := factoryProofPath(key)
		proof, found := ctx.Factories.Factory(key)
		if !found {
			report.Add(
				PhaseFactories,
				path,
				CodeFactoryProofMissing,
				"exact factory proof is missing",
			)
			continue
		}

		switch proof.State() {
		case FactoryProofResolved:
			for _, failure := range proof.Failures() {
				code, ok := factoryFailureCode(failure.Reason)
				if !ok {
					report.Add(
						PhaseFactories,
						path,
						CodeFactoryUnprovable,
						"factory failure reason cannot be proven",
					)
					continue
				}
				report.Add(
					PhaseFactories,
					joinPath(path, string(failure.Reason)),
					code,
					"frozen factory validation failed",
				)
			}
		case FactoryProofUnprovable:
			report.Add(
				PhaseFactories,
				path,
				CodeFactoryUnprovable,
				"exact factory proof cannot be established",
			)
		default:
			report.Add(
				PhaseFactories,
				path,
				CodeFactoryProofMissing,
				"factory proof state is missing",
			)
		}
	}

	report.Sort()
	return report
}

func resolvedExecutionFactoryKeys(ctx ValidationContext) []FactoryKey {
	executionAgents := make(map[AgentVersionKey]AgentVersionProof)
	for _, bundle := range ExecutionBundles(ctx.Lead, ctx.Graph) {
		if proof, ok := ctx.Agents[bundle.Key]; ok {
			executionAgents[bundle.Key] = proof
		}
	}
	return resolvedFactoryKeys(executionAgents)
}

func resolvedFactoryKeys(agents map[AgentVersionKey]AgentVersionProof) []FactoryKey {
	unique := make(map[FactoryKey]struct{})
	for _, proof := range agents {
		if proof.State() == AgentProofResolved {
			unique[proof.FactoryKey()] = struct{}{}
		}
	}
	keys := make([]FactoryKey, 0, len(unique))
	for key := range unique {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].FactoryID != keys[j].FactoryID {
			return keys[i].FactoryID < keys[j].FactoryID
		}
		if keys[i].FactoryVersion != keys[j].FactoryVersion {
			return keys[i].FactoryVersion < keys[j].FactoryVersion
		}
		return keys[i].CompilerABI < keys[j].CompilerABI
	})
	return keys
}

func factoryFailureCode(reason FactoryFailureReason) (string, bool) {
	switch reason {
	case FactoryUnknown:
		return CodeFactoryUnknown, true
	case FactoryABIIncompatible:
		return CodeFactoryABIIncompatible, true
	case FactoryInputInvalid:
		return CodeFactoryInputInvalid, true
	case FactoryFrozenDependencyUndeclared:
		return CodeFrozenDependencyUndeclared, true
	case FactoryFrozenManifestMismatch:
		return CodeFrozenManifestMismatch, true
	case FactoryFrozenCapabilityMismatch:
		return CodeFrozenCapabilityMismatch, true
	default:
		return "", false
	}
}

func factoryProofPath(key FactoryKey) string {
	path := joinPath("/proofs/factories", key.FactoryID)
	path = joinPath(path, key.FactoryVersion)
	return joinPath(path, key.CompilerABI)
}
