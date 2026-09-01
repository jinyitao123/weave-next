package machine

// OwningParallelNode returns the parallel node that feeds joinNodeID and
// contains at least one candidate node in its reachable segment.
func OwningParallelNode(graph GraphDefinition, joinNodeID string, candidateNodeIDs []string) string {
	candidates := make(map[string]struct{}, len(candidateNodeIDs))
	for _, nodeID := range candidateNodeIDs {
		candidates[nodeID] = struct{}{}
	}
	adjacent := make(map[string][]string)
	for _, edge := range graph.Edges {
		adjacent[edge.FromNodeID] = append(adjacent[edge.FromNodeID], edge.ToNodeID)
	}
	for _, node := range graph.Nodes {
		config, parallel := node.Config.(ParallelConfig)
		if !parallel || config.JoinNodeID != joinNodeID {
			continue
		}
		seen := map[string]struct{}{node.ID: {}}
		queue := []string{node.ID}
		for len(queue) > 0 {
			current := queue[0]
			queue = queue[1:]
			if _, matched := candidates[current]; matched {
				return node.ID
			}
			for _, next := range adjacent[current] {
				if _, present := seen[next]; present {
					continue
				}
				seen[next] = struct{}{}
				queue = append(queue, next)
			}
		}
	}
	return ""
}
