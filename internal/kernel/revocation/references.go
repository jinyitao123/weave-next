// Package revocation contains control-plane facts shared by roster writers and
// workflow readers without introducing a registry/workflow import cycle.
package revocation

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

// Reference is one TeamWorker authorization actually used by a graph node.
type Reference struct {
	WorkerAgentID string `json:"worker_agent_id"`
	Kind          string `json:"kind"`
}

// ExtractGraphReferences strictly decodes a workflow graph and returns its
// stable, unique TeamWorker authorization references.
func ExtractGraphReferences(raw json.RawMessage) ([]Reference, error) {
	graph, report := machine.DecodeGraphDefinitionV1(raw)
	if report != nil && len(report.Issues) > 0 {
		issue := report.Issues[0]
		return nil, fmt.Errorf(
			"decode workflow graph reference at %s: %s",
			issue.Path,
			issue.Code,
		)
	}

	references := make([]Reference, 0)
	seen := make(map[Reference]struct{})
	for _, node := range graph.Nodes {
		var reference Reference
		switch node.Type {
		case machine.NodeWorker:
			config, ok := node.Config.(machine.WorkerConfig)
			if !ok {
				return nil, fmt.Errorf("worker node %q has unexpected config %T", node.ID, node.Config)
			}
			reference = Reference{WorkerAgentID: config.AgentID, Kind: string(config.Kind)}
		case machine.NodeHandoff:
			config, ok := node.Config.(machine.HandoffConfig)
			if !ok {
				return nil, fmt.Errorf("handoff node %q has unexpected config %T", node.ID, node.Config)
			}
			reference = Reference{WorkerAgentID: config.AgentID, Kind: "handoff"}
		default:
			continue
		}
		if reference.WorkerAgentID == "" || reference.Kind == "" {
			return nil, fmt.Errorf("workflow node %q has incomplete TeamWorker reference", node.ID)
		}
		if _, duplicate := seen[reference]; duplicate {
			continue
		}
		seen[reference] = struct{}{}
		references = append(references, reference)
	}

	sort.Slice(references, func(i, j int) bool {
		if references[i].WorkerAgentID != references[j].WorkerAgentID {
			return references[i].WorkerAgentID < references[j].WorkerAgentID
		}
		return referenceKindRank(references[i].Kind) < referenceKindRank(references[j].Kind)
	})
	return references, nil
}

func referenceKindRank(kind string) int {
	switch kind {
	case "consult":
		return 0
	case "dispatch":
		return 1
	case "handoff":
		return 2
	default:
		return 3
	}
}
