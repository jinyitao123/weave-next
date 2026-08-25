package machine

import (
	"fmt"
	"math/bits"
)

type nodeBitSet []uint64

func newNodeBitSet(size int) nodeBitSet {
	return make(nodeBitSet, (size+63)/64)
}

func (set nodeBitSet) add(index int) {
	set[index/64] |= uint64(1) << uint(index%64)
}

func (set nodeBitSet) contains(index int) bool {
	return set[index/64]&(uint64(1)<<uint(index%64)) != 0
}

func (set nodeBitSet) clone() nodeBitSet {
	return append(nodeBitSet(nil), set...)
}

func (set nodeBitSet) intersect(other nodeBitSet) {
	for index := range set {
		set[index] &= other[index]
	}
}

type controlFlowAnalysis struct {
	nodeIndex        map[string]int
	outgoing         [][]int
	incoming         [][]int
	forwardOutgoing  [][]int
	forwardIncoming  [][]int
	topologicalOrder []int
	forwardAcyclic   bool
	dominators       []nodeBitSet
	dominatorWordOps int
}

func buildControlFlowAnalysis(graph GraphDefinition) controlFlowAnalysis {
	nodeCount := len(graph.Nodes)
	analysis := controlFlowAnalysis{
		nodeIndex:       make(map[string]int, nodeCount),
		outgoing:        make([][]int, nodeCount),
		incoming:        make([][]int, nodeCount),
		forwardOutgoing: make([][]int, nodeCount),
		forwardIncoming: make([][]int, nodeCount),
	}
	for index, node := range graph.Nodes {
		analysis.nodeIndex[node.ID] = index
	}

	indegree := make([]int, nodeCount)
	for edgeIndex, edge := range graph.Edges {
		from := analysis.nodeIndex[edge.FromNodeID]
		to := analysis.nodeIndex[edge.ToNodeID]
		analysis.outgoing[from] = append(analysis.outgoing[from], edgeIndex)
		analysis.incoming[to] = append(analysis.incoming[to], edgeIndex)
		if edge.Route == RouteBack {
			continue
		}
		analysis.forwardOutgoing[from] = append(analysis.forwardOutgoing[from], edgeIndex)
		analysis.forwardIncoming[to] = append(analysis.forwardIncoming[to], edgeIndex)
		indegree[to]++
	}

	queue := make([]int, 0, nodeCount)
	for index, degree := range indegree {
		if degree == 0 {
			queue = append(queue, index)
		}
	}
	for len(queue) != 0 {
		current := queue[0]
		queue = queue[1:]
		analysis.topologicalOrder = append(analysis.topologicalOrder, current)
		for _, edgeIndex := range analysis.forwardOutgoing[current] {
			next := analysis.nodeIndex[graph.Edges[edgeIndex].ToNodeID]
			indegree[next]--
			if indegree[next] == 0 {
				queue = append(queue, next)
			}
		}
	}
	analysis.forwardAcyclic = len(analysis.topologicalOrder) == nodeCount
	if !analysis.forwardAcyclic || nodeCount == 0 {
		return analysis
	}

	analysis.dominators = make([]nodeBitSet, nodeCount)
	entry := analysis.nodeIndex[graph.EntryNodeID]
	for _, nodeIndex := range analysis.topologicalOrder {
		if nodeIndex == entry {
			set := newNodeBitSet(nodeCount)
			set.add(nodeIndex)
			analysis.dominators[nodeIndex] = set
			continue
		}
		predecessors := analysis.forwardIncoming[nodeIndex]
		var set nodeBitSet
		if len(predecessors) == 0 {
			set = newNodeBitSet(nodeCount)
		} else {
			first := analysis.nodeIndex[graph.Edges[predecessors[0]].FromNodeID]
			set = analysis.dominators[first].clone()
			analysis.dominatorWordOps += len(set)
			for _, edgeIndex := range predecessors[1:] {
				previous := analysis.nodeIndex[graph.Edges[edgeIndex].FromNodeID]
				set.intersect(analysis.dominators[previous])
				analysis.dominatorWordOps += len(set)
			}
		}
		set.add(nodeIndex)
		analysis.dominators[nodeIndex] = set
	}
	return analysis
}

func (analysis controlFlowAnalysis) dominates(dominator, node int) bool {
	return node >= 0 && node < len(analysis.dominators) &&
		dominator >= 0 && dominator < len(analysis.dominators) &&
		analysis.dominators[node] != nil &&
		analysis.dominators[node].contains(dominator)
}

type naturalLoop struct {
	header  int
	latch   int
	members nodeBitSet
}

type naturalLoopFacts struct {
	loops           []naturalLoop
	bodyOwnerByNode []int
	loopByHeader    []int
}

// buildNaturalLoopFacts reuses the phase-five loop definition after that phase
// has accepted the graph. Each node lookup is O(1); ValueRef validation must not
// rescan all loops for every reference.
func buildNaturalLoopFacts(graph GraphDefinition, analysis controlFlowAnalysis) naturalLoopFacts {
	facts := naturalLoopFacts{
		bodyOwnerByNode: make([]int, len(graph.Nodes)),
		loopByHeader:    make([]int, len(graph.Nodes)),
	}
	for index := range facts.bodyOwnerByNode {
		facts.bodyOwnerByNode[index] = -1
		facts.loopByHeader[index] = -1
	}
	for header, node := range graph.Nodes {
		if node.Type != NodeLoop {
			continue
		}
		config, ok := node.Config.(LoopConfig)
		if !ok {
			continue
		}
		latch, latchFound := analysis.nodeIndex[config.LatchNodeID]
		if !latchFound {
			continue
		}
		bodyStart := -1
		for _, edgeIndex := range analysis.outgoing[header] {
			edge := graph.Edges[edgeIndex]
			if edge.Route == RouteBody {
				bodyStart = analysis.nodeIndex[edge.ToNodeID]
				break
			}
		}
		if bodyStart < 0 {
			continue
		}
		members := loopMembers(graph, analysis, header, bodyStart, latch)
		members.add(header)
		loopIndex := len(facts.loops)
		facts.loops = append(facts.loops, naturalLoop{header: header, latch: latch, members: members})
		facts.loopByHeader[header] = loopIndex
		for member := range graph.Nodes {
			if member != header && members.contains(member) {
				facts.bodyOwnerByNode[member] = loopIndex
			}
		}
	}
	return facts
}

func validateNaturalLoops(ctx ValidationContext) Report {
	var report Report
	graph := ctx.Graph
	analysis := buildControlFlowAnalysis(graph)
	if !analysis.forwardAcyclic {
		report.Add(PhaseNaturalLoops, "/edges", CodeCycleInvalid, "removing back edges must leave an acyclic graph")
		return report
	}

	loopIndexes := make([]int, 0)
	latchOwners := make(map[int][]int)
	for index, node := range graph.Nodes {
		if node.Type != NodeLoop {
			continue
		}
		loopIndexes = append(loopIndexes, index)
		config := node.Config.(LoopConfig)
		latchOwners[analysis.nodeIndex[config.LatchNodeID]] = append(latchOwners[analysis.nodeIndex[config.LatchNodeID]], index)
	}

	loops := make([]naturalLoop, 0, len(loopIndexes))
	for _, header := range loopIndexes {
		node := graph.Nodes[header]
		config := node.Config.(LoopConfig)
		nodePath := fmt.Sprintf("/nodes/%d", header)
		latch := analysis.nodeIndex[config.LatchNodeID]

		if !validLoopLatch(graph.Nodes[latch]) || len(latchOwners[latch]) != 1 {
			report.AddNode(
				PhaseNaturalLoops,
				joinPath(joinPath(nodePath, "config"), "latch_node_id"),
				node.ID,
				CodeLoopLatchInvalid,
				"loop latch must be an exclusive lead, consult worker, or transform",
			)
		}

		backEdges := make([]int, 0, 1)
		for _, edgeIndex := range analysis.outgoing[latch] {
			edge := graph.Edges[edgeIndex]
			if edge.Route == RouteBack && edge.ToNodeID == node.ID {
				backEdges = append(backEdges, edgeIndex)
			}
		}
		switch len(backEdges) {
		case 0:
			report.AddNode(
				PhaseNaturalLoops,
				joinPath(joinPath(nodePath, "config"), "latch_node_id"),
				node.ID,
				CodeLoopBackMissing,
				"loop latch must have one back edge to its header",
			)
			continue
		case 1:
		default:
			for _, edgeIndex := range backEdges {
				report.AddNode(
					PhaseNaturalLoops,
					fmt.Sprintf("/edges/%d", edgeIndex),
					node.ID,
					CodeLoopBackDuplicate,
					"loop latch has more than one back edge to its header",
				)
			}
			continue
		}

		if !analysis.dominates(header, latch) {
			report.AddNode(
				PhaseNaturalLoops,
				fmt.Sprintf("/edges/%d", backEdges[0]),
				node.ID,
				CodeLoopBackInvalid,
				"loop header must dominate its latch",
			)
		}

		bodyEdge := -1
		for _, edgeIndex := range analysis.outgoing[header] {
			if graph.Edges[edgeIndex].Route == RouteBody {
				bodyEdge = edgeIndex
				break
			}
		}
		if bodyEdge < 0 {
			// Phase 3 owns route cardinality, but keep the phase implementation
			// total for direct use in tests and future callers.
			report.AddNode(PhaseNaturalLoops, nodePath, node.ID, CodeLoopBodyInvalid, "loop body edge is missing")
			continue
		}
		bodyStart := analysis.nodeIndex[graph.Edges[bodyEdge].ToNodeID]
		members := loopMembers(graph, analysis, header, bodyStart, latch)
		if !members.contains(bodyStart) || !members.contains(latch) {
			report.AddNode(
				PhaseNaturalLoops,
				fmt.Sprintf("/edges/%d", bodyEdge),
				node.ID,
				CodeLoopBodyInvalid,
				"loop body must lead from the body edge to the latch",
			)
			continue
		}
		members.add(header)
		loops = append(loops, naturalLoop{header: header, latch: latch, members: members})

		for member := range graph.Nodes {
			if member == header || !members.contains(member) {
				continue
			}
			if !analysis.dominates(header, member) {
				report.AddNode(
					PhaseNaturalLoops,
					fmt.Sprintf("/nodes/%d", member),
					node.ID,
					CodeLoopBodyInvalid,
					"loop header must dominate every body node",
				)
			}
			for _, edgeIndex := range analysis.incoming[member] {
				edge := graph.Edges[edgeIndex]
				source := analysis.nodeIndex[edge.FromNodeID]
				allowedHeaderEntry := source == header && edgeIndex == bodyEdge && edge.Route == RouteBody
				if !members.contains(source) && !allowedHeaderEntry {
					report.AddNode(
						PhaseNaturalLoops,
						fmt.Sprintf("/edges/%d", edgeIndex),
						node.ID,
						CodeLoopSideEntry,
						"loop body may only be entered through the header body edge",
					)
				}
			}
			for _, edgeIndex := range analysis.outgoing[member] {
				edge := graph.Edges[edgeIndex]
				target := analysis.nodeIndex[edge.ToNodeID]
				exactBack := member == latch && edge.Route == RouteBack && target == header
				latchFailure := member == latch && edge.Route == RouteFailure
				if exactBack || latchFailure {
					continue
				}
				if target == header || !members.contains(target) {
					report.AddNode(
						PhaseNaturalLoops,
						fmt.Sprintf("/edges/%d", edgeIndex),
						node.ID,
						CodeLoopSideExit,
						"loop body may only leave through the header exit after its back edge",
					)
				}
			}
		}
	}

	indexLoopConflicts(&report, graph.Nodes, loops, nil)
	return report
}

type loopConflictStats struct {
	bitsetWordScans int
	memberVisits    int
	conflictChecks  int
}

type loopConflictPair struct {
	first  int
	second int
}

// indexLoopConflicts indexes the first loop owner for each member. A loop that
// intersects an already-owned member is already invalid; checking that stable
// owner is sufficient to classify and reject it without comparing every pair
// of loop-sized bitsets.
func indexLoopConflicts(report *Report, nodes []Node, loops []naturalLoop, stats *loopConflictStats) {
	memberOwner := make([]int, len(nodes))
	for index := range memberOwner {
		memberOwner[index] = -1
	}
	seenPairs := make(map[loopConflictPair]struct{})

	for loopIndex, loop := range loops {
		for wordIndex, packed := range loop.members {
			if stats != nil {
				stats.bitsetWordScans++
			}
			for packed != 0 {
				bit := bits.TrailingZeros64(packed)
				packed &= packed - 1
				member := wordIndex*64 + bit
				if member >= len(nodes) {
					continue
				}
				if stats != nil {
					stats.memberVisits++
				}
				owner := memberOwner[member]
				if owner < 0 {
					memberOwner[member] = loopIndex
					continue
				}
				if owner == loopIndex {
					continue
				}
				pair := loopConflictPair{first: owner, second: loopIndex}
				if _, exists := seenPairs[pair]; exists {
					continue
				}
				seenPairs[pair] = struct{}{}
				if stats != nil {
					stats.conflictChecks++
				}

				ownerLoop := loops[owner]
				nested := ownerLoop.members.contains(loop.header) || loop.members.contains(ownerLoop.header)
				code := CodeLoopOverlapping
				message := "loop bodies must not overlap"
				if nested {
					code = CodeLoopNested
					message = "nested loops are not supported in v1"
				}
				header := loop.header
				report.AddNode(PhaseNaturalLoops, fmt.Sprintf("/nodes/%d", header), nodes[header].ID, code, message)
			}
		}
	}
}

func validLoopLatch(node Node) bool {
	switch node.Type {
	case NodeLead, NodeTransform:
		return true
	case NodeWorker:
		config, ok := node.Config.(WorkerConfig)
		return ok && config.Kind == WorkerConsult
	default:
		return false
	}
}

func loopMembers(graph GraphDefinition, analysis controlFlowAnalysis, header, bodyStart, latch int) nodeBitSet {
	reachableFromBody := newNodeBitSet(len(graph.Nodes))
	stack := []int{bodyStart}
	for len(stack) != 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if current == header || reachableFromBody.contains(current) {
			continue
		}
		reachableFromBody.add(current)
		for _, edgeIndex := range analysis.forwardOutgoing[current] {
			stack = append(stack, analysis.nodeIndex[graph.Edges[edgeIndex].ToNodeID])
		}
	}

	canReachLatch := newNodeBitSet(len(graph.Nodes))
	stack = []int{latch}
	for len(stack) != 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if current == header || canReachLatch.contains(current) {
			continue
		}
		canReachLatch.add(current)
		for _, edgeIndex := range analysis.forwardIncoming[current] {
			stack = append(stack, analysis.nodeIndex[graph.Edges[edgeIndex].FromNodeID])
		}
	}
	reachableFromBody.intersect(canReachLatch)
	return reachableFromBody
}

func validateParallelJoin(ctx ValidationContext) Report {
	var report Report
	graph := ctx.Graph
	analysis := buildControlFlowAnalysis(graph)
	// Frozen may_yield capability is an explicit phase-9 proof owned by Task
	// 2D. Phase 6 must not infer it from node shape or treat a missing proof as
	// success.
	joinOwners := make(map[int][]int)
	branchOwnerCount := make([]int, len(graph.Nodes))
	for index, node := range graph.Nodes {
		if node.Type != NodeParallel {
			continue
		}
		config := node.Config.(ParallelConfig)
		joinOwners[analysis.nodeIndex[config.JoinNodeID]] = append(joinOwners[analysis.nodeIndex[config.JoinNodeID]], index)
		for _, edgeIndex := range analysis.outgoing[index] {
			edge := graph.Edges[edgeIndex]
			if edge.Route == RouteBranch {
				branchOwnerCount[analysis.nodeIndex[edge.ToNodeID]]++
			}
		}
	}

	for parallelIndex, node := range graph.Nodes {
		if node.Type != NodeParallel {
			continue
		}
		nodePath := fmt.Sprintf("/nodes/%d", parallelIndex)
		config := node.Config.(ParallelConfig)
		joinIndex := analysis.nodeIndex[config.JoinNodeID]
		branches := make([]int, 0)
		for _, edgeIndex := range analysis.outgoing[parallelIndex] {
			if graph.Edges[edgeIndex].Route == RouteBranch {
				branches = append(branches, edgeIndex)
			}
		}
		if len(branches) < 2 {
			report.AddNode(PhaseParallelJoin, nodePath, node.ID, CodeParallelBranchCountInvalid, "parallel requires at least two branches")
		}

		joinValid := graph.Nodes[joinIndex].Type == NodeJoin
		if !joinValid {
			report.AddNode(
				PhaseParallelJoin,
				joinPath(joinPath(nodePath, "config"), "join_node_id"),
				node.ID,
				CodeParallelJoinInvalid,
				"parallel join_node_id must reference a join node",
			)
		}
		if len(joinOwners[joinIndex]) != 1 {
			report.AddNode(
				PhaseParallelJoin,
				joinPath(joinPath(nodePath, "config"), "join_node_id"),
				node.ID,
				CodeParallelJoinShared,
				"a join node may be owned by only one parallel",
			)
		}

		legSet := newNodeBitSet(len(graph.Nodes))
		for _, edgeIndex := range branches {
			edge := graph.Edges[edgeIndex]
			leg := analysis.nodeIndex[edge.ToNodeID]
			legNode := graph.Nodes[leg]
			worker, isWorker := legNode.Config.(WorkerConfig)
			if legNode.Type == NodeParallel {
				report.AddNode(PhaseParallelJoin, fmt.Sprintf("/edges/%d/to_node_id", edgeIndex), node.ID, CodeParallelNested, "nested parallel is not supported in v1")
			}
			if !isWorker || legNode.Type != NodeWorker || worker.Kind != WorkerDispatch {
				report.AddNode(
					PhaseParallelJoin,
					fmt.Sprintf("/edges/%d/to_node_id", edgeIndex),
					node.ID,
					CodeParallelBranchNotDispatchWorker,
					"parallel branch target must be a dispatch worker",
				)
				continue
			}
			if legSet.contains(leg) || len(analysis.incoming[leg]) != 1 || analysis.incoming[leg][0] != edgeIndex {
				report.AddNode(
					PhaseParallelJoin,
					fmt.Sprintf("/edges/%d/to_node_id", edgeIndex),
					node.ID,
					CodeParallelBranchNotExclusive,
					"parallel branch worker must have exactly one branch input",
				)
			}
			legSet.add(leg)
			outgoing := analysis.outgoing[leg]
			if len(outgoing) != 1 {
				report.AddNode(
					PhaseParallelJoin,
					fmt.Sprintf("/nodes/%d", leg),
					node.ID,
					CodeParallelBranchNotExclusive,
					"parallel branch worker must have exactly one join output",
				)
				continue
			}
			output := graph.Edges[outgoing[0]]
			if output.Route != RouteJoin || analysis.nodeIndex[output.ToNodeID] != joinIndex {
				report.AddNode(
					PhaseParallelJoin,
					fmt.Sprintf("/edges/%d", outgoing[0]),
					node.ID,
					CodeParallelJoinInvalid,
					"parallel branch worker must join its owner's join node",
				)
			}
		}

		if joinValid && !joinInputsMatch(graph, analysis, joinIndex, legSet, len(branches)) {
			report.AddNode(
				PhaseParallelJoin,
				joinPath(joinPath(nodePath, "config"), "join_node_id"),
				node.ID,
				CodeJoinInputsMismatch,
				"join inputs must exactly match the parallel branch workers",
			)
		}
		if joinValid {
			validateJoinPolicy(&report, graph.Nodes[joinIndex], joinIndex, len(branches))
		}
	}

	for nodeIndex, node := range graph.Nodes {
		switch node.Type {
		case NodeWorker:
			config := node.Config.(WorkerConfig)
			if config.Kind == WorkerDispatch && branchOwnerCount[nodeIndex] != 1 {
				report.AddNode(
					PhaseParallelJoin,
					fmt.Sprintf("/nodes/%d", nodeIndex),
					node.ID,
					CodeParallelBranchNotExclusive,
					"dispatch worker must be owned by exactly one parallel branch",
				)
			}
		case NodeJoin:
			if len(joinOwners[nodeIndex]) != 1 {
				report.AddNode(
					PhaseParallelJoin,
					fmt.Sprintf("/nodes/%d", nodeIndex),
					node.ID,
					CodeParallelJoinInvalid,
					"join node must be owned by exactly one parallel",
				)
			}
		}
	}
	return report
}

func joinInputsMatch(graph GraphDefinition, analysis controlFlowAnalysis, joinIndex int, legs nodeBitSet, legCount int) bool {
	incoming := analysis.incoming[joinIndex]
	if len(incoming) != legCount {
		return false
	}
	seen := newNodeBitSet(len(graph.Nodes))
	for _, edgeIndex := range incoming {
		edge := graph.Edges[edgeIndex]
		source := analysis.nodeIndex[edge.FromNodeID]
		if edge.Route != RouteJoin || !legs.contains(source) || seen.contains(source) {
			return false
		}
		seen.add(source)
	}
	return true
}

func validateJoinPolicy(report *Report, node Node, nodeIndex, legCount int) {
	config := node.Config.(JoinConfig)
	configPath := fmt.Sprintf("/nodes/%d/config", nodeIndex)
	switch config.Policy {
	case JoinAllSuccess, JoinFailFast:
	case JoinQuorum:
		if config.SuccessCount == nil || *config.SuccessCount <= 0 || *config.SuccessCount > int64(legCount) {
			report.AddNode(PhaseParallelJoin, joinPath(configPath, "success_count"), node.ID, CodeJoinQuorumInvalid, "quorum success_count must be within the branch count")
		}
	case JoinDeadline:
		if config.DeadlineSeconds == nil || *config.DeadlineSeconds <= 0 {
			report.AddNode(PhaseParallelJoin, joinPath(configPath, "deadline_seconds"), node.ID, CodeJoinDeadlineInvalid, "deadline_seconds must be positive")
		}
	default:
		report.AddNode(PhaseParallelJoin, joinPath(configPath, "policy"), node.ID, CodeJoinPolicyInvalid, "join policy is invalid")
	}
}

func validateConditions(ctx ValidationContext) Report {
	var report Report
	graph := ctx.Graph
	analysis := buildControlFlowAnalysis(graph)
	for nodeIndex, node := range graph.Nodes {
		switch node.Type {
		case NodeCondition:
			cases := make([]int, 0)
			defaults := make([]int, 0)
			for _, edgeIndex := range analysis.outgoing[nodeIndex] {
				switch graph.Edges[edgeIndex].Route {
				case RouteCase:
					cases = append(cases, edgeIndex)
				case RouteDefault:
					defaults = append(defaults, edgeIndex)
				}
			}
			if len(cases) == 0 {
				report.AddNode(PhaseConditions, fmt.Sprintf("/nodes/%d", nodeIndex), node.ID, CodeConditionCaseMissing, "condition requires at least one case edge")
			}
			if len(defaults) == 0 {
				report.AddNode(PhaseConditions, fmt.Sprintf("/nodes/%d", nodeIndex), node.ID, CodeConditionDefaultMissing, "condition requires one default edge")
			}
			if len(defaults) > 1 {
				for _, edgeIndex := range defaults {
					report.AddNode(PhaseConditions, fmt.Sprintf("/edges/%d", edgeIndex), node.ID, CodeConditionDefaultDuplicate, "condition must have exactly one default edge")
				}
			}
			priorities := make(map[int64][]int)
			for _, edgeIndex := range cases {
				edge := graph.Edges[edgeIndex]
				if edge.Priority == nil {
					report.AddNode(PhaseConditions, fmt.Sprintf("/edges/%d/priority", edgeIndex), node.ID, CodeConditionPriorityRequired, "condition case priority is required")
				} else {
					priorities[*edge.Priority] = append(priorities[*edge.Priority], edgeIndex)
				}
				if edge.Predicate == nil {
					report.AddNode(PhaseConditions, fmt.Sprintf("/edges/%d/predicate", edgeIndex), node.ID, CodePredicateRequired, "condition case predicate is required")
				} else {
					validatePredicateStructure(&report, node.ID, fmt.Sprintf("/edges/%d/predicate", edgeIndex), *edge.Predicate)
				}
			}
			for _, edgeIndexes := range priorities {
				if len(edgeIndexes) < 2 {
					continue
				}
				for _, edgeIndex := range edgeIndexes {
					report.AddNode(PhaseConditions, fmt.Sprintf("/edges/%d/priority", edgeIndex), node.ID, CodeConditionPriorityDuplicate, "condition case priority must be unique")
				}
			}
		case NodeLoop:
			config := node.Config.(LoopConfig)
			validatePredicateStructure(
				&report,
				node.ID,
				joinPath(joinPath(fmt.Sprintf("/nodes/%d", nodeIndex), "config"), "continue_predicate"),
				config.ContinuePredicate,
			)
		}
	}
	return report
}

func validatePredicateStructure(report *Report, nodeID, path string, predicate Predicate) {
	if !validPredicateOperator(predicate.Operator) {
		report.AddNode(PhaseConditions, joinPath(path, "operator"), nodeID, CodePredicateOperatorInvalid, "predicate operator is invalid")
		return
	}
	if predicate.Operator == OperatorExists {
		if predicate.Right != nil {
			report.AddNode(PhaseConditions, joinPath(path, "right"), nodeID, CodePredicateRightForbidden, "exists predicate forbids right")
		}
		return
	}
	if predicate.Right == nil {
		report.AddNode(PhaseConditions, joinPath(path, "right"), nodeID, CodePredicateRightRequired, "predicate operator requires right")
	}
}
