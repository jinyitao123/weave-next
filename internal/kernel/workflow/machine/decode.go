package machine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jinyitao123/weave/internal/base/deliverable"
)

const maxSafeIntegerText = "9007199254740991"
const maxJSONNestingDepth = 256

type dtoError struct {
	path   string
	nodeID string
	code   string
	err    error
}

func (e *dtoError) Error() string {
	if e.err == nil {
		return e.code
	}
	return e.err.Error()
}

func DecodeTriggerConfigV1(raw json.RawMessage) (TriggerConfig, *Report) {
	var result TriggerConfig
	if issue := inspectJSON(raw); issue != nil {
		return result, reportFor(issue)
	}

	object, issue := objectFields(raw, "")
	if issue != nil {
		return result, reportFor(issue)
	}
	if issue = rejectUnknown(object, "", "schema_version", "type", "config", "delivery"); issue != nil {
		return result, reportFor(issue)
	}
	if issue = requireField(object, "schema_version", "/schema_version", CodeSchemaVersionRequired); issue != nil {
		return result, reportFor(issue)
	}
	schemaVersion, issue := integerField(object["schema_version"], "/schema_version")
	if issue != nil {
		return result, reportFor(issue)
	}
	if schemaVersion != SchemaVersionV1 {
		return result, reportFor(newDTOError("/schema_version", CodeSchemaVersionUnsupported, "unsupported schema_version"))
	}
	result.SchemaVersion = int(schemaVersion)

	if issue = requireField(object, "type", "/type", CodeDiscriminatorRequired); issue != nil {
		return result, reportFor(issue)
	}
	triggerType, issue := stringField(object["type"], "/type")
	if issue != nil {
		return result, reportFor(issue)
	}
	result.Type = TriggerType(triggerType)
	if !validTriggerType(result.Type) {
		return TriggerConfig{}, reportFor(newDTOError("/type", CodeEnumInvalid, "invalid trigger type"))
	}
	if issue = requireField(object, "config", "/config", CodeFieldRequired); issue != nil {
		return TriggerConfig{}, reportFor(issue)
	}

	switch result.Type {
	case TriggerConversationExplicit:
		result.Config, issue = decodeEmptyTriggerConfig(object["config"], "/config")
	case TriggerConversationAuto:
		result.Config, issue = decodeConversationAutoConfig(object["config"], "/config")
	case TriggerSchedule:
		result.Config, issue = decodeScheduleConfig(object["config"], "/config")
	case TriggerAPI:
		result.Config, issue = decodeAPIConfig(object["config"], "/config")
	case TriggerEvent:
		result.Config, issue = decodeEventConfig(object["config"], "/config")
	}
	if issue != nil {
		return TriggerConfig{}, reportFor(issue)
	}

	deliveryRaw, hasDelivery := object["delivery"]
	if result.Type == TriggerConversationExplicit || result.Type == TriggerConversationAuto {
		if hasDelivery {
			return TriggerConfig{}, reportFor(newDTOError("/delivery", CodeDeliveryForbidden, "delivery is forbidden for session triggers"))
		}
		return result, nil
	}
	if !hasDelivery {
		return TriggerConfig{}, reportFor(newDTOError("/delivery", CodeDeliveryRequired, "delivery is required for non-session triggers"))
	}
	result.Delivery, issue = decodeDelivery(deliveryRaw, "/delivery")
	if issue != nil {
		return TriggerConfig{}, reportFor(issue)
	}
	return result, nil
}

func DecodeGraphDefinitionV1(raw json.RawMessage) (GraphDefinition, *Report) {
	var result GraphDefinition
	if issue := inspectJSON(raw); issue != nil {
		issue.nodeID = bestEffortNodeIDForPath(raw, issue.path)
		return result, reportFor(issue)
	}
	object, issue := objectFields(raw, "")
	if issue != nil {
		return result, reportFor(issue)
	}
	if issue = rejectUnknown(object, "", "schema_version", "entry_node_id", "input_contract", "output_contract", "delivery_contract", "nodes", "edges"); issue != nil {
		return result, reportFor(issue)
	}

	if issue = requireField(object, "schema_version", "/schema_version", CodeSchemaVersionRequired); issue != nil {
		return result, reportFor(issue)
	}
	schemaVersion, issue := integerField(object["schema_version"], "/schema_version")
	if issue != nil {
		return result, reportFor(issue)
	}
	if schemaVersion != SchemaVersionV1 {
		return result, reportFor(newDTOError("/schema_version", CodeSchemaVersionUnsupported, "unsupported schema_version"))
	}
	result.SchemaVersion = int(schemaVersion)

	required := []string{"entry_node_id", "input_contract", "output_contract", "nodes", "edges"}
	for _, name := range required {
		if issue = requireField(object, name, "/"+name, CodeFieldRequired); issue != nil {
			return GraphDefinition{}, reportFor(issue)
		}
	}
	if result.EntryNodeID, issue = stringField(object["entry_node_id"], "/entry_node_id"); issue != nil {
		return GraphDefinition{}, reportFor(issue)
	}
	if result.InputContract, issue = decodeOutputContract(object["input_contract"], "/input_contract"); issue != nil {
		return GraphDefinition{}, reportFor(issue)
	}
	if result.OutputContract, issue = decodeOutputContract(object["output_contract"], "/output_contract"); issue != nil {
		return GraphDefinition{}, reportFor(issue)
	}
	if raw, ok := object["delivery_contract"]; ok {
		contract, err := deliverable.DecodeDeliveryContract(raw)
		if err != nil {
			return GraphDefinition{}, reportFor(newDTOError("/delivery_contract", CodeContractInvalid, err.Error()))
		}
		result.DeliveryContract = contract
	}

	nodeValues, issue := arrayValues(object["nodes"], "/nodes")
	if issue != nil {
		return GraphDefinition{}, reportFor(issue)
	}
	result.Nodes = make([]Node, len(nodeValues))
	for index, nodeRaw := range nodeValues {
		path := fmt.Sprintf("/nodes/%d", index)
		result.Nodes[index], issue = decodeNode(nodeRaw, path)
		if issue != nil {
			return GraphDefinition{}, reportFor(issue)
		}
	}

	edgeValues, issue := arrayValues(object["edges"], "/edges")
	if issue != nil {
		return GraphDefinition{}, reportFor(issue)
	}
	result.Edges = make([]Edge, len(edgeValues))
	for index, edgeRaw := range edgeValues {
		path := fmt.Sprintf("/edges/%d", index)
		result.Edges[index], issue = decodeEdge(edgeRaw, path)
		if issue != nil {
			return GraphDefinition{}, reportFor(issue)
		}
	}
	return result, nil
}

func decodeEmptyTriggerConfig(raw json.RawMessage, path string) (TriggerVariant, *dtoError) {
	object, issue := objectFields(raw, path)
	if issue != nil {
		return nil, issue
	}
	if issue = rejectUnknown(object, path); issue != nil {
		return nil, issue
	}
	return ConversationExplicitConfig{}, nil
}

func decodeConversationAutoConfig(raw json.RawMessage, path string) (TriggerVariant, *dtoError) {
	object, issue := objectFields(raw, path)
	if issue != nil {
		return nil, issue
	}
	if issue = rejectUnknown(object, path, "catalog_key"); issue != nil {
		return nil, issue
	}
	if issue = requireField(object, "catalog_key", joinPath(path, "catalog_key"), CodeFieldRequired); issue != nil {
		return nil, issue
	}
	value, issue := stringField(object["catalog_key"], joinPath(path, "catalog_key"))
	return ConversationAutoConfig{CatalogKey: value}, issue
}

func decodeScheduleConfig(raw json.RawMessage, path string) (TriggerVariant, *dtoError) {
	object, issue := objectFields(raw, path)
	if issue != nil {
		return nil, issue
	}
	if issue = rejectUnknown(object, path, "schedule_id"); issue != nil {
		return nil, issue
	}
	if issue = requireField(object, "schedule_id", joinPath(path, "schedule_id"), CodeFieldRequired); issue != nil {
		return nil, issue
	}
	value, issue := stringField(object["schedule_id"], joinPath(path, "schedule_id"))
	return ScheduleConfig{ScheduleID: value}, issue
}

func decodeAPIConfig(raw json.RawMessage, path string) (TriggerVariant, *dtoError) {
	object, issue := objectFields(raw, path)
	if issue != nil {
		return nil, issue
	}
	if issue = rejectUnknown(object, path, "endpoint_key"); issue != nil {
		return nil, issue
	}
	if issue = requireField(object, "endpoint_key", joinPath(path, "endpoint_key"), CodeFieldRequired); issue != nil {
		return nil, issue
	}
	value, issue := stringField(object["endpoint_key"], joinPath(path, "endpoint_key"))
	return APIConfig{EndpointKey: value}, issue
}

func decodeEventConfig(raw json.RawMessage, path string) (TriggerVariant, *dtoError) {
	object, issue := objectFields(raw, path)
	if issue != nil {
		return nil, issue
	}
	if issue = rejectUnknown(object, path, "event_type"); issue != nil {
		return nil, issue
	}
	if issue = requireField(object, "event_type", joinPath(path, "event_type"), CodeFieldRequired); issue != nil {
		return nil, issue
	}
	value, issue := stringField(object["event_type"], joinPath(path, "event_type"))
	return EventConfig{EventType: value}, issue
}

func decodeDelivery(raw json.RawMessage, path string) (*Delivery, *dtoError) {
	object, issue := objectFields(raw, path)
	if issue != nil {
		return nil, issue
	}
	if issue = rejectUnknown(object, path, "kind", "ref"); issue != nil {
		return nil, issue
	}
	if issue = requireField(object, "kind", joinPath(path, "kind"), CodeDiscriminatorRequired); issue != nil {
		return nil, issue
	}
	kindValue, issue := stringField(object["kind"], joinPath(path, "kind"))
	if issue != nil {
		return nil, issue
	}
	kind := DeliveryKind(kindValue)
	if kind != DeliveryJobRecord && kind != DeliveryCallbackRef && kind != DeliveryTargetRef {
		return nil, newDTOError(joinPath(path, "kind"), CodeEnumInvalid, "invalid delivery kind")
	}
	refRaw, hasRef := object["ref"]
	if kind == DeliveryJobRecord {
		if hasRef {
			return nil, newDTOError(joinPath(path, "ref"), CodeUnknownField, "ref is not allowed for job_record")
		}
		return &Delivery{Kind: kind}, nil
	}
	if !hasRef {
		return nil, newDTOError(joinPath(path, "ref"), CodeFieldRequired, "ref is required")
	}
	ref, issue := stringField(refRaw, joinPath(path, "ref"))
	if issue != nil {
		return nil, issue
	}
	return &Delivery{Kind: kind, Ref: ref}, nil
}

func decodeNode(raw json.RawMessage, path string) (result Node, issue *dtoError) {
	var parsedNodeID string
	defer func() {
		if issue != nil && parsedNodeID != "" {
			issue.nodeID = parsedNodeID
		}
	}()

	object, issue := objectFields(raw, path)
	if issue != nil {
		return result, issue
	}
	parsedNodeID = bestEffortNodeID(raw)
	if issue = rejectUnknown(object, path, "id", "type", "label", "inputs", "output", "config"); issue != nil {
		return result, issue
	}
	if issue = requireField(object, "id", joinPath(path, "id"), CodeFieldRequired); issue != nil {
		return result, issue
	}
	if result.ID, issue = stringField(object["id"], joinPath(path, "id")); issue != nil {
		return Node{}, issue
	}
	parsedNodeID = result.ID
	if issue = requireField(object, "type", joinPath(path, "type"), CodeDiscriminatorRequired); issue != nil {
		return Node{}, issue
	}
	nodeType, issue := stringField(object["type"], joinPath(path, "type"))
	if issue != nil {
		return Node{}, issue
	}
	result.Type = NodeType(nodeType)
	if !validNodeType(result.Type) {
		return Node{}, newDTOError(joinPath(path, "type"), CodeEnumInvalid, "invalid node type")
	}
	if issue = requireField(object, "config", joinPath(path, "config"), CodeFieldRequired); issue != nil {
		return Node{}, issue
	}

	if rawLabel, ok := object["label"]; ok {
		if result.Label, issue = stringField(rawLabel, joinPath(path, "label")); issue != nil {
			return Node{}, issue
		}
	}
	if rawInputs, ok := object["inputs"]; ok {
		if result.Inputs, issue = decodeInputs(rawInputs, joinPath(path, "inputs")); issue != nil {
			return Node{}, issue
		}
	}
	if rawOutput, ok := object["output"]; ok {
		output, outputIssue := decodeOutputContract(rawOutput, joinPath(path, "output"))
		if outputIssue != nil {
			return Node{}, outputIssue
		}
		result.Output = &output
	}
	result.Config, issue = decodeNodeConfig(result.Type, object["config"], joinPath(path, "config"))
	if issue != nil {
		return Node{}, issue
	}
	return result, nil
}

func decodeNodeConfig(nodeType NodeType, raw json.RawMessage, path string) (NodeConfig, *dtoError) {
	switch nodeType {
	case NodeLead:
		return decodeLeadConfig(raw, path)
	case NodeWorker:
		return decodeWorkerConfig(raw, path)
	case NodeTransform:
		return decodeTransformConfig(raw, path)
	case NodeCondition:
		return decodeConditionConfig(raw, path)
	case NodeParallel:
		return decodeParallelConfig(raw, path)
	case NodeJoin:
		return decodeJoinConfig(raw, path)
	case NodeWait:
		return decodeWaitConfig(raw, path)
	case NodeLoop:
		return decodeLoopConfig(raw, path)
	case NodeDeliver:
		return decodeDeliverConfig(raw, path)
	case NodeHandoff:
		return decodeHandoffConfig(raw, path)
	default:
		return nil, newDTOError(path, CodeEnumInvalid, "invalid node type")
	}
}

func decodeLeadConfig(raw json.RawMessage, path string) (NodeConfig, *dtoError) {
	object, issue := configObject(raw, path, "instruction")
	if issue != nil {
		return nil, issue
	}
	instruction, issue := requiredString(object, "instruction", path)
	return LeadConfig{Instruction: instruction}, issue
}

func decodeWorkerConfig(raw json.RawMessage, path string) (NodeConfig, *dtoError) {
	object, issue := configObject(raw, path, "agent_id", "agent_version", "kind", "result_requirement")
	if issue != nil {
		return nil, issue
	}
	agentID, issue := requiredString(object, "agent_id", path)
	if issue != nil {
		return nil, issue
	}
	if issue = requireField(object, "agent_version", joinPath(path, "agent_version"), CodeFieldRequired); issue != nil {
		return nil, issue
	}
	version, issue := integerField(object["agent_version"], joinPath(path, "agent_version"))
	if issue != nil {
		return nil, issue
	}
	kindValue, issue := requiredString(object, "kind", path)
	if issue != nil {
		return nil, issue
	}
	kind := WorkerKind(kindValue)
	if kind != WorkerConsult && kind != WorkerDispatch {
		return nil, newDTOError(joinPath(path, "kind"), CodeEnumInvalid, "invalid worker kind")
	}
	requirement, issue := requiredString(object, "result_requirement", path)
	if issue != nil {
		return nil, issue
	}
	return WorkerConfig{AgentID: agentID, AgentVersion: version, Kind: kind, ResultRequirement: requirement}, nil
}

func decodeTransformConfig(raw json.RawMessage, path string) (NodeConfig, *dtoError) {
	object, issue := configObject(raw, path, "operation", "value", "fields", "items")
	if issue != nil {
		return nil, issue
	}
	operationValue, issue := requiredString(object, "operation", path)
	if issue != nil {
		return nil, issue
	}
	operation := TransformOperation(operationValue)
	result := TransformConfig{Operation: operation}
	switch operation {
	case TransformIdentity:
		if issue = rejectUnknown(object, path, "operation", "value"); issue != nil {
			return nil, issue
		}
		if issue = requireField(object, "value", joinPath(path, "value"), CodeFieldRequired); issue != nil {
			return nil, issue
		}
		value, valueIssue := decodeValueRef(object["value"], joinPath(path, "value"))
		if valueIssue != nil {
			return nil, valueIssue
		}
		result.Value = &value
	case TransformObject:
		if issue = rejectUnknown(object, path, "operation", "fields"); issue != nil {
			return nil, issue
		}
		if issue = requireField(object, "fields", joinPath(path, "fields"), CodeFieldRequired); issue != nil {
			return nil, issue
		}
		result.Fields, issue = decodeValueRefMap(object["fields"], joinPath(path, "fields"))
		if issue != nil {
			return nil, issue
		}
	case TransformArray:
		if issue = rejectUnknown(object, path, "operation", "items"); issue != nil {
			return nil, issue
		}
		if issue = requireField(object, "items", joinPath(path, "items"), CodeFieldRequired); issue != nil {
			return nil, issue
		}
		values, valuesIssue := arrayValues(object["items"], joinPath(path, "items"))
		if valuesIssue != nil {
			return nil, valuesIssue
		}
		result.Items = make([]ValueRef, len(values))
		for index, valueRaw := range values {
			result.Items[index], issue = decodeValueRef(valueRaw, fmt.Sprintf("%s/%d", joinPath(path, "items"), index))
			if issue != nil {
				return nil, issue
			}
		}
	default:
		return nil, newDTOError(joinPath(path, "operation"), CodeEnumInvalid, "invalid transform operation")
	}
	return result, nil
}

func decodeConditionConfig(raw json.RawMessage, path string) (NodeConfig, *dtoError) {
	object, issue := configObject(raw, path)
	if issue != nil {
		return nil, issue
	}
	if len(object) != 0 {
		return nil, rejectUnknown(object, path)
	}
	return ConditionConfig{}, nil
}

func decodeParallelConfig(raw json.RawMessage, path string) (NodeConfig, *dtoError) {
	object, issue := configObject(raw, path, "join_node_id")
	if issue != nil {
		return nil, issue
	}
	value, issue := requiredString(object, "join_node_id", path)
	return ParallelConfig{JoinNodeID: value}, issue
}

func decodeJoinConfig(raw json.RawMessage, path string) (NodeConfig, *dtoError) {
	object, issue := configObject(raw, path, "policy", "success_count", "deadline_seconds")
	if issue != nil {
		return nil, issue
	}
	policyValue, issue := requiredString(object, "policy", path)
	if issue != nil {
		return nil, issue
	}
	policy := JoinPolicy(policyValue)
	result := JoinConfig{Policy: policy}
	switch policy {
	case JoinAllSuccess, JoinFailFast:
		if issue = rejectUnknown(object, path, "policy"); issue != nil {
			return nil, issue
		}
	case JoinQuorum:
		if issue = rejectUnknown(object, path, "policy", "success_count"); issue != nil {
			return nil, issue
		}
		if issue = requireField(object, "success_count", joinPath(path, "success_count"), CodeFieldRequired); issue != nil {
			return nil, issue
		}
		value, valueIssue := integerField(object["success_count"], joinPath(path, "success_count"))
		if valueIssue != nil {
			return nil, valueIssue
		}
		result.SuccessCount = &value
	case JoinDeadline:
		if issue = rejectUnknown(object, path, "policy", "deadline_seconds"); issue != nil {
			return nil, issue
		}
		if issue = requireField(object, "deadline_seconds", joinPath(path, "deadline_seconds"), CodeFieldRequired); issue != nil {
			return nil, issue
		}
		value, valueIssue := integerField(object["deadline_seconds"], joinPath(path, "deadline_seconds"))
		if valueIssue != nil {
			return nil, valueIssue
		}
		result.DeadlineSeconds = &value
	default:
		return nil, newDTOError(joinPath(path, "policy"), CodeEnumInvalid, "invalid join policy")
	}
	return result, nil
}

func decodeWaitConfig(raw json.RawMessage, path string) (NodeConfig, *dtoError) {
	object, issue := configObject(raw, path, "kind", "resume_schema", "timeout_seconds", "task")
	if issue != nil {
		return nil, issue
	}
	kind := WaitKind("")
	if rawKind, ok := object["kind"]; ok {
		value, valueIssue := stringField(rawKind, joinPath(path, "kind"))
		if valueIssue != nil {
			return nil, valueIssue
		}
		kind = WaitKind(value)
		if kind != WaitKindTimer && kind != WaitKindHuman {
			return nil, newDTOError(joinPath(path, "kind"), CodeEnumInvalid, "wait kind must be timer or human")
		}
	}
	if issue = requireField(object, "resume_schema", joinPath(path, "resume_schema"), CodeFieldRequired); issue != nil {
		return nil, issue
	}
	if _, issue = objectFields(object["resume_schema"], joinPath(path, "resume_schema")); issue != nil {
		return nil, issue
	}
	result := WaitConfig{Kind: kind, ResumeSchema: cloneRaw(object["resume_schema"])}
	if timeout, ok := object["timeout_seconds"]; ok {
		value, valueIssue := integerField(timeout, joinPath(path, "timeout_seconds"))
		if valueIssue != nil {
			return nil, valueIssue
		}
		result.TimeoutSeconds = &value
	}
	if rawTask, ok := object["task"]; ok {
		taskObject, taskIssue := configObject(rawTask, joinPath(path, "task"), "title", "instructions", "audience_ref")
		if taskIssue != nil {
			return nil, taskIssue
		}
		title, taskIssue := requiredString(taskObject, "title", joinPath(path, "task"))
		if taskIssue != nil {
			return nil, taskIssue
		}
		instructions, taskIssue := requiredString(taskObject, "instructions", joinPath(path, "task"))
		if taskIssue != nil {
			return nil, taskIssue
		}
		audienceRef := ""
		if rawAudience, exists := taskObject["audience_ref"]; exists {
			audienceRef, taskIssue = stringField(rawAudience, joinPath(joinPath(path, "task"), "audience_ref"))
			if taskIssue != nil {
				return nil, taskIssue
			}
		}
		result.Task = &HumanTaskConfig{Title: title, Instructions: instructions, AudienceRef: audienceRef}
	}
	if result.EffectiveKind() == WaitKindHuman && result.Task == nil {
		return nil, newDTOError(joinPath(path, "task"), CodeFieldRequired, "human wait requires task")
	}
	if result.EffectiveKind() == WaitKindTimer && result.Task != nil {
		return nil, newDTOError(joinPath(path, "task"), CodeUnknownField, "task is only allowed for human wait")
	}
	return result, nil
}

func decodeLoopConfig(raw json.RawMessage, path string) (NodeConfig, *dtoError) {
	object, issue := configObject(raw, path, "max_iterations", "latch_node_id", "continue_predicate")
	if issue != nil {
		return nil, issue
	}
	if issue = requireField(object, "max_iterations", joinPath(path, "max_iterations"), CodeFieldRequired); issue != nil {
		return nil, issue
	}
	iterations, issue := integerField(object["max_iterations"], joinPath(path, "max_iterations"))
	if issue != nil {
		return nil, issue
	}
	latch, issue := requiredString(object, "latch_node_id", path)
	if issue != nil {
		return nil, issue
	}
	if issue = requireField(object, "continue_predicate", joinPath(path, "continue_predicate"), CodeFieldRequired); issue != nil {
		return nil, issue
	}
	predicate, issue := decodePredicate(object["continue_predicate"], joinPath(path, "continue_predicate"))
	if issue != nil {
		return nil, issue
	}
	return LoopConfig{MaxIterations: iterations, LatchNodeID: latch, ContinuePredicate: predicate}, nil
}

func decodeDeliverConfig(raw json.RawMessage, path string) (NodeConfig, *dtoError) {
	object, issue := configObject(raw, path, "result")
	if issue != nil {
		return nil, issue
	}
	if issue = requireField(object, "result", joinPath(path, "result"), CodeFieldRequired); issue != nil {
		return nil, issue
	}
	result, issue := decodeValueRef(object["result"], joinPath(path, "result"))
	return DeliverConfig{Result: result}, issue
}

func decodeHandoffConfig(raw json.RawMessage, path string) (NodeConfig, *dtoError) {
	object, issue := configObject(raw, path, "agent_id", "agent_version", "instruction", "timeout_seconds")
	if issue != nil {
		return nil, issue
	}
	agentID, issue := requiredString(object, "agent_id", path)
	if issue != nil {
		return nil, issue
	}
	if issue = requireField(object, "agent_version", joinPath(path, "agent_version"), CodeFieldRequired); issue != nil {
		return nil, issue
	}
	version, issue := integerField(object["agent_version"], joinPath(path, "agent_version"))
	if issue != nil {
		return nil, issue
	}
	instruction, issue := requiredString(object, "instruction", path)
	if issue != nil {
		return nil, issue
	}
	result := HandoffConfig{AgentID: agentID, AgentVersion: version, Instruction: instruction}
	if timeout, ok := object["timeout_seconds"]; ok {
		value, valueIssue := integerField(timeout, joinPath(path, "timeout_seconds"))
		if valueIssue != nil {
			return nil, valueIssue
		}
		result.TimeoutSeconds = &value
	}
	return result, nil
}

func decodeInputs(raw json.RawMessage, path string) (map[string]InputBinding, *dtoError) {
	object, issue := objectFields(raw, path)
	if issue != nil {
		return nil, issue
	}
	result := make(map[string]InputBinding, len(object))
	for _, name := range sortedRawKeys(object) {
		bindingRaw := object[name]
		bindingPath := joinPath(path, name)
		bindingFields, fieldIssue := objectFields(bindingRaw, bindingPath)
		if fieldIssue != nil {
			return nil, fieldIssue
		}
		if fieldIssue = rejectUnknown(bindingFields, bindingPath, "expected_type", "value"); fieldIssue != nil {
			return nil, fieldIssue
		}
		for _, required := range []string{"expected_type", "value"} {
			if fieldIssue = requireField(bindingFields, required, joinPath(bindingPath, required), CodeFieldRequired); fieldIssue != nil {
				return nil, fieldIssue
			}
		}
		expected, fieldIssue := decodeValueType(bindingFields["expected_type"], joinPath(bindingPath, "expected_type"))
		if fieldIssue != nil {
			return nil, fieldIssue
		}
		value, fieldIssue := decodeValueRef(bindingFields["value"], joinPath(bindingPath, "value"))
		if fieldIssue != nil {
			return nil, fieldIssue
		}
		result[name] = InputBinding{ExpectedType: expected, Value: value}
	}
	return result, nil
}

func decodeOutputContract(raw json.RawMessage, path string) (OutputContract, *dtoError) {
	var result OutputContract
	object, issue := objectFields(raw, path)
	if issue != nil {
		return result, issue
	}
	if issue = rejectUnknown(object, path, "type", "schema"); issue != nil {
		return result, issue
	}
	if issue = requireField(object, "type", joinPath(path, "type"), CodeFieldRequired); issue != nil {
		return result, issue
	}
	result.Type, issue = decodeValueType(object["type"], joinPath(path, "type"))
	if issue != nil {
		return OutputContract{}, issue
	}
	if schema, ok := object["schema"]; ok {
		if _, issue = objectFields(schema, joinPath(path, "schema")); issue != nil {
			return OutputContract{}, issue
		}
		result.Schema = cloneRaw(schema)
	}
	return result, nil
}

func decodeValueRef(raw json.RawMessage, path string) (ValueRef, *dtoError) {
	var result ValueRef
	object, issue := objectFields(raw, path)
	if issue != nil {
		return result, issue
	}
	if issue = requireField(object, "source", joinPath(path, "source"), CodeDiscriminatorRequired); issue != nil {
		return result, issue
	}
	sourceValue, issue := stringField(object["source"], joinPath(path, "source"))
	if issue != nil {
		return result, issue
	}
	result.Source = ValueSource(sourceValue)
	switch result.Source {
	case ValueRunInput:
		if issue = rejectUnknown(object, path, "source", "path"); issue != nil {
			return ValueRef{}, issue
		}
		if issue = requireField(object, "path", joinPath(path, "path"), CodeFieldRequired); issue != nil {
			return ValueRef{}, issue
		}
	case ValueNodeOutput:
		if issue = rejectUnknown(object, path, "source", "node_id", "path", "iteration", "default"); issue != nil {
			return ValueRef{}, issue
		}
		for _, name := range []string{"node_id", "path"} {
			if issue = requireField(object, name, joinPath(path, name), CodeFieldRequired); issue != nil {
				return ValueRef{}, issue
			}
		}
	case ValueLiteral:
		if issue = rejectUnknown(object, path, "source", "value"); issue != nil {
			return ValueRef{}, issue
		}
		if issue = requireField(object, "value", joinPath(path, "value"), CodeFieldRequired); issue != nil {
			return ValueRef{}, issue
		}
	default:
		return ValueRef{}, newDTOError(joinPath(path, "source"), CodeEnumInvalid, "invalid value source")
	}

	if rawPath, ok := object["path"]; ok {
		if result.Path, issue = stringField(rawPath, joinPath(path, "path")); issue != nil {
			return ValueRef{}, issue
		}
	}
	if rawNodeID, ok := object["node_id"]; ok {
		if result.NodeID, issue = stringField(rawNodeID, joinPath(path, "node_id")); issue != nil {
			return ValueRef{}, issue
		}
	}
	if rawValue, ok := object["value"]; ok {
		result.Value = cloneRaw(rawValue)
	}
	if rawIteration, ok := object["iteration"]; ok {
		iteration, iterationIssue := stringField(rawIteration, joinPath(path, "iteration"))
		if iterationIssue != nil {
			return ValueRef{}, iterationIssue
		}
		result.Iteration = Iteration(iteration)
		if result.Iteration != IterationCurrent && result.Iteration != IterationPrevious {
			return ValueRef{}, newDTOError(joinPath(path, "iteration"), CodeEnumInvalid, "invalid iteration")
		}
	}
	if rawDefault, ok := object["default"]; ok {
		value, defaultIssue := decodeLiteralDefault(rawDefault, joinPath(path, "default"))
		if defaultIssue != nil {
			return ValueRef{}, defaultIssue
		}
		result.Default = &value
	}
	return result, nil
}

func decodeLiteralDefault(raw json.RawMessage, path string) (ValueRef, *dtoError) {
	object, issue := objectFields(raw, path)
	if issue != nil {
		return ValueRef{}, issue
	}
	if issue = requireField(object, "source", joinPath(path, "source"), CodeDiscriminatorRequired); issue != nil {
		return ValueRef{}, issue
	}
	source, issue := stringField(object["source"], joinPath(path, "source"))
	if issue != nil {
		return ValueRef{}, issue
	}
	if ValueSource(source) != ValueLiteral {
		return ValueRef{}, newDTOError(joinPath(path, "source"), CodeEnumInvalid, "default source must be literal")
	}
	if issue = rejectUnknown(object, path, "source", "value"); issue != nil {
		return ValueRef{}, issue
	}
	if issue = requireField(object, "value", joinPath(path, "value"), CodeFieldRequired); issue != nil {
		return ValueRef{}, issue
	}
	return ValueRef{Source: ValueLiteral, Value: cloneRaw(object["value"])}, nil
}

func decodeValueRefMap(raw json.RawMessage, path string) (map[string]ValueRef, *dtoError) {
	object, issue := objectFields(raw, path)
	if issue != nil {
		return nil, issue
	}
	result := make(map[string]ValueRef, len(object))
	for _, name := range sortedRawKeys(object) {
		valueRaw := object[name]
		value, valueIssue := decodeValueRef(valueRaw, joinPath(path, name))
		if valueIssue != nil {
			return nil, valueIssue
		}
		result[name] = value
	}
	return result, nil
}

func decodePredicate(raw json.RawMessage, path string) (Predicate, *dtoError) {
	var result Predicate
	object, issue := objectFields(raw, path)
	if issue != nil {
		return result, issue
	}
	if issue = rejectUnknown(object, path, "left", "operator", "right"); issue != nil {
		return result, issue
	}
	for _, required := range []string{"left", "operator"} {
		if issue = requireField(object, required, joinPath(path, required), CodeFieldRequired); issue != nil {
			return Predicate{}, issue
		}
	}
	if result.Left, issue = decodeValueRef(object["left"], joinPath(path, "left")); issue != nil {
		return Predicate{}, issue
	}
	operatorValue, issue := stringField(object["operator"], joinPath(path, "operator"))
	if issue != nil {
		return Predicate{}, issue
	}
	result.Operator = PredicateOperator(operatorValue)
	if !validPredicateOperator(result.Operator) {
		return Predicate{}, newDTOError(joinPath(path, "operator"), CodeEnumInvalid, "invalid predicate operator")
	}
	if rawRight, ok := object["right"]; ok {
		right, rightIssue := decodeValueRef(rawRight, joinPath(path, "right"))
		if rightIssue != nil {
			return Predicate{}, rightIssue
		}
		result.Right = &right
	}
	return result, nil
}

func decodeEdge(raw json.RawMessage, path string) (Edge, *dtoError) {
	var result Edge
	object, issue := objectFields(raw, path)
	if issue != nil {
		return result, issue
	}
	if issue = rejectUnknown(object, path, "id", "from_node_id", "to_node_id", "route", "priority", "predicate"); issue != nil {
		return result, issue
	}
	for _, name := range []string{"id", "from_node_id", "to_node_id", "route"} {
		if issue = requireField(object, name, joinPath(path, name), CodeFieldRequired); issue != nil {
			return Edge{}, issue
		}
	}
	if result.ID, issue = stringField(object["id"], joinPath(path, "id")); issue != nil {
		return Edge{}, issue
	}
	if result.FromNodeID, issue = stringField(object["from_node_id"], joinPath(path, "from_node_id")); issue != nil {
		return Edge{}, issue
	}
	if result.ToNodeID, issue = stringField(object["to_node_id"], joinPath(path, "to_node_id")); issue != nil {
		return Edge{}, issue
	}
	routeValue, issue := stringField(object["route"], joinPath(path, "route"))
	if issue != nil {
		return Edge{}, issue
	}
	result.Route = EdgeRoute(routeValue)
	if !validEdgeRoute(result.Route) {
		return Edge{}, newDTOError(joinPath(path, "route"), CodeEnumInvalid, "invalid edge route")
	}
	if rawPriority, ok := object["priority"]; ok {
		value, priorityIssue := integerField(rawPriority, joinPath(path, "priority"))
		if priorityIssue != nil {
			return Edge{}, priorityIssue
		}
		result.Priority = &value
	}
	if rawPredicate, ok := object["predicate"]; ok {
		value, predicateIssue := decodePredicate(rawPredicate, joinPath(path, "predicate"))
		if predicateIssue != nil {
			return Edge{}, predicateIssue
		}
		result.Predicate = &value
	}
	return result, nil
}

func configObject(raw json.RawMessage, path string, allowed ...string) (map[string]json.RawMessage, *dtoError) {
	object, issue := objectFields(raw, path)
	if issue != nil {
		return nil, issue
	}
	if issue = rejectUnknown(object, path, allowed...); issue != nil {
		return nil, issue
	}
	return object, nil
}

func objectFields(raw json.RawMessage, path string) (map[string]json.RawMessage, *dtoError) {
	var result map[string]json.RawMessage
	if err := strictDecode(raw, &result); err != nil || result == nil {
		return nil, newDTOError(path, CodeTypeInvalid, "expected object")
	}
	return result, nil
}

func arrayValues(raw json.RawMessage, path string) ([]json.RawMessage, *dtoError) {
	var result []json.RawMessage
	if err := strictDecode(raw, &result); err != nil || result == nil {
		return nil, newDTOError(path, CodeTypeInvalid, "expected array")
	}
	return result, nil
}

func strictDecode(raw json.RawMessage, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func inspectJSON(raw json.RawMessage) *dtoError {
	if !utf8.Valid(raw) {
		return newDTOError("", CodeUnicodeInvalid, "JSON contains invalid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if issue := inspectValue(decoder, raw, "", 0); issue != nil {
		return issue
	}
	var extra any
	err := decoder.Decode(&extra)
	if err == nil {
		return newDTOError("", CodeMultipleJSONValues, "multiple JSON values")
	}
	if !errors.Is(err, io.EOF) {
		return newDTOError("", CodeJSONInvalid, "invalid JSON")
	}
	return nil
}

func inspectValue(decoder *json.Decoder, raw json.RawMessage, path string, depth int) *dtoError {
	if depth > maxJSONNestingDepth {
		return newDTOError(path, CodeNestingLimit, "JSON nesting exceeds the codec limit")
	}
	startOffset := decoder.InputOffset()
	token, err := decoder.Token()
	if err != nil {
		return newDTOError(path, CodeJSONInvalid, "invalid JSON")
	}
	endOffset := decoder.InputOffset()
	if _, ok := token.(string); ok && invalidJSONStringToken(raw[startOffset:endOffset]) {
		return newDTOError(path, CodeUnicodeInvalid, "JSON string contains an invalid Unicode scalar")
	}
	switch value := token.(type) {
	case json.Delim:
		if depth >= maxJSONNestingDepth {
			return newDTOError(path, CodeNestingLimit, "JSON nesting exceeds the codec limit")
		}
		switch value {
		case '{':
			seen := make(map[string]struct{})
			for decoder.More() {
				keyStart := decoder.InputOffset()
				keyToken, keyErr := decoder.Token()
				if keyErr != nil {
					return newDTOError(path, CodeJSONInvalid, "invalid JSON object")
				}
				keyEnd := decoder.InputOffset()
				key, ok := keyToken.(string)
				if !ok {
					return newDTOError(path, CodeJSONInvalid, "invalid JSON object key")
				}
				keyPath := joinPath(path, key)
				if invalidJSONStringToken(raw[keyStart:keyEnd]) {
					return newDTOError(keyPath, CodeUnicodeInvalid, "JSON object key contains an invalid Unicode scalar")
				}
				if _, duplicate := seen[key]; duplicate {
					return newDTOError(keyPath, CodeDuplicateField, "duplicate field")
				}
				seen[key] = struct{}{}
				if issue := inspectValue(decoder, raw, keyPath, depth+1); issue != nil {
					return issue
				}
			}
			if _, err = decoder.Token(); err != nil {
				return newDTOError(path, CodeJSONInvalid, "invalid JSON object")
			}
		case '[':
			index := 0
			for decoder.More() {
				if issue := inspectValue(decoder, raw, fmt.Sprintf("%s/%d", path, index), depth+1); issue != nil {
					return issue
				}
				index++
			}
			if _, err = decoder.Token(); err != nil {
				return newDTOError(path, CodeJSONInvalid, "invalid JSON array")
			}
		default:
			return newDTOError(path, CodeJSONInvalid, "unexpected JSON delimiter")
		}
	case json.Number:
		if !safeJSONInteger(value.String()) {
			return newDTOError(path, CodeUnsafeInteger, "integer is outside the interoperable JSON range")
		}
	}
	return nil
}

func invalidJSONStringToken(segment []byte) bool {
	start := bytes.IndexByte(segment, '"')
	if start < 0 {
		return false
	}
	end := len(segment) - 1
	for end > start && segment[end] != '"' {
		end--
	}
	if end <= start {
		return false
	}
	for index := start + 1; index < end; index++ {
		if segment[index] != '\\' {
			continue
		}
		if index+1 >= end {
			return false
		}
		escape := segment[index+1]
		if escape != 'u' {
			index++
			continue
		}
		if index+6 > end {
			return false
		}
		code, ok := parseHexCodeUnit(segment[index+2 : index+6])
		if !ok {
			return false
		}
		switch {
		case code >= 0xD800 && code <= 0xDBFF:
			if index+12 > end || segment[index+6] != '\\' || segment[index+7] != 'u' {
				return true
			}
			low, lowOK := parseHexCodeUnit(segment[index+8 : index+12])
			if !lowOK || low < 0xDC00 || low > 0xDFFF {
				return true
			}
			index += 11
		case code >= 0xDC00 && code <= 0xDFFF:
			return true
		default:
			index += 5
		}
	}
	return false
}

func parseHexCodeUnit(value []byte) (uint16, bool) {
	if len(value) != 4 {
		return 0, false
	}
	var result uint16
	for _, character := range value {
		result <<= 4
		switch {
		case character >= '0' && character <= '9':
			result |= uint16(character - '0')
		case character >= 'a' && character <= 'f':
			result |= uint16(character-'a') + 10
		case character >= 'A' && character <= 'F':
			result |= uint16(character-'A') + 10
		default:
			return 0, false
		}
	}
	return result, true
}

func safeJSONInteger(value string) bool {
	digits, fractionDigits, exponentNegative, exponentMagnitude, ok := splitJSONNumber(value)
	if !ok {
		return false
	}

	firstNonZero := 0
	for firstNonZero < len(digits) && digits[firstNonZero] == '0' {
		firstNonZero++
	}
	if firstNonZero == len(digits) {
		return true
	}
	digits = digits[firstNonZero:]

	trailingZeros := 0
	for index := len(digits) - 1; index >= 0 && digits[index] == '0'; index-- {
		trailingZeros++
	}

	var decimalShift int
	if exponentNegative {
		negativeShift := fractionDigits + exponentMagnitude
		if negativeShift > trailingZeros {
			return true
		}
		digits = digits[:len(digits)-negativeShift]
	} else if exponentMagnitude < fractionDigits {
		negativeShift := fractionDigits - exponentMagnitude
		if negativeShift > trailingZeros {
			return true
		}
		digits = digits[:len(digits)-negativeShift]
	} else {
		decimalShift = exponentMagnitude - fractionDigits
	}

	totalDigits := len(digits) + decimalShift
	if totalDigits != len(maxSafeIntegerText) {
		return totalDigits < len(maxSafeIntegerText)
	}
	for index := range len(maxSafeIntegerText) {
		digit := byte('0')
		if index < len(digits) {
			digit = digits[index]
		}
		if digit != maxSafeIntegerText[index] {
			return digit < maxSafeIntegerText[index]
		}
	}
	return true
}

func splitJSONNumber(value string) (digits []byte, fractionDigits int, exponentNegative bool, exponentMagnitude int, ok bool) {
	if value == "" {
		return nil, 0, false, 0, false
	}
	index := 0
	if value[index] == '-' {
		index++
		if index == len(value) {
			return nil, 0, false, 0, false
		}
	}

	digits = make([]byte, 0, len(value))
	switch {
	case value[index] == '0':
		digits = append(digits, '0')
		index++
		if index < len(value) && value[index] >= '0' && value[index] <= '9' {
			return nil, 0, false, 0, false
		}
	case value[index] >= '1' && value[index] <= '9':
		for index < len(value) && value[index] >= '0' && value[index] <= '9' {
			digits = append(digits, value[index])
			index++
		}
	default:
		return nil, 0, false, 0, false
	}

	if index < len(value) && value[index] == '.' {
		index++
		fractionStart := index
		for index < len(value) && value[index] >= '0' && value[index] <= '9' {
			digits = append(digits, value[index])
			index++
		}
		fractionDigits = index - fractionStart
		if fractionDigits == 0 {
			return nil, 0, false, 0, false
		}
	}

	if index == len(value) {
		return digits, fractionDigits, false, 0, true
	}
	if value[index] != 'e' && value[index] != 'E' {
		return nil, 0, false, 0, false
	}
	index++
	if index == len(value) {
		return nil, 0, false, 0, false
	}
	if value[index] == '+' || value[index] == '-' {
		exponentNegative = value[index] == '-'
		index++
	}
	if index == len(value) {
		return nil, 0, false, 0, false
	}

	// Exact magnitudes beyond the input's digit count cannot change the
	// classification, so saturate instead of constructing an enormous power.
	exponentLimit := len(value) + len(maxSafeIntegerText)
	for ; index < len(value); index++ {
		character := value[index]
		if character < '0' || character > '9' {
			return nil, 0, false, 0, false
		}
		if exponentMagnitude <= exponentLimit {
			exponentMagnitude = exponentMagnitude*10 + int(character-'0')
			if exponentMagnitude > exponentLimit {
				exponentMagnitude = exponentLimit + 1
			}
		}
	}
	return digits, fractionDigits, exponentNegative, exponentMagnitude, true
}

func bestEffortNodeIDForPath(raw json.RawMessage, path string) string {
	const prefix = "/nodes/"
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	remainder := strings.TrimPrefix(path, prefix)
	end := strings.IndexByte(remainder, '/')
	if end < 0 {
		return ""
	}
	index, err := strconv.Atoi(remainder[:end])
	if err != nil || index < 0 {
		return ""
	}

	nodesRaw, ok := uniqueTopLevelRawField(raw, "nodes")
	if !ok {
		return ""
	}
	nodes, issue := arrayValues(nodesRaw, "/nodes")
	if issue != nil || index >= len(nodes) {
		return ""
	}
	return bestEffortNodeID(nodes[index])
}

func uniqueTopLevelRawField(raw json.RawMessage, fieldName string) (json.RawMessage, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, false
	}

	var result json.RawMessage
	count := 0
	for decoder.More() {
		keyToken, keyErr := decoder.Token()
		if keyErr != nil {
			return nil, false
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, false
		}
		var value json.RawMessage
		if err = decoder.Decode(&value); err != nil {
			return nil, false
		}
		if key == fieldName {
			count++
			if count == 1 {
				result = cloneRaw(value)
			}
		}
	}
	if _, err = decoder.Token(); err != nil || count != 1 {
		return nil, false
	}
	var extra any
	if err = decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return result, true
}

func bestEffortNodeID(raw json.RawMessage) string {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return ""
	}

	var idRaw json.RawMessage
	idCount := 0
	for decoder.More() {
		key, keyErr := decoder.Token()
		if keyErr != nil {
			return ""
		}
		var field json.RawMessage
		if err = decoder.Decode(&field); err != nil {
			return ""
		}
		if key == "id" {
			idCount++
			idRaw = field
		}
	}
	if _, err = decoder.Token(); err != nil || idCount != 1 {
		return ""
	}
	if issue := inspectJSON(idRaw); issue != nil {
		return ""
	}
	id, issue := stringField(idRaw, "")
	if issue != nil || id == "" {
		return ""
	}
	return id
}

func requiredString(object map[string]json.RawMessage, name, path string) (string, *dtoError) {
	fieldPath := joinPath(path, name)
	if issue := requireField(object, name, fieldPath, CodeFieldRequired); issue != nil {
		return "", issue
	}
	return stringField(object[name], fieldPath)
}

func stringField(raw json.RawMessage, path string) (string, *dtoError) {
	var value string
	if err := strictDecode(raw, &value); err != nil {
		return "", newDTOError(path, CodeTypeInvalid, "expected string")
	}
	return value, nil
}

func integerField(raw json.RawMessage, path string) (int64, *dtoError) {
	var value int64
	if err := strictDecode(raw, &value); err != nil {
		return 0, newDTOError(path, CodeTypeInvalid, "expected integer")
	}
	return value, nil
}

func decodeValueType(raw json.RawMessage, path string) (ValueType, *dtoError) {
	value, issue := stringField(raw, path)
	if issue != nil {
		return "", issue
	}
	result := ValueType(value)
	if result != ValueText && result != ValueJSON && result != ValueBoolean && result != ValueNumber {
		return "", newDTOError(path, CodeEnumInvalid, "invalid value type")
	}
	return result, nil
}

func requireField(object map[string]json.RawMessage, name, path, code string) *dtoError {
	if _, ok := object[name]; !ok {
		return newDTOError(path, code, "required field is missing")
	}
	return nil
}

func rejectUnknown(object map[string]json.RawMessage, path string, allowed ...string) *dtoError {
	known := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		known[name] = struct{}{}
	}
	var unknown string
	for name := range object {
		if _, ok := known[name]; !ok && (unknown == "" || name < unknown) {
			unknown = name
		}
	}
	if unknown == "" {
		return nil
	}
	return newDTOError(joinPath(path, unknown), CodeUnknownField, "unknown field")
}

func sortedRawKeys(object map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(object))
	for name := range object {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	return keys
}

func reportFor(issue *dtoError) *Report {
	if issue == nil {
		return nil
	}
	report := &Report{}
	report.AddNode(PhaseDTO, issue.path, issue.nodeID, issue.code, issue.Error())
	return report
}

func newDTOError(path, code, message string) *dtoError {
	return &dtoError{path: path, code: code, err: errors.New(message)}
}

func joinPath(parent, child string) string {
	escaped := strings.ReplaceAll(strings.ReplaceAll(child, "~", "~0"), "/", "~1")
	if parent == "" {
		return "/" + escaped
	}
	return parent + "/" + escaped
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), raw...)
}

func validTriggerType(value TriggerType) bool {
	switch value {
	case TriggerConversationExplicit, TriggerConversationAuto, TriggerSchedule, TriggerAPI, TriggerEvent:
		return true
	default:
		return false
	}
}

func validNodeType(value NodeType) bool {
	switch value {
	case NodeLead, NodeWorker, NodeTransform, NodeCondition, NodeParallel, NodeJoin, NodeWait, NodeLoop, NodeDeliver, NodeHandoff:
		return true
	default:
		return false
	}
}

func validPredicateOperator(value PredicateOperator) bool {
	switch value {
	case OperatorExists, OperatorEQ, OperatorNEQ, OperatorGT, OperatorGTE, OperatorLT, OperatorLTE, OperatorContains, OperatorIn:
		return true
	default:
		return false
	}
}

func validEdgeRoute(value EdgeRoute) bool {
	switch value {
	case RouteSuccess, RouteFailure, RouteCase, RouteDefault, RouteBranch, RouteJoin, RouteTimeout, RouteBody, RouteExit, RouteBack:
		return true
	default:
		return false
	}
}
