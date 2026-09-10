package teamforge

// Deterministic workflow JSON encoder (plan §10.2.3 "严格编码"). The tool
// builds the typed machine model, and this encoder regenerates the canonical
// trigger_config / graph_definition RawMessages with the exact field sets the
// strict machine decoders require. The one divergence from plain
// json.Marshal is deliberate: ValueRef fields such as node_output.path are
// required by the decoders even when empty, so the encoder emits them
// explicitly instead of dropping them through omitempty.

import (
	"encoding/json"

	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

// encodeWorkflowDraftJSON encodes one draft into the two RawMessages the
// workflow store persists.
func encodeWorkflowDraftJSON(trigger machine.TriggerConfig, graph machine.GraphDefinition) (json.RawMessage, json.RawMessage, error) {
	triggerJSON, err := encodeWorkflowTrigger(trigger)
	if err != nil {
		return nil, nil, err
	}
	graphJSON, err := encodeWorkflowGraph(graph)
	if err != nil {
		return nil, nil, err
	}
	return triggerJSON, graphJSON, nil
}

func encodeWorkflowTrigger(trigger machine.TriggerConfig) (json.RawMessage, error) {
	payload := map[string]any{
		"schema_version": trigger.SchemaVersion,
		"type":           trigger.Type,
		"config":         workflowTriggerVariantJSON(trigger),
	}
	if trigger.Delivery != nil {
		payload["delivery"] = map[string]any{
			"kind": trigger.Delivery.Kind,
		}
		if trigger.Delivery.Kind != machine.DeliveryJobRecord {
			payload["delivery"].(map[string]any)["ref"] = trigger.Delivery.Ref
		}
	}
	return json.Marshal(payload)
}

func workflowTriggerVariantJSON(trigger machine.TriggerConfig) any {
	switch config := trigger.Config.(type) {
	case machine.ConversationExplicitConfig:
		return map[string]any{}
	case machine.ConversationAutoConfig:
		return map[string]any{"catalog_key": config.CatalogKey}
	case machine.ScheduleConfig:
		return map[string]any{"schedule_id": config.ScheduleID}
	case machine.APIConfig:
		return map[string]any{"endpoint_key": config.EndpointKey}
	case machine.EventConfig:
		return map[string]any{"event_type": config.EventType}
	default:
		return map[string]any{}
	}
}

func encodeWorkflowGraph(graph machine.GraphDefinition) (json.RawMessage, error) {
	nodes := make([]any, 0, len(graph.Nodes))
	for _, node := range graph.Nodes {
		nodes = append(nodes, workflowNodeJSON(node))
	}
	edges := make([]any, 0, len(graph.Edges))
	for _, edge := range graph.Edges {
		edges = append(edges, workflowEdgeJSON(edge))
	}
	payload := map[string]any{
		"schema_version":  graph.SchemaVersion,
		"entry_node_id":   graph.EntryNodeID,
		"input_contract":  workflowContractJSON(graph.InputContract),
		"output_contract": workflowContractJSON(graph.OutputContract),
		"nodes":           nodes,
		"edges":           edges,
	}
	if graph.DeliveryContract != nil {
		payload["delivery_contract"] = graph.DeliveryContract
	}
	return json.Marshal(payload)
}

func workflowContractJSON(contract machine.OutputContract) map[string]any {
	payload := map[string]any{"type": contract.Type}
	if len(contract.Schema) != 0 {
		payload["schema"] = contract.Schema
	}
	return payload
}

func workflowNodeJSON(node machine.Node) map[string]any {
	payload := map[string]any{
		"id":     node.ID,
		"type":   node.Type,
		"config": workflowNodeConfigJSON(node.Type, node.Config),
	}
	if node.Label != "" {
		payload["label"] = node.Label
	}
	if len(node.Inputs) != 0 {
		inputs := make(map[string]any, len(node.Inputs))
		for name, binding := range node.Inputs {
			inputs[name] = map[string]any{
				"expected_type": binding.ExpectedType,
				"value":         workflowValueRefJSON(binding.Value),
			}
		}
		payload["inputs"] = inputs
	}
	if node.Output != nil {
		payload["output"] = workflowContractJSON(*node.Output)
	}
	return payload
}

func workflowNodeConfigJSON(nodeType machine.NodeType, config machine.NodeConfig) any {
	switch typed := config.(type) {
	case machine.LeadConfig:
		return map[string]any{"instruction": typed.Instruction}
	case machine.WorkerConfig:
		return map[string]any{
			"agent_id":           typed.AgentID,
			"agent_version":      typed.AgentVersion,
			"kind":               typed.Kind,
			"result_requirement": typed.ResultRequirement,
		}
	case machine.TransformConfig:
		payload := map[string]any{"operation": typed.Operation}
		switch typed.Operation {
		case machine.TransformIdentity:
			if typed.Value != nil {
				payload["value"] = workflowValueRefJSON(*typed.Value)
			}
		case machine.TransformObject:
			fields := make(map[string]any, len(typed.Fields))
			for name, ref := range typed.Fields {
				fields[name] = workflowValueRefJSON(ref)
			}
			payload["fields"] = fields
		case machine.TransformArray:
			items := make([]any, len(typed.Items))
			for i, ref := range typed.Items {
				items[i] = workflowValueRefJSON(ref)
			}
			payload["items"] = items
		}
		return payload
	case machine.ConditionConfig:
		return map[string]any{}
	case machine.ParallelConfig:
		return map[string]any{"join_node_id": typed.JoinNodeID}
	case machine.JoinConfig:
		payload := map[string]any{"policy": typed.Policy}
		if typed.SuccessCount != nil {
			payload["success_count"] = *typed.SuccessCount
		}
		if typed.DeadlineSeconds != nil {
			payload["deadline_seconds"] = *typed.DeadlineSeconds
		}
		return payload
	case machine.WaitConfig:
		payload := map[string]any{"resume_schema": typed.ResumeSchema}
		if typed.Kind != "" {
			payload["kind"] = typed.Kind
		}
		if typed.TimeoutSeconds != nil {
			payload["timeout_seconds"] = *typed.TimeoutSeconds
		}
		if typed.Task != nil {
			payload["task"] = typed.Task
		}
		return payload
	case machine.LoopConfig:
		return map[string]any{
			"max_iterations":     typed.MaxIterations,
			"latch_node_id":      typed.LatchNodeID,
			"continue_predicate": workflowPredicateJSON(typed.ContinuePredicate),
		}
	case machine.DeliverConfig:
		return map[string]any{"result": workflowValueRefJSON(typed.Result)}
	case machine.HandoffConfig:
		payload := map[string]any{
			"agent_id":      typed.AgentID,
			"agent_version": typed.AgentVersion,
			"instruction":   typed.Instruction,
		}
		if typed.TimeoutSeconds != nil {
			payload["timeout_seconds"] = *typed.TimeoutSeconds
		}
		return payload
	default:
		_ = nodeType
		return map[string]any{}
	}
}

func workflowEdgeJSON(edge machine.Edge) map[string]any {
	payload := map[string]any{
		"id":           edge.ID,
		"from_node_id": edge.FromNodeID,
		"to_node_id":   edge.ToNodeID,
		"route":        edge.Route,
	}
	if edge.Priority != nil {
		payload["priority"] = *edge.Priority
	}
	if edge.Predicate != nil {
		payload["predicate"] = workflowPredicateJSON(*edge.Predicate)
	}
	return payload
}

func workflowPredicateJSON(predicate machine.Predicate) map[string]any {
	payload := map[string]any{
		"left":     workflowValueRefJSON(predicate.Left),
		"operator": predicate.Operator,
	}
	if predicate.Right != nil {
		payload["right"] = workflowValueRefJSON(*predicate.Right)
	}
	return payload
}

// workflowValueRefJSON emits the tagged-union fields the strict decoder
// requires, including empty path/node_id for run_input/node_output sources.
func workflowValueRefJSON(ref machine.ValueRef) map[string]any {
	payload := map[string]any{"source": ref.Source}
	switch ref.Source {
	case machine.ValueRunInput:
		payload["path"] = ref.Path
	case machine.ValueNodeOutput:
		payload["node_id"] = ref.NodeID
		payload["path"] = ref.Path
		if ref.Iteration != "" {
			payload["iteration"] = ref.Iteration
		}
		if ref.Default != nil {
			payload["default"] = workflowValueRefJSON(*ref.Default)
		}
	case machine.ValueLiteral:
		payload["value"] = ref.Value
	}
	return payload
}
