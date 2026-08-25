package machine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

const (
	maxSafeMachineInteger        int64 = 9007199254740991
	maxGraphNodesV1                    = 128
	maxGraphEdgesV1                    = 256
	maxParallelBranchesV1              = 16
	maxLoopIterationsV1          int64 = 5
	maxEstimatedExecutionStepsV1 int64 = 2000
)

// ValidationContext contains the pure machine inputs and caller-loaded proof
// snapshots. Validate never resolves latest data or performs I/O.
type ValidationContext struct {
	WorkspaceID string
	TeamID      string
	Lead        AgentVersionKey

	Trigger TriggerConfig
	Graph   GraphDefinition

	Authorization AuthorizationSnapshot
	Agents        map[AgentVersionKey]AgentVersionProof
	Capabilities  map[AgentVersionKey]CapabilityProof
	Dependencies  DependencySnapshot
	Factories     FactorySnapshot
}

// Validate runs the ordered workflow validator and returns every issue from the
// earliest failing phase.
func Validate(ctx ValidationContext) Report {
	phases := []func(ValidationContext) Report{
		validateTypedBasics,
		validateIdentityReferences,
		validateTopology,
		validateRoutes,
		validateNaturalLoops,
		validateParallelJoin,
		validateConditions,
		func(ctx ValidationContext) Report { return validateValuesAndSchemas(ctx, nil) },
		validateTriggerCapabilities,
		validateAuthorization,
		validateAgentVersions,
		validateDependencies,
		validateFactories,
	}
	for _, phase := range phases {
		report := phase(ctx)
		if len(report.Issues) != 0 {
			report.Sort()
			return report
		}
	}
	return Report{}
}

func validateDeclaredSchemas(ctx ValidationContext) Report {
	var report Report
	graph := ctx.Graph

	parseContractSchema(&report, "", "/input_contract", graph.InputContract)
	parseContractSchema(&report, "", "/output_contract", graph.OutputContract)

	for index, node := range graph.Nodes {
		nodePath := fmt.Sprintf("/nodes/%d", index)
		if node.Output != nil {
			parseContractSchema(&report, node.ID, joinPath(nodePath, "output"), *node.Output)
		}

		configPath := joinPath(nodePath, "config")
		switch config := node.Config.(type) {
		case WaitConfig:
			parseSchemaIntoReport(&report, config.ResumeSchema, joinPath(configPath, "resume_schema"), node.ID)
		}
	}

	// Handoff output compatibility is intentionally not guessed here. Phase 11
	// derives it from the exact AgentVersion proof and Phase 8 then reuses the
	// contract algebra in schema.go.
	return report
}

func parseContractSchema(report *Report, nodeID, path string, contract OutputContract) *FixedJSONSchema {
	if len(contract.Schema) == 0 {
		return nil
	}
	return parseSchemaIntoReport(report, contract.Schema, joinPath(path, "schema"), nodeID)
}

func parseSchemaIntoReport(report *Report, raw json.RawMessage, path, nodeID string) *FixedJSONSchema {
	schema, parsed := parseFixedJSONSchema(raw, path, nodeID)
	for _, issue := range parsed.Issues {
		report.AddNode(issue.Phase, issue.Path, issue.NodeID, issue.Code, issue.Message)
	}
	if len(parsed.Issues) != 0 {
		return nil
	}
	return schema
}

func validateTypedBasics(ctx ValidationContext) Report {
	var report Report
	graph := ctx.Graph

	validateTriggerBasic(&report, ctx.Trigger)
	if graph.SchemaVersion != SchemaVersionV1 {
		report.Add(PhaseDTO, "/schema_version", CodeSchemaVersionUnsupported, "graph schema_version must be 1")
	}
	validateSafeInteger(&report, "", "/schema_version", int64(graph.SchemaVersion))
	if graph.EntryNodeID == "" {
		report.Add(PhaseDTO, "/entry_node_id", CodeFieldRequired, "entry_node_id must not be empty")
	}
	if len(graph.Nodes) == 0 {
		report.Add(PhaseDTO, "/nodes", CodeGraphEmpty, "graph must contain at least one node")
	}
	validateGraphComplexity(&report, graph)
	validateContractBasic(&report, "/input_contract", graph.InputContract)
	validateContractBasic(&report, "/output_contract", graph.OutputContract)

	for index := range graph.Nodes {
		node := graph.Nodes[index]
		path := fmt.Sprintf("/nodes/%d", index)
		if node.ID == "" {
			report.AddNode(PhaseDTO, joinPath(path, "id"), node.ID, CodeFieldRequired, "node id must not be empty")
		}
		if !validNodeType(node.Type) {
			report.AddNode(PhaseDTO, joinPath(path, "type"), node.ID, CodeEnumInvalid, "invalid node type")
		}
		for _, name := range sortedInputNames(node.Inputs) {
			binding := node.Inputs[name]
			bindingPath := joinPath(joinPath(path, "inputs"), name)
			if !validValueType(binding.ExpectedType) {
				report.AddNode(PhaseDTO, joinPath(bindingPath, "expected_type"), node.ID, CodeEnumInvalid, "invalid input value type")
			}
			validateValueRefBasic(&report, node.ID, joinPath(bindingPath, "value"), binding.Value)
		}

		if nodeOutputRequired(node.Type) && node.Output == nil {
			report.AddNode(PhaseDTO, joinPath(path, "output"), node.ID, CodeOutputRequired, "node must declare output")
		}
		if nodeOutputForbidden(node.Type) && node.Output != nil {
			report.AddNode(PhaseDTO, joinPath(path, "output"), node.ID, CodeOutputForbidden, "node output is fixed or derived")
		}
		if node.Output != nil {
			validateContractBasicNode(&report, node.ID, joinPath(path, "output"), *node.Output)
		}
		validateNodeConfigBasic(&report, path, node)
	}

	for index := range graph.Edges {
		edge := graph.Edges[index]
		path := fmt.Sprintf("/edges/%d", index)
		if edge.ID == "" {
			report.Add(PhaseDTO, joinPath(path, "id"), CodeFieldRequired, "edge id must not be empty")
		}
		if edge.FromNodeID == "" {
			report.Add(PhaseDTO, joinPath(path, "from_node_id"), CodeFieldRequired, "edge source must not be empty")
		}
		if edge.ToNodeID == "" {
			report.Add(PhaseDTO, joinPath(path, "to_node_id"), CodeFieldRequired, "edge target must not be empty")
		}
		if !validEdgeRoute(edge.Route) {
			report.Add(PhaseDTO, joinPath(path, "route"), CodeEnumInvalid, "invalid edge route")
		}
		if edge.Priority != nil && *edge.Priority < 0 {
			report.Add(PhaseDTO, joinPath(path, "priority"), CodeIntegerInvalid, "edge priority must be non-negative")
		}
		if edge.Priority != nil {
			validateSafeInteger(&report, "", joinPath(path, "priority"), *edge.Priority)
		}
		if edge.Predicate != nil {
			validatePredicateBasic(&report, "", joinPath(path, "predicate"), *edge.Predicate)
		}
	}

	return report
}

// validateGraphComplexity enforces the intrinsic machine-v1 bounds before
// graph analysis allocates indexes proportional to untrusted graph size. The
// execution estimate intentionally matches serialMachineStepBudget in
// internal/teamrun/executor.go.
func validateGraphComplexity(report *Report, graph GraphDefinition) {
	nodeCount := len(graph.Nodes)
	if nodeCount > maxGraphNodesV1 {
		report.Add(
			PhaseDTO,
			"/nodes",
			CodeGraphTooLarge,
			fmt.Sprintf("graph node count %d exceeds machine v1 limit %d", nodeCount, maxGraphNodesV1),
		)
	}
	if len(graph.Edges) > maxGraphEdgesV1 {
		report.Add(
			PhaseDTO,
			"/edges",
			CodeGraphTooLarge,
			fmt.Sprintf("graph edge count %d exceeds machine v1 limit %d", len(graph.Edges), maxGraphEdgesV1),
		)
	}

	branchCounts := make(map[string]int)
	for _, edge := range graph.Edges {
		if edge.Route == RouteBranch {
			branchCounts[edge.FromNodeID]++
		}
	}

	loopIterations := int64(0)
	loopIterationsExceededEstimateRange := false
	for index, node := range graph.Nodes {
		switch node.Type {
		case NodeParallel:
			branchCount := branchCounts[node.ID]
			if branchCount > maxParallelBranchesV1 {
				report.AddNode(
					PhaseDTO,
					fmt.Sprintf("/nodes/%d", index),
					node.ID,
					CodeGraphTooLarge,
					fmt.Sprintf("parallel branch count %d exceeds machine v1 limit %d", branchCount, maxParallelBranchesV1),
				)
			}
		case NodeLoop:
			iterations := int64(1)
			if config, ok := node.Config.(LoopConfig); ok && config.MaxIterations >= 1 {
				iterations = config.MaxIterations
				if config.MaxIterations > maxLoopIterationsV1 {
					report.AddNode(
						PhaseDTO,
						fmt.Sprintf("/nodes/%d/config/max_iterations", index),
						node.ID,
						CodeComplexityBudgetExceeded,
						fmt.Sprintf("loop max_iterations %d exceeds machine v1 limit %d", config.MaxIterations, maxLoopIterationsV1),
					)
				}
			}
			if iterations > maxEstimatedExecutionStepsV1-loopIterations {
				loopIterationsExceededEstimateRange = true
				loopIterations = maxEstimatedExecutionStepsV1
			} else {
				loopIterations += iterations
			}
		}
	}

	estimatedSteps := int64(nodeCount)
	estimateExceeded := loopIterationsExceededEstimateRange
	if nodeCount > 0 && !estimateExceeded {
		multiplier := 1 + loopIterations
		if int64(nodeCount) > maxEstimatedExecutionStepsV1/multiplier {
			estimateExceeded = true
		} else {
			estimatedSteps = int64(nodeCount) * multiplier
			estimateExceeded = estimatedSteps > maxEstimatedExecutionStepsV1
		}
	}
	if estimateExceeded {
		message := fmt.Sprintf(
			"estimated execution steps exceed machine v1 limit %d (node_count=%d, summed_loop_iterations=%d)",
			maxEstimatedExecutionStepsV1,
			nodeCount,
			loopIterations,
		)
		if loopIterationsExceededEstimateRange {
			message = fmt.Sprintf(
				"estimated execution steps exceed machine v1 limit %d (node_count=%d, summed_loop_iterations>=%d)",
				maxEstimatedExecutionStepsV1,
				nodeCount,
				loopIterations,
			)
		}
		report.Add(PhaseDTO, "/nodes", CodeComplexityBudgetExceeded, message)
	}
}

func validateTriggerBasic(report *Report, trigger TriggerConfig) {
	const root = "/trigger_config"
	if trigger.SchemaVersion != SchemaVersionV1 {
		report.Add(PhaseDTO, joinPath(root, "schema_version"), CodeSchemaVersionUnsupported, "trigger schema_version must be 1")
	}
	validateSafeInteger(report, "", joinPath(root, "schema_version"), int64(trigger.SchemaVersion))
	if !validTriggerType(trigger.Type) {
		report.Add(PhaseDTO, joinPath(root, "type"), CodeEnumInvalid, "invalid trigger type")
	} else {
		validateTriggerConfigBasic(report, trigger, root)
	}

	sessionTrigger := trigger.Type == TriggerConversationExplicit || trigger.Type == TriggerConversationAuto
	if sessionTrigger && trigger.Delivery != nil {
		report.Add(PhaseDTO, joinPath(root, "delivery"), CodeDeliveryForbidden, "session trigger must not declare delivery")
	}
	if !sessionTrigger && validTriggerType(trigger.Type) && trigger.Delivery == nil {
		report.Add(PhaseDTO, joinPath(root, "delivery"), CodeDeliveryRequired, "non-session trigger requires delivery")
	}
	if trigger.Delivery != nil {
		validateDeliveryBasic(report, root, *trigger.Delivery)
	}
}

func validateTriggerConfigBasic(report *Report, trigger TriggerConfig, root string) {
	configPath := joinPath(root, "config")
	missingOrMismatch := func(matches bool) bool {
		if matches {
			return false
		}
		if trigger.Config == nil {
			report.Add(PhaseDTO, configPath, CodeFieldRequired, "trigger config is required")
		} else {
			report.Add(PhaseDTO, configPath, CodeTypeInvalid, "trigger config does not match trigger type")
		}
		return true
	}

	switch trigger.Type {
	case TriggerConversationExplicit:
		_, ok := trigger.Config.(ConversationExplicitConfig)
		missingOrMismatch(ok)
	case TriggerConversationAuto:
		config, ok := trigger.Config.(ConversationAutoConfig)
		if missingOrMismatch(ok) {
			return
		}
		if config.CatalogKey == "" {
			report.Add(PhaseDTO, joinPath(configPath, "catalog_key"), CodeFieldRequired, "catalog_key must not be empty")
		}
	case TriggerSchedule:
		config, ok := trigger.Config.(ScheduleConfig)
		if missingOrMismatch(ok) {
			return
		}
		if config.ScheduleID == "" {
			report.Add(PhaseDTO, joinPath(configPath, "schedule_id"), CodeFieldRequired, "schedule_id must not be empty")
		}
	case TriggerAPI:
		config, ok := trigger.Config.(APIConfig)
		if missingOrMismatch(ok) {
			return
		}
		if config.EndpointKey == "" {
			report.Add(PhaseDTO, joinPath(configPath, "endpoint_key"), CodeFieldRequired, "endpoint_key must not be empty")
		}
	case TriggerEvent:
		config, ok := trigger.Config.(EventConfig)
		if missingOrMismatch(ok) {
			return
		}
		if config.EventType == "" {
			report.Add(PhaseDTO, joinPath(configPath, "event_type"), CodeFieldRequired, "event_type must not be empty")
		}
	}
}

func validateDeliveryBasic(report *Report, root string, delivery Delivery) {
	path := joinPath(root, "delivery")
	switch delivery.Kind {
	case DeliveryJobRecord:
		if delivery.Ref != "" {
			report.Add(PhaseDTO, joinPath(path, "ref"), CodeDeliveryForbidden, "job_record delivery must not declare ref")
		}
	case DeliveryCallbackRef, DeliveryTargetRef:
		if delivery.Ref == "" {
			report.Add(PhaseDTO, joinPath(path, "ref"), CodeFieldRequired, "delivery ref must not be empty")
		}
	default:
		report.Add(PhaseDTO, joinPath(path, "kind"), CodeEnumInvalid, "invalid delivery kind")
	}
}

func validateContractBasic(report *Report, path string, contract OutputContract) {
	validateContractBasicNode(report, "", path, contract)
}

func validateContractBasicNode(report *Report, nodeID, path string, contract OutputContract) {
	add := func(issuePath, code, message string) {
		if nodeID == "" {
			report.Add(PhaseDTO, issuePath, code, message)
		} else {
			report.AddNode(PhaseDTO, issuePath, nodeID, code, message)
		}
	}
	if !validValueType(contract.Type) {
		add(joinPath(path, "type"), CodeEnumInvalid, "invalid contract value type")
	}
	if len(contract.Schema) == 0 {
		return
	}
	if !validateRawJSONObject(report, nodeID, joinPath(path, "schema"), contract.Schema) {
		return
	}
	if contract.Type != ValueJSON {
		add(joinPath(path, "schema"), CodeContractInvalid, "only json contracts may declare schema")
	}
}

func rawStartsJSONObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) >= 2 && trimmed[0] == '{'
}

func validateRawJSONObject(report *Report, nodeID, path string, raw json.RawMessage) bool {
	if issue := inspectJSON(raw); issue != nil {
		addPrefixedDTOIssue(report, nodeID, path, issue)
		return false
	}
	if !rawStartsJSONObject(raw) {
		if nodeID == "" {
			report.Add(PhaseDTO, path, CodeContractInvalid, "schema must be an object")
		} else {
			report.AddNode(PhaseDTO, path, nodeID, CodeContractInvalid, "schema must be an object")
		}
		return false
	}
	return true
}

func validateRawJSONValue(report *Report, nodeID, path string, raw json.RawMessage) {
	if issue := inspectJSON(raw); issue != nil {
		addPrefixedDTOIssue(report, nodeID, path, issue)
	}
}

func addPrefixedDTOIssue(report *Report, nodeID, basePath string, issue *dtoError) {
	path := basePath
	if issue.path != "" {
		path += issue.path
	}
	message := issue.code
	if issue.err != nil {
		message = issue.err.Error()
	}
	if nodeID == "" {
		report.Add(PhaseDTO, path, issue.code, message)
	} else {
		report.AddNode(PhaseDTO, path, nodeID, issue.code, message)
	}
}

func validateSafeInteger(report *Report, nodeID, path string, value int64) {
	if value >= -maxSafeMachineInteger && value <= maxSafeMachineInteger {
		return
	}
	if nodeID == "" {
		report.Add(PhaseDTO, path, CodeUnsafeInteger, "integer exceeds the safe machine range")
	} else {
		report.AddNode(PhaseDTO, path, nodeID, CodeUnsafeInteger, "integer exceeds the safe machine range")
	}
}

func validValueType(value ValueType) bool {
	switch value {
	case ValueText, ValueJSON, ValueBoolean, ValueNumber:
		return true
	default:
		return false
	}
}

func nodeOutputRequired(nodeType NodeType) bool {
	switch nodeType {
	case NodeLead, NodeWorker, NodeTransform:
		return true
	default:
		return false
	}
}

func nodeOutputForbidden(nodeType NodeType) bool {
	switch nodeType {
	case NodeCondition, NodeParallel, NodeJoin, NodeWait, NodeLoop, NodeDeliver, NodeHandoff:
		return true
	default:
		return false
	}
}

func validateNodeConfigBasic(report *Report, path string, node Node) {
	configPath := joinPath(path, "config")
	typeMismatch := func() {
		report.AddNode(PhaseDTO, configPath, node.ID, CodeTypeInvalid, "node config does not match node type")
	}
	positive := func(value *int64, field string) {
		if value == nil {
			return
		}
		validateSafeInteger(report, node.ID, joinPath(configPath, field), *value)
		if *value <= 0 {
			report.AddNode(PhaseDTO, joinPath(configPath, field), node.ID, CodeIntegerInvalid, "integer must be positive")
		}
	}

	switch node.Type {
	case NodeLead:
		if _, ok := node.Config.(LeadConfig); !ok {
			typeMismatch()
		}
	case NodeWorker:
		config, ok := node.Config.(WorkerConfig)
		if !ok {
			typeMismatch()
			return
		}
		if config.AgentID == "" {
			report.AddNode(PhaseDTO, joinPath(configPath, "agent_id"), node.ID, CodeFieldRequired, "agent_id must not be empty")
		}
		if config.AgentVersion <= 0 {
			report.AddNode(PhaseDTO, joinPath(configPath, "agent_version"), node.ID, CodeIntegerInvalid, "agent_version must be positive")
		}
		validateSafeInteger(report, node.ID, joinPath(configPath, "agent_version"), config.AgentVersion)
		if config.Kind != WorkerConsult && config.Kind != WorkerDispatch {
			report.AddNode(PhaseDTO, joinPath(configPath, "kind"), node.ID, CodeEnumInvalid, "invalid worker kind")
		}
	case NodeTransform:
		config, ok := node.Config.(TransformConfig)
		if !ok {
			typeMismatch()
			return
		}
		switch config.Operation {
		case TransformIdentity:
			if config.Value == nil {
				report.AddNode(PhaseDTO, joinPath(configPath, "value"), node.ID, CodeFieldRequired, "identity value is required")
			} else {
				validateValueRefBasic(report, node.ID, joinPath(configPath, "value"), *config.Value)
			}
			rejectForbiddenValueRefMap(report, node.ID, configPath, "fields", config.Fields)
			rejectForbiddenValueRefSlice(report, node.ID, configPath, "items", config.Items)
		case TransformObject:
			if len(config.Fields) == 0 {
				report.AddNode(PhaseDTO, joinPath(configPath, "fields"), node.ID, CodeFieldRequired, "object fields must not be empty")
			}
			for _, name := range sortedValueRefNames(config.Fields) {
				validateValueRefBasic(report, node.ID, joinPath(joinPath(configPath, "fields"), name), config.Fields[name])
			}
			rejectForbiddenValueRefPointer(report, node.ID, configPath, "value", config.Value)
			rejectForbiddenValueRefSlice(report, node.ID, configPath, "items", config.Items)
		case TransformArray:
			if config.Items == nil {
				report.AddNode(PhaseDTO, joinPath(configPath, "items"), node.ID, CodeFieldRequired, "array items are required")
			} else {
				for index, ref := range config.Items {
					validateValueRefBasic(report, node.ID, fmt.Sprintf("%s/%d", joinPath(configPath, "items"), index), ref)
				}
			}
			rejectForbiddenValueRefPointer(report, node.ID, configPath, "value", config.Value)
			rejectForbiddenValueRefMap(report, node.ID, configPath, "fields", config.Fields)
		default:
			scanValueRefPointerRaw(report, node.ID, joinPath(configPath, "value"), config.Value)
			scanValueRefMapRaw(report, node.ID, joinPath(configPath, "fields"), config.Fields)
			scanValueRefSliceRaw(report, node.ID, joinPath(configPath, "items"), config.Items)
			report.AddNode(PhaseDTO, joinPath(configPath, "operation"), node.ID, CodeEnumInvalid, "invalid transform operation")
		}
	case NodeCondition:
		if _, ok := node.Config.(ConditionConfig); !ok {
			typeMismatch()
		}
	case NodeParallel:
		config, ok := node.Config.(ParallelConfig)
		if !ok {
			typeMismatch()
			return
		}
		if config.JoinNodeID == "" {
			report.AddNode(PhaseDTO, joinPath(configPath, "join_node_id"), node.ID, CodeFieldRequired, "join_node_id must not be empty")
		}
	case NodeJoin:
		config, ok := node.Config.(JoinConfig)
		if !ok {
			typeMismatch()
			return
		}
		positive(config.SuccessCount, "success_count")
		positive(config.DeadlineSeconds, "deadline_seconds")
		switch config.Policy {
		case JoinAllSuccess, JoinFailFast:
			rejectForbiddenInteger(report, node.ID, configPath, "success_count", config.SuccessCount)
			rejectForbiddenInteger(report, node.ID, configPath, "deadline_seconds", config.DeadlineSeconds)
		case JoinQuorum:
			if config.SuccessCount == nil {
				report.AddNode(PhaseDTO, joinPath(configPath, "success_count"), node.ID, CodeFieldRequired, "success_count is required")
			}
			rejectForbiddenInteger(report, node.ID, configPath, "deadline_seconds", config.DeadlineSeconds)
		case JoinDeadline:
			if config.DeadlineSeconds == nil {
				report.AddNode(PhaseDTO, joinPath(configPath, "deadline_seconds"), node.ID, CodeFieldRequired, "deadline_seconds is required")
			}
			rejectForbiddenInteger(report, node.ID, configPath, "success_count", config.SuccessCount)
		default:
			report.AddNode(PhaseDTO, joinPath(configPath, "policy"), node.ID, CodeEnumInvalid, "invalid join policy")
		}
	case NodeWait:
		config, ok := node.Config.(WaitConfig)
		if !ok {
			typeMismatch()
			return
		}
		if len(config.ResumeSchema) == 0 {
			report.AddNode(PhaseDTO, joinPath(configPath, "resume_schema"), node.ID, CodeFieldRequired, "resume_schema is required")
		} else {
			validateRawJSONObject(report, node.ID, joinPath(configPath, "resume_schema"), config.ResumeSchema)
		}
		positive(config.TimeoutSeconds, "timeout_seconds")
	case NodeLoop:
		config, ok := node.Config.(LoopConfig)
		if !ok {
			typeMismatch()
			return
		}
		if config.MaxIterations <= 0 {
			report.AddNode(PhaseDTO, joinPath(configPath, "max_iterations"), node.ID, CodeIntegerInvalid, "max_iterations must be positive")
		}
		validateSafeInteger(report, node.ID, joinPath(configPath, "max_iterations"), config.MaxIterations)
		if config.LatchNodeID == "" {
			report.AddNode(PhaseDTO, joinPath(configPath, "latch_node_id"), node.ID, CodeFieldRequired, "latch_node_id must not be empty")
		}
		validatePredicateBasic(report, node.ID, joinPath(configPath, "continue_predicate"), config.ContinuePredicate)
	case NodeDeliver:
		config, ok := node.Config.(DeliverConfig)
		if !ok {
			typeMismatch()
			return
		}
		validateValueRefBasic(report, node.ID, joinPath(configPath, "result"), config.Result)
	case NodeHandoff:
		config, ok := node.Config.(HandoffConfig)
		if !ok {
			typeMismatch()
			return
		}
		if config.AgentID == "" {
			report.AddNode(PhaseDTO, joinPath(configPath, "agent_id"), node.ID, CodeFieldRequired, "agent_id must not be empty")
		}
		if config.AgentVersion <= 0 {
			report.AddNode(PhaseDTO, joinPath(configPath, "agent_version"), node.ID, CodeIntegerInvalid, "agent_version must be positive")
		}
		validateSafeInteger(report, node.ID, joinPath(configPath, "agent_version"), config.AgentVersion)
		positive(config.TimeoutSeconds, "timeout_seconds")
	}
}

func validatePredicateBasic(report *Report, nodeID, path string, predicate Predicate) {
	validateValueRefBasic(report, nodeID, joinPath(path, "left"), predicate.Left)
	if !validPredicateOperator(predicate.Operator) {
		if nodeID == "" {
			report.Add(PhaseDTO, joinPath(path, "operator"), CodeEnumInvalid, "invalid predicate operator")
		} else {
			report.AddNode(PhaseDTO, joinPath(path, "operator"), nodeID, CodeEnumInvalid, "invalid predicate operator")
		}
	}
	if predicate.Right != nil {
		validateValueRefBasic(report, nodeID, joinPath(path, "right"), *predicate.Right)
	}
}

func rejectForbiddenInteger(report *Report, nodeID, configPath, field string, value *int64) {
	if value != nil {
		report.AddNode(PhaseDTO, joinPath(configPath, field), nodeID, CodeUnknownField, "field is forbidden for this tagged-union variant")
	}
}

func rejectForbiddenValueRefPointer(report *Report, nodeID, configPath, field string, value *ValueRef) {
	if value == nil {
		return
	}
	path := joinPath(configPath, field)
	scanValueRefRawShallow(report, nodeID, path, *value)
	report.AddNode(PhaseDTO, path, nodeID, CodeUnknownField, "field is forbidden for this tagged-union variant")
}

func rejectForbiddenValueRefMap(report *Report, nodeID, configPath, field string, values map[string]ValueRef) {
	if values == nil {
		return
	}
	path := joinPath(configPath, field)
	scanValueRefMapRaw(report, nodeID, path, values)
	report.AddNode(PhaseDTO, path, nodeID, CodeUnknownField, "field is forbidden for this tagged-union variant")
}

func rejectForbiddenValueRefSlice(report *Report, nodeID, configPath, field string, values []ValueRef) {
	if values == nil {
		return
	}
	path := joinPath(configPath, field)
	scanValueRefSliceRaw(report, nodeID, path, values)
	report.AddNode(PhaseDTO, path, nodeID, CodeUnknownField, "field is forbidden for this tagged-union variant")
}

func scanValueRefPointerRaw(report *Report, nodeID, path string, value *ValueRef) {
	if value != nil {
		scanValueRefRawShallow(report, nodeID, path, *value)
	}
}

func scanValueRefMapRaw(report *Report, nodeID, path string, values map[string]ValueRef) {
	for _, name := range sortedValueRefNames(values) {
		scanValueRefRawShallow(report, nodeID, joinPath(path, name), values[name])
	}
}

func scanValueRefSliceRaw(report *Report, nodeID, path string, values []ValueRef) {
	for index, value := range values {
		scanValueRefRawShallow(report, nodeID, fmt.Sprintf("%s/%d", path, index), value)
	}
}

func scanValueRefRawShallow(report *Report, nodeID, path string, ref ValueRef) {
	if len(ref.Value) != 0 {
		validateRawJSONValue(report, nodeID, joinPath(path, "value"), ref.Value)
	}
	if ref.Default != nil && len(ref.Default.Value) != 0 {
		validateRawJSONValue(report, nodeID, joinPath(joinPath(path, "default"), "value"), ref.Default.Value)
	}
}

func validateValueRefBasic(report *Report, nodeID, path string, ref ValueRef) {
	add := func(issuePath, code, message string) {
		if nodeID == "" {
			report.Add(PhaseDTO, issuePath, code, message)
		} else {
			report.AddNode(PhaseDTO, issuePath, nodeID, code, message)
		}
	}
	if len(ref.Value) != 0 {
		validateRawJSONValue(report, nodeID, joinPath(path, "value"), ref.Value)
	}
	switch ref.Source {
	case ValueRunInput:
		rejectValueRefField(add, path, "node_id", ref.NodeID != "")
		rejectValueRefField(add, path, "value", len(ref.Value) != 0)
		rejectValueRefField(add, path, "iteration", ref.Iteration != "")
		if ref.Default != nil {
			scanValueRefRawShallow(report, nodeID, joinPath(path, "default"), *ref.Default)
			rejectValueRefField(add, path, "default", true)
		}
	case ValueNodeOutput:
		if ref.NodeID == "" {
			add(joinPath(path, "node_id"), CodeFieldRequired, "node_id must not be empty")
		}
		rejectValueRefField(add, path, "value", len(ref.Value) != 0)
		if ref.Iteration != "" && ref.Iteration != IterationCurrent && ref.Iteration != IterationPrevious {
			add(joinPath(path, "iteration"), CodeEnumInvalid, "invalid node_output iteration")
		}
		if ref.Default != nil {
			validateLiteralDefaultBasic(report, nodeID, joinPath(path, "default"), ref.Default)
		}
	case ValueLiteral:
		if len(ref.Value) == 0 {
			add(joinPath(path, "value"), CodeFieldRequired, "literal value is required")
		}
		rejectValueRefField(add, path, "path", ref.Path != "")
		rejectValueRefField(add, path, "node_id", ref.NodeID != "")
		rejectValueRefField(add, path, "iteration", ref.Iteration != "")
		if ref.Default != nil {
			scanValueRefRawShallow(report, nodeID, joinPath(path, "default"), *ref.Default)
			rejectValueRefField(add, path, "default", true)
		}
	default:
		add(joinPath(path, "source"), CodeEnumInvalid, "invalid value source")
		if ref.Default != nil {
			scanValueRefRawShallow(report, nodeID, joinPath(path, "default"), *ref.Default)
		}
	}
}

func rejectValueRefField(add func(string, string, string), path, field string, present bool) {
	if present {
		add(joinPath(path, field), CodeUnknownField, "field is forbidden for this tagged-union variant")
	}
}

func validateLiteralDefaultBasic(report *Report, nodeID, path string, ref *ValueRef) {
	add := func(issuePath, code, message string) {
		if nodeID == "" {
			report.Add(PhaseDTO, issuePath, code, message)
		} else {
			report.AddNode(PhaseDTO, issuePath, nodeID, code, message)
		}
	}
	if len(ref.Value) != 0 {
		validateRawJSONValue(report, nodeID, joinPath(path, "value"), ref.Value)
	}
	if ref.Source != ValueLiteral {
		add(path, CodeTypeInvalid, "default must be a literal ValueRef")
	}
	if ref.Default != nil {
		scanValueRefRawShallow(report, nodeID, joinPath(path, "default"), *ref.Default)
		add(joinPath(path, "default"), CodeTypeInvalid, "default must not be recursive")
	}
	if ref.Source == ValueLiteral {
		if ref.NodeID != "" {
			add(joinPath(path, "node_id"), CodeTypeInvalid, "literal default must not declare node_id")
		}
		if ref.Path != "" {
			add(joinPath(path, "path"), CodeTypeInvalid, "literal default must not declare path")
		}
		if ref.Iteration != "" {
			add(joinPath(path, "iteration"), CodeTypeInvalid, "literal default must not declare iteration")
		}
		if len(ref.Value) == 0 {
			add(joinPath(path, "value"), CodeFieldRequired, "literal default value is required")
		}
	}
}

func validateIdentityReferences(ctx ValidationContext) Report {
	var report Report
	graph := ctx.Graph
	nodeIDs := make(map[string]struct{}, len(graph.Nodes))
	edgeIDs := make(map[string]struct{}, len(graph.Edges))

	for index, node := range graph.Nodes {
		path := fmt.Sprintf("/nodes/%d/id", index)
		if _, exists := nodeIDs[node.ID]; exists {
			report.AddNode(PhaseIdentityReferences, path, node.ID, CodeNodeIDDuplicate, "node id must be unique")
		}
		nodeIDs[node.ID] = struct{}{}
	}
	for index, edge := range graph.Edges {
		path := fmt.Sprintf("/edges/%d/id", index)
		if _, exists := edgeIDs[edge.ID]; exists {
			report.Add(PhaseIdentityReferences, path, CodeEdgeIDDuplicate, "edge id must be unique")
		}
		edgeIDs[edge.ID] = struct{}{}
	}
	if _, exists := nodeIDs[graph.EntryNodeID]; !exists {
		report.Add(PhaseIdentityReferences, "/entry_node_id", CodeEntryNotFound, "entry node does not exist")
	}

	for index, edge := range graph.Edges {
		path := fmt.Sprintf("/edges/%d", index)
		if _, exists := nodeIDs[edge.FromNodeID]; !exists {
			report.Add(PhaseIdentityReferences, joinPath(path, "from_node_id"), CodeEdgeSourceNotFound, "edge source node does not exist")
		}
		if _, exists := nodeIDs[edge.ToNodeID]; !exists {
			report.Add(PhaseIdentityReferences, joinPath(path, "to_node_id"), CodeEdgeTargetNotFound, "edge target node does not exist")
		}
		if edge.Predicate != nil {
			validatePredicateReferences(&report, "", joinPath(path, "predicate"), *edge.Predicate, nodeIDs)
		}
	}

	for index, node := range graph.Nodes {
		path := fmt.Sprintf("/nodes/%d", index)
		configPath := joinPath(path, "config")
		switch config := node.Config.(type) {
		case ParallelConfig:
			validateNodeReference(&report, node.ID, joinPath(configPath, "join_node_id"), config.JoinNodeID, nodeIDs)
		case LoopConfig:
			validateNodeReference(&report, node.ID, joinPath(configPath, "latch_node_id"), config.LatchNodeID, nodeIDs)
			validatePredicateReferences(&report, node.ID, joinPath(configPath, "continue_predicate"), config.ContinuePredicate, nodeIDs)
		}
		for _, name := range sortedInputNames(node.Inputs) {
			validateValueRefReferences(&report, node.ID, joinPath(joinPath(joinPath(path, "inputs"), name), "value"), node.Inputs[name].Value, nodeIDs)
		}
		switch config := node.Config.(type) {
		case TransformConfig:
			if config.Value != nil {
				validateValueRefReferences(&report, node.ID, joinPath(configPath, "value"), *config.Value, nodeIDs)
			}
			for _, name := range sortedValueRefNames(config.Fields) {
				validateValueRefReferences(&report, node.ID, joinPath(joinPath(configPath, "fields"), name), config.Fields[name], nodeIDs)
			}
			for itemIndex, ref := range config.Items {
				validateValueRefReferences(&report, node.ID, fmt.Sprintf("%s/%d", joinPath(configPath, "items"), itemIndex), ref, nodeIDs)
			}
		case DeliverConfig:
			validateValueRefReferences(&report, node.ID, joinPath(configPath, "result"), config.Result, nodeIDs)
		}
	}
	return report
}

func validatePredicateReferences(report *Report, nodeID, path string, predicate Predicate, nodeIDs map[string]struct{}) {
	validateValueRefReferences(report, nodeID, joinPath(path, "left"), predicate.Left, nodeIDs)
	if predicate.Right != nil {
		validateValueRefReferences(report, nodeID, joinPath(path, "right"), *predicate.Right, nodeIDs)
	}
}

func validateValueRefReferences(report *Report, nodeID, path string, ref ValueRef, nodeIDs map[string]struct{}) {
	if ref.Source == ValueNodeOutput {
		validateNodeReference(report, nodeID, joinPath(path, "node_id"), ref.NodeID, nodeIDs)
	}
}

func validateNodeReference(report *Report, nodeID, path, referencedID string, nodeIDs map[string]struct{}) {
	if _, exists := nodeIDs[referencedID]; exists {
		return
	}
	if nodeID == "" {
		report.Add(PhaseIdentityReferences, path, CodeNodeReferenceNotFound, "referenced node does not exist")
	} else {
		report.AddNode(PhaseIdentityReferences, path, nodeID, CodeNodeReferenceNotFound, "referenced node does not exist")
	}
}

func validateTopology(ctx ValidationContext) Report {
	var report Report
	graph := ctx.Graph
	nodeIndex := make(map[string]int, len(graph.Nodes))
	for index, node := range graph.Nodes {
		nodeIndex[node.ID] = index
	}
	outgoing := make([][]int, len(graph.Nodes))
	for edgeIndex, edge := range graph.Edges {
		from := nodeIndex[edge.FromNodeID]
		outgoing[from] = append(outgoing[from], edgeIndex)
	}

	entry := nodeIndex[graph.EntryNodeID]
	reachable := make([]bool, len(graph.Nodes))
	queue := []int{entry}
	reachable[entry] = true
	for len(queue) != 0 {
		current := queue[0]
		queue = queue[1:]
		for _, edgeIndex := range outgoing[current] {
			next := nodeIndex[graph.Edges[edgeIndex].ToNodeID]
			if !reachable[next] {
				reachable[next] = true
				queue = append(queue, next)
			}
		}
	}
	for index, node := range graph.Nodes {
		if !reachable[index] {
			report.AddNode(PhaseTopology, fmt.Sprintf("/nodes/%d", index), node.ID, CodeNodeUnreachable, "node is not reachable from entry")
		}
	}

	loopByLatch := buildLoopHeaderIndex(graph.Nodes)
	for index, node := range graph.Nodes {
		edges := outgoing[index]
		if !validOutgoingCardinality(node, edges, graph.Edges, loopByLatch) {
			report.AddNode(PhaseTopology, fmt.Sprintf("/nodes/%d", index), node.ID, CodeEdgeCardinalityInvalid, "node outgoing edge cardinality is invalid")
		}
	}

	good := make([]bool, len(graph.Nodes))
	baseTerminal := make([]bool, len(graph.Nodes))
	remaining := make([]int, len(graph.Nodes))
	goodSuccessors := make([]int, len(graph.Nodes))
	continuationIncoming := make([][]int, len(graph.Nodes))
	loopExitTargets := make(map[string][]int)
	for _, edge := range graph.Edges {
		source := graph.Nodes[nodeIndex[edge.FromNodeID]]
		if source.Type == NodeLoop && edge.Route == RouteExit {
			loopExitTargets[source.ID] = append(loopExitTargets[source.ID], nodeIndex[edge.ToNodeID])
		}
	}
	type latchHeaderPair struct {
		latch  string
		header string
	}
	seenVirtualExit := make(map[latchHeaderPair]struct{})
	terminalCount := 0
	for index, node := range graph.Nodes {
		if node.Type == NodeDeliver || node.Type == NodeHandoff {
			baseTerminal[index] = true
			terminalCount++
		}
		for _, edgeIndex := range outgoing[index] {
			edge := graph.Edges[edgeIndex]
			if loopByLatch.exactBackEdge(edge) {
				pair := latchHeaderPair{latch: edge.FromNodeID, header: edge.ToNodeID}
				if _, exists := seenVirtualExit[pair]; exists {
					continue
				}
				seenVirtualExit[pair] = struct{}{}
				for _, target := range loopExitTargets[edge.ToNodeID] {
					remaining[index]++
					continuationIncoming[target] = append(continuationIncoming[target], index)
				}
				continue
			}
			if !routeAllowedForNode(node, edge, loopByLatch) {
				continue
			}
			target := nodeIndex[edge.ToNodeID]
			remaining[index]++
			continuationIncoming[target] = append(continuationIncoming[target], index)
		}
	}
	if terminalCount == 0 {
		report.Add(PhaseTopology, "/nodes", CodeSuccessTerminalMissing, "graph must contain a deliver or handoff success terminal")
	}
	goodQueue := make([]int, 0)
	for index := range graph.Nodes {
		if baseTerminal[index] && remaining[index] == 0 {
			good[index] = true
			goodQueue = append(goodQueue, index)
		}
	}
	for len(goodQueue) != 0 {
		current := goodQueue[0]
		goodQueue = goodQueue[1:]
		for _, previous := range continuationIncoming[current] {
			if good[previous] {
				continue
			}
			goodSuccessors[previous]++
			if goodSuccessors[previous] == remaining[previous] {
				good[previous] = true
				goodQueue = append(goodQueue, previous)
			}
		}
	}
	for index, node := range graph.Nodes {
		if reachable[index] && !good[index] {
			report.AddNode(PhaseTopology, fmt.Sprintf("/nodes/%d", index), node.ID, CodeSuccessPathUnterminated, "a possible continuing path does not terminate")
		}
	}

	return report
}

type loopHeaderIndex map[string]map[string]struct{}

func buildLoopHeaderIndex(nodes []Node) loopHeaderIndex {
	result := make(loopHeaderIndex)
	for _, node := range nodes {
		config, ok := node.Config.(LoopConfig)
		if !ok {
			continue
		}
		headers := result[config.LatchNodeID]
		if headers == nil {
			headers = make(map[string]struct{})
			result[config.LatchNodeID] = headers
		}
		headers[node.ID] = struct{}{}
	}
	return result
}

func (index loopHeaderIndex) exactBackEdge(edge Edge) bool {
	if edge.Route != RouteBack {
		return false
	}
	_, exists := index[edge.FromNodeID][edge.ToNodeID]
	return exists
}

func validOutgoingCardinality(node Node, edgeIndexes []int, edges []Edge, loopByLatch loopHeaderIndex) bool {
	counts := make(map[EdgeRoute]int)
	allowedEdgeCount := 0
	for _, edgeIndex := range edgeIndexes {
		edge := edges[edgeIndex]
		if !routeAllowedForNode(node, edge, loopByLatch) {
			continue
		}
		counts[edge.Route]++
		allowedEdgeCount++
	}
	only := func(routes ...EdgeRoute) bool {
		total := 0
		for _, route := range routes {
			total += counts[route]
		}
		return total == allowedEdgeCount
	}
	isLatch := len(loopByLatch[node.ID]) != 0

	switch node.Type {
	case NodeLead, NodeTransform:
		if isLatch {
			return counts[RouteBack] == 1 && counts[RouteFailure] <= 1 && only(RouteBack, RouteFailure)
		}
		return counts[RouteSuccess] == 1 && counts[RouteFailure] <= 1 && only(RouteSuccess, RouteFailure)
	case NodeWorker:
		config, _ := node.Config.(WorkerConfig)
		if config.Kind == WorkerDispatch {
			return counts[RouteJoin] == 1 && only(RouteJoin)
		}
		if isLatch {
			return counts[RouteBack] == 1 && counts[RouteFailure] <= 1 && only(RouteBack, RouteFailure)
		}
		return counts[RouteSuccess] == 1 && counts[RouteFailure] <= 1 && only(RouteSuccess, RouteFailure)
	case NodeCondition:
		return counts[RouteCase] >= 1 && counts[RouteDefault] == 1 && counts[RouteFailure] <= 1 && only(RouteCase, RouteDefault, RouteFailure)
	case NodeParallel:
		return counts[RouteBranch] >= 2 && only(RouteBranch)
	case NodeJoin:
		return counts[RouteSuccess] == 1 && counts[RouteFailure] <= 1 && only(RouteSuccess, RouteFailure)
	case NodeWait:
		return counts[RouteSuccess] == 1 && counts[RouteTimeout] <= 1 && counts[RouteFailure] <= 1 && only(RouteSuccess, RouteTimeout, RouteFailure)
	case NodeLoop:
		return counts[RouteBody] == 1 && counts[RouteExit] == 1 && only(RouteBody, RouteExit)
	case NodeDeliver:
		return allowedEdgeCount == 0
	case NodeHandoff:
		return counts[RouteFailure] == 1 && only(RouteFailure)
	default:
		return false
	}
}

func validateRoutes(ctx ValidationContext) Report {
	var report Report
	graph := ctx.Graph
	nodeByID := make(map[string]Node, len(graph.Nodes))
	for _, node := range graph.Nodes {
		nodeByID[node.ID] = node
	}
	loopByLatch := buildLoopHeaderIndex(graph.Nodes)
	for index, edge := range graph.Edges {
		path := fmt.Sprintf("/edges/%d", index)
		source := nodeByID[edge.FromNodeID]
		if edge.Route == RouteBack {
			if !loopByLatch.exactBackEdge(edge) {
				report.Add(PhaseRoutes, joinPath(path, "route"), CodeBackRouteInvalid, "back route must connect an exact loop latch to its header")
			}
		} else if !routeAllowedForNode(source, edge, loopByLatch) {
			report.AddNode(PhaseRoutes, joinPath(path, "route"), source.ID, CodeRouteNotAllowed, "route is not allowed for source node type")
		}

		predicateAllowed := source.Type == NodeCondition && edge.Route == RouteCase
		if edge.Priority != nil && !predicateAllowed {
			report.AddNode(PhaseRoutes, joinPath(path, "priority"), source.ID, CodeEdgePriorityForbidden, "priority is only allowed on condition case edges")
		}
		if edge.Predicate != nil && !predicateAllowed {
			report.AddNode(PhaseRoutes, joinPath(path, "predicate"), source.ID, CodeEdgePredicateForbidden, "predicate is only allowed on condition case edges")
		}
		if edge.Route == RouteTimeout && !nodeHasTimeout(source) {
			report.AddNode(PhaseRoutes, joinPath(path, "route"), source.ID, CodeTimeoutRouteWithoutTimeout, "timeout route requires timeout_seconds")
		}
	}
	return report
}

func routeAllowedForNode(node Node, edge Edge, loopByLatch loopHeaderIndex) bool {
	if edge.Route == RouteBack {
		return loopByLatch.exactBackEdge(edge)
	}
	switch node.Type {
	case NodeLead, NodeTransform:
		return edge.Route == RouteSuccess || edge.Route == RouteFailure
	case NodeWorker:
		config, _ := node.Config.(WorkerConfig)
		if config.Kind == WorkerDispatch {
			return edge.Route == RouteJoin
		}
		return edge.Route == RouteSuccess || edge.Route == RouteFailure
	case NodeCondition:
		return edge.Route == RouteCase || edge.Route == RouteDefault || edge.Route == RouteFailure
	case NodeParallel:
		return edge.Route == RouteBranch
	case NodeJoin:
		return edge.Route == RouteSuccess || edge.Route == RouteFailure
	case NodeWait:
		return edge.Route == RouteSuccess || edge.Route == RouteTimeout || edge.Route == RouteFailure
	case NodeLoop:
		return edge.Route == RouteBody || edge.Route == RouteExit
	case NodeDeliver:
		return false
	case NodeHandoff:
		return edge.Route == RouteFailure
	default:
		return false
	}
}

func nodeHasTimeout(node Node) bool {
	switch config := node.Config.(type) {
	case WaitConfig:
		return config.TimeoutSeconds != nil
	default:
		return false
	}
}

func sortedInputNames(values map[string]InputBinding) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedValueRefNames(values map[string]ValueRef) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
