package machine

import (
	"fmt"
)

func validateDependencies(ctx ValidationContext) Report {
	var report Report

	for _, bundle := range ExecutionBundles(ctx.Lead, ctx.Graph) {
		path := dependencyAgentPath(bundle.Key)
		proof, found := ctx.Dependencies.Agent(bundle.Key)
		if !found {
			report.AddNode(
				PhaseDependencies,
				path,
				bundle.NodeID,
				CodeDependencyProofMissing,
				"exact AgentVersion dependency proof is missing",
			)
			continue
		}
		addDependencyProofIssues(&report, path, bundle.NodeID, proof)
	}

	const workflowPath = "/proofs/workflow/dependencies"
	workflow, found := ctx.Dependencies.Workflow()
	if !found {
		report.Add(
			PhaseDependencies,
			workflowPath,
			CodeDependencyProofMissing,
			"workflow dependency proof is missing",
		)
	} else {
		addDependencyProofIssues(&report, workflowPath, "", workflow)
	}

	report.Sort()
	return report
}

func addDependencyProofIssues(
	report *Report,
	basePath string,
	nodeID string,
	proof DependencyProof,
) {
	switch proof.State() {
	case DependencyProofResolved:
		for _, failure := range proof.Failures() {
			code, ok := dependencyFailureCode(failure.Reason)
			if !ok {
				report.AddNode(
					PhaseDependencies,
					basePath,
					nodeID,
					CodeDependencyUnprovable,
					"dependency failure reason cannot be proven",
				)
				continue
			}
			path := joinPath(joinPath(basePath, failure.DependencyType), failure.DependencyKey)
			report.AddNode(
				PhaseDependencies,
				path,
				nodeID,
				code,
				"frozen dependency validation failed",
			)
		}
	case DependencyProofUnprovable:
		report.AddNode(
			PhaseDependencies,
			basePath,
			nodeID,
			CodeDependencyUnprovable,
			"dependency proof cannot be established",
		)
	default:
		report.AddNode(
			PhaseDependencies,
			basePath,
			nodeID,
			CodeDependencyProofMissing,
			"dependency proof state is missing",
		)
	}
}

func dependencyFailureCode(reason DependencyFailureReason) (string, bool) {
	switch reason {
	case DependencyUnenumerable:
		return CodeDependencyUnenumerable, true
	case DependencySkillVersionRequired:
		return CodeSkillVersionRequired, true
	case DependencyProviderRevisionRequired:
		return CodeProviderRevisionRequired, true
	case DependencyCredentialUnavailable:
		return CodeCredentialUnavailable, true
	case DependencyCredentialVersionUnsupported:
		return CodeCredentialVersionUnsupported, true
	case DependencyVersionRequired:
		return CodeDependencyVersionRequired, true
	case DependencyNotFound:
		return CodeDependencyNotFound, true
	case DependencyCorrupt:
		return CodeDependencyCorrupt, true
	case DependencyFrozenIncomplete:
		return CodeFrozenDependencyIncomplete, true
	default:
		return "", false
	}
}

func dependencyAgentPath(key AgentVersionKey) string {
	path := joinPath("/proofs/agents", key.AgentID)
	path = joinPath(path, "versions")
	path = joinPath(path, fmt.Sprintf("%d", key.AgentVersion))
	return joinPath(path, "dependencies")
}
