package teamforge

// Strict typed decoding helpers for the team-workflow write tools (plan
// §10.2.3). The tools build the typed machine model from JSON parameters
// with the same field whitelists and tagged-union rules the strict platform
// decoders enforce (internal/workflow/machine/decode.go), so the model can
// never smuggle raw graph JSON past the tool layer: tool output is the only
// encoding that ever reaches the workflow store, and tf_wf_commit re-runs
// machine.DecodeGraphDefinitionV1 / DecodeTriggerConfigV1 over it.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

// strictRaw decodes exactly one JSON value into dst, rejecting unknown
// fields and trailing values.
func strictRaw(raw json.RawMessage, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
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

// decodeValueRefStrict decodes one ValueRef with the machine's tagged-union
// rules: iteration and default are node_output-only modifiers, defaults must
// be literal and non-recursive, and run_input/literal forbid them.
func decodeValueRefStrict(raw json.RawMessage) (machine.ValueRef, error) {
	fields, err := decodeRawFields(raw)
	if err != nil {
		return machine.ValueRef{}, fmt.Errorf("value ref: %w", err)
	}
	var ref machine.ValueRef
	if err := strictRaw(raw, &ref); err != nil {
		return machine.ValueRef{}, fmt.Errorf("value ref: %w", err)
	}
	if err := requireJSONField(fields, "source", "value ref"); err != nil {
		return machine.ValueRef{}, err
	}
	source := strings.TrimSpace(string(ref.Source))
	switch machine.ValueSource(source) {
	case machine.ValueRunInput:
		if _, hasDefault := fields["default"]; hasDefault {
			return machine.ValueRef{}, errors.New("default is only allowed on node_output value refs")
		}
		if err := rejectJSONFields(fields, "source", "path"); err != nil {
			return machine.ValueRef{}, err
		}
		if err := requireJSONField(fields, "path", "run_input value ref"); err != nil {
			return machine.ValueRef{}, err
		}
	case machine.ValueNodeOutput:
		if err := rejectJSONFields(fields, "source", "node_id", "path", "iteration", "default"); err != nil {
			return machine.ValueRef{}, err
		}
		for _, name := range []string{"node_id", "path"} {
			if err := requireJSONField(fields, name, "node_output value ref"); err != nil {
				return machine.ValueRef{}, err
			}
		}
		if ref.Iteration != "" && ref.Iteration != machine.IterationCurrent &&
			ref.Iteration != machine.IterationPrevious {
			return machine.ValueRef{}, fmt.Errorf("node_output value ref iteration must be %q or %q",
				machine.IterationCurrent, machine.IterationPrevious)
		}
	case machine.ValueLiteral:
		if _, hasDefault := fields["default"]; hasDefault {
			return machine.ValueRef{}, errors.New("default is only allowed on node_output value refs")
		}
		if err := rejectJSONFields(fields, "source", "value"); err != nil {
			return machine.ValueRef{}, err
		}
		if err := requireJSONField(fields, "value", "literal value ref"); err != nil {
			return machine.ValueRef{}, err
		}
	default:
		return machine.ValueRef{}, fmt.Errorf("value ref source must be run_input, node_output, or literal (got %q)", source)
	}
	if rawDefault, ok := fields["default"]; ok {
		if ref.Source != machine.ValueNodeOutput {
			return machine.ValueRef{}, fmt.Errorf("default is only allowed on node_output value refs")
		}
		literal, err := decodeLiteralDefaultStrict(rawDefault)
		if err != nil {
			return machine.ValueRef{}, err
		}
		ref.Default = &literal
	}
	if ref.Value != nil && len(ref.Value) == 0 {
		ref.Value = nil
	}
	return ref, nil
}

func decodeLiteralDefaultStrict(raw json.RawMessage) (machine.ValueRef, error) {
	fields, err := decodeRawFields(raw)
	if err != nil {
		return machine.ValueRef{}, fmt.Errorf("value ref default: %w", err)
	}
	if _, nested := fields["default"]; nested {
		return machine.ValueRef{}, errors.New("value ref default must not be recursive")
	}
	if err := requireJSONField(fields, "source", "value ref default"); err != nil {
		return machine.ValueRef{}, err
	}
	var source machine.ValueSource
	if err := json.Unmarshal(fields["source"], &source); err != nil {
		return machine.ValueRef{}, errors.New("value ref default source must be a string")
	}
	if source != machine.ValueLiteral {
		return machine.ValueRef{}, errors.New("value ref default must be a literal")
	}
	if err := rejectJSONFields(fields, "source", "value"); err != nil {
		return machine.ValueRef{}, err
	}
	if err := requireJSONField(fields, "value", "value ref default"); err != nil {
		return machine.ValueRef{}, err
	}
	var literal machine.ValueRef
	if err := strictRaw(raw, &literal); err != nil {
		return machine.ValueRef{}, fmt.Errorf("value ref default: %w", err)
	}
	return literal, nil
}

// decodePredicateStrict decodes one condition case or loop continue
// predicate with the machine's field whitelist.
func decodePredicateStrict(raw json.RawMessage) (machine.Predicate, error) {
	fields, err := decodeRawFields(raw)
	if err != nil {
		return machine.Predicate{}, fmt.Errorf("predicate: %w", err)
	}
	if err := rejectJSONFields(fields, "left", "operator", "right"); err != nil {
		return machine.Predicate{}, err
	}
	for _, name := range []string{"left", "operator"} {
		if err := requireJSONField(fields, name, "predicate"); err != nil {
			return machine.Predicate{}, err
		}
	}
	var predicate machine.Predicate
	if err := strictRaw(raw, &predicate); err != nil {
		return machine.Predicate{}, fmt.Errorf("predicate: %w", err)
	}
	left, err := decodeValueRefStrict(fields["left"])
	if err != nil {
		return machine.Predicate{}, err
	}
	predicate.Left = left
	if rawRight, ok := fields["right"]; ok {
		right, err := decodeValueRefStrict(rawRight)
		if err != nil {
			return machine.Predicate{}, err
		}
		predicate.Right = &right
	}
	switch predicate.Operator {
	case machine.OperatorExists, machine.OperatorEQ, machine.OperatorNEQ,
		machine.OperatorGT, machine.OperatorGTE, machine.OperatorLT,
		machine.OperatorLTE, machine.OperatorContains, machine.OperatorIn:
	default:
		return machine.Predicate{}, fmt.Errorf("predicate operator must be one of exists, eq, neq, gt, gte, lt, lte, contains, in (got %q)", predicate.Operator)
	}
	return predicate, nil
}

// decodeOutputContractStrict decodes one contract object {type, schema?}
// with the machine's value-type vocabulary. Schema validity is owned by the
// validator phase; the tool only requires an object when one is present.
func decodeOutputContractStrict(raw json.RawMessage) (machine.OutputContract, error) {
	fields, err := decodeRawFields(raw)
	if err != nil {
		return machine.OutputContract{}, fmt.Errorf("contract: %w", err)
	}
	if err := rejectJSONFields(fields, "type", "schema"); err != nil {
		return machine.OutputContract{}, err
	}
	if err := requireJSONField(fields, "type", "contract"); err != nil {
		return machine.OutputContract{}, err
	}
	var contract machine.OutputContract
	if err := strictRaw(raw, &contract); err != nil {
		return machine.OutputContract{}, fmt.Errorf("contract: %w", err)
	}
	switch contract.Type {
	case machine.ValueText, machine.ValueJSON, machine.ValueBoolean, machine.ValueNumber:
	default:
		return machine.OutputContract{}, fmt.Errorf("contract type must be text, json, boolean, or number (got %q)", contract.Type)
	}
	if rawSchema, ok := fields["schema"]; ok {
		var probe map[string]json.RawMessage
		if err := strictRaw(rawSchema, &probe); err != nil {
			return machine.OutputContract{}, errors.New("contract schema must be a JSON object")
		}
		contract.Schema = append(json.RawMessage(nil), rawSchema...)
	}
	return contract, nil
}

// decodeNodeConfigStrict decodes one node config against the machine node
// vocabulary, enforcing per-type required fields, enums, and the ValueRef
// tagged-union rules embedded in transform/deliver/loop configs.
func decodeNodeConfigStrict(nodeType machine.NodeType, raw json.RawMessage) (machine.NodeConfig, error) {
	switch nodeType {
	case machine.NodeLead:
		var config machine.LeadConfig
		if err := requireOnlyFields(raw, "instruction"); err != nil {
			return nil, err
		}
		if err := strictRaw(raw, &config); err != nil {
			return nil, fmt.Errorf("lead config: %w", err)
		}
		if strings.TrimSpace(config.Instruction) == "" {
			return nil, errors.New("lead config requires a non-empty instruction")
		}
		return config, nil
	case machine.NodeWorker:
		var config machine.WorkerConfig
		if err := requireOnlyFields(raw, "agent_id", "agent_version", "kind", "result_requirement"); err != nil {
			return nil, err
		}
		if err := strictRaw(raw, &config); err != nil {
			return nil, fmt.Errorf("worker config: %w", err)
		}
		if strings.TrimSpace(config.AgentID) == "" || config.AgentVersion == 0 ||
			strings.TrimSpace(config.ResultRequirement) == "" {
			return nil, errors.New("worker config requires agent_id, a positive agent_version, and result_requirement")
		}
		if config.Kind != machine.WorkerConsult && config.Kind != machine.WorkerDispatch {
			return nil, fmt.Errorf("worker config kind must be consult or dispatch (got %q)", config.Kind)
		}
		return config, nil
	case machine.NodeTransform:
		return decodeTransformConfigStrict(raw)
	case machine.NodeCondition:
		var config machine.ConditionConfig
		if err := requireOnlyFields(raw); err != nil {
			return nil, err
		}
		if len(bytes.TrimSpace(raw)) != 0 && string(bytes.TrimSpace(raw)) != "{}" {
			return nil, errors.New("condition config must be an empty object")
		}
		return config, nil
	case machine.NodeParallel:
		var config machine.ParallelConfig
		if err := requireOnlyFields(raw, "join_node_id"); err != nil {
			return nil, err
		}
		if err := strictRaw(raw, &config); err != nil {
			return nil, fmt.Errorf("parallel config: %w", err)
		}
		if strings.TrimSpace(config.JoinNodeID) == "" {
			return nil, errors.New("parallel config requires a non-empty join_node_id")
		}
		return config, nil
	case machine.NodeJoin:
		return decodeJoinConfigStrict(raw)
	case machine.NodeWait:
		fields, err := decodeRawFields(raw)
		if err != nil {
			return nil, fmt.Errorf("wait config: %w", err)
		}
		if err := rejectJSONFields(fields, "kind", "resume_schema", "timeout_seconds", "task"); err != nil {
			return nil, err
		}
		if err := requireJSONField(fields, "resume_schema", "wait config"); err != nil {
			return nil, err
		}
		var probe map[string]json.RawMessage
		if err := strictRaw(fields["resume_schema"], &probe); err != nil {
			return nil, errors.New("wait config resume_schema must be a JSON object")
		}
		var config machine.WaitConfig
		if err := strictRaw(raw, &config); err != nil {
			return nil, fmt.Errorf("wait config: %w", err)
		}
		config.ResumeSchema = append(json.RawMessage(nil), fields["resume_schema"]...)
		return config, nil
	case machine.NodeLoop:
		fields, err := decodeRawFields(raw)
		if err != nil {
			return nil, fmt.Errorf("loop config: %w", err)
		}
		if err := rejectJSONFields(fields, "max_iterations", "latch_node_id", "continue_predicate"); err != nil {
			return nil, err
		}
		if err := requireJSONField(fields, "max_iterations", "loop config"); err != nil {
			return nil, err
		}
		if err := requireJSONField(fields, "latch_node_id", "loop config"); err != nil {
			return nil, err
		}
		if err := requireJSONField(fields, "continue_predicate", "loop config"); err != nil {
			return nil, err
		}
		predicate, err := decodePredicateStrict(fields["continue_predicate"])
		if err != nil {
			return nil, err
		}
		var config machine.LoopConfig
		if err := strictRaw(raw, &config); err != nil {
			return nil, fmt.Errorf("loop config: %w", err)
		}
		if config.MaxIterations <= 0 {
			return nil, errors.New("loop config max_iterations must be positive")
		}
		if strings.TrimSpace(config.LatchNodeID) == "" {
			return nil, errors.New("loop config requires a non-empty latch_node_id")
		}
		config.ContinuePredicate = predicate
		return config, nil
	case machine.NodeDeliver:
		fields, err := decodeRawFields(raw)
		if err != nil {
			return nil, fmt.Errorf("deliver config: %w", err)
		}
		if err := rejectJSONFields(fields, "result"); err != nil {
			return nil, err
		}
		if err := requireJSONField(fields, "result", "deliver config"); err != nil {
			return nil, err
		}
		result, err := decodeValueRefStrict(fields["result"])
		if err != nil {
			return nil, err
		}
		var config machine.DeliverConfig
		if err := strictRaw(raw, &config); err != nil {
			return nil, fmt.Errorf("deliver config: %w", err)
		}
		config.Result = result
		return config, nil
	case machine.NodeHandoff:
		var config machine.HandoffConfig
		if err := requireOnlyFields(raw, "agent_id", "agent_version", "instruction", "timeout_seconds"); err != nil {
			return nil, err
		}
		if err := strictRaw(raw, &config); err != nil {
			return nil, fmt.Errorf("handoff config: %w", err)
		}
		if strings.TrimSpace(config.AgentID) == "" || config.AgentVersion == 0 ||
			strings.TrimSpace(config.Instruction) == "" {
			return nil, errors.New("handoff config requires agent_id, a positive agent_version, and instruction")
		}
		return config, nil
	default:
		return nil, fmt.Errorf("unknown node type %q", nodeType)
	}
}

func decodeTransformConfigStrict(raw json.RawMessage) (machine.NodeConfig, error) {
	fields, err := decodeRawFields(raw)
	if err != nil {
		return nil, fmt.Errorf("transform config: %w", err)
	}
	if err := rejectJSONFields(fields, "operation", "value", "fields", "items"); err != nil {
		return nil, err
	}
	if err := requireJSONField(fields, "operation", "transform config"); err != nil {
		return nil, err
	}
	var operation machine.TransformOperation
	if err := json.Unmarshal(fields["operation"], &operation); err != nil {
		return nil, errors.New("transform config operation must be a string")
	}
	var config machine.TransformConfig
	if err := strictRaw(raw, &config); err != nil {
		return nil, fmt.Errorf("transform config: %w", err)
	}
	switch operation {
	case machine.TransformIdentity:
		if err := rejectJSONFields(fields, "operation", "value"); err != nil {
			return nil, err
		}
		if err := requireJSONField(fields, "value", "identity transform"); err != nil {
			return nil, err
		}
		value, err := decodeValueRefStrict(fields["value"])
		if err != nil {
			return nil, err
		}
		config.Operation = operation
		config.Value = &value
		return config, nil
	case machine.TransformObject:
		if err := rejectJSONFields(fields, "operation", "fields"); err != nil {
			return nil, err
		}
		if err := requireJSONField(fields, "fields", "object transform"); err != nil {
			return nil, err
		}
		var fieldMap map[string]json.RawMessage
		if err := strictRaw(fields["fields"], &fieldMap); err != nil {
			return nil, errors.New("object transform fields must be a JSON object of value refs")
		}
		config.Operation = operation
		config.Fields = make(map[string]machine.ValueRef, len(fieldMap))
		for name, rawRef := range fieldMap {
			ref, err := decodeValueRefStrict(rawRef)
			if err != nil {
				return nil, fmt.Errorf("object transform field %q: %w", name, err)
			}
			config.Fields[name] = ref
		}
		return config, nil
	case machine.TransformArray:
		if err := rejectJSONFields(fields, "operation", "items"); err != nil {
			return nil, err
		}
		if err := requireJSONField(fields, "items", "array transform"); err != nil {
			return nil, err
		}
		var items []json.RawMessage
		if err := strictRaw(fields["items"], &items); err != nil {
			return nil, errors.New("array transform items must be a JSON array of value refs")
		}
		if len(items) == 0 {
			return nil, errors.New("array transform items must be non-empty")
		}
		config.Operation = operation
		config.Items = make([]machine.ValueRef, len(items))
		for i, rawRef := range items {
			ref, err := decodeValueRefStrict(rawRef)
			if err != nil {
				return nil, fmt.Errorf("array transform item %d: %w", i, err)
			}
			config.Items[i] = ref
		}
		return config, nil
	default:
		return nil, fmt.Errorf("transform config operation must be identity, object, or array (got %q)", operation)
	}
}

func decodeJoinConfigStrict(raw json.RawMessage) (machine.NodeConfig, error) {
	fields, err := decodeRawFields(raw)
	if err != nil {
		return nil, fmt.Errorf("join config: %w", err)
	}
	if err := rejectJSONFields(fields, "policy", "success_count", "deadline_seconds"); err != nil {
		return nil, err
	}
	if err := requireJSONField(fields, "policy", "join config"); err != nil {
		return nil, err
	}
	var config machine.JoinConfig
	if err := strictRaw(raw, &config); err != nil {
		return nil, fmt.Errorf("join config: %w", err)
	}
	switch config.Policy {
	case machine.JoinAllSuccess, machine.JoinFailFast:
		if err := rejectJSONFields(fields, "policy"); err != nil {
			return nil, err
		}
	case machine.JoinQuorum:
		if err := rejectJSONFields(fields, "policy", "success_count"); err != nil {
			return nil, err
		}
		if config.SuccessCount == nil || *config.SuccessCount <= 0 {
			return nil, errors.New("quorum join requires a positive success_count")
		}
	case machine.JoinDeadline:
		if err := rejectJSONFields(fields, "policy", "deadline_seconds"); err != nil {
			return nil, err
		}
		if config.DeadlineSeconds == nil || *config.DeadlineSeconds <= 0 {
			return nil, errors.New("deadline join requires a positive deadline_seconds")
		}
	default:
		return nil, fmt.Errorf("join config policy must be all_success, quorum, deadline, or fail_fast (got %q)", config.Policy)
	}
	return config, nil
}

// decodeTriggerStrict decodes one trigger object with the machine's session/
// delivery rules: session triggers forbid delivery, non-session triggers
// require it.
func decodeTriggerStrict(raw json.RawMessage) (machine.TriggerConfig, error) {
	fields, err := decodeRawFields(raw)
	if err != nil {
		return machine.TriggerConfig{}, fmt.Errorf("trigger: %w", err)
	}
	if err := rejectJSONFields(fields, "schema_version", "type", "config", "delivery"); err != nil {
		return machine.TriggerConfig{}, err
	}
	if err := requireJSONField(fields, "schema_version", "trigger"); err != nil {
		return machine.TriggerConfig{}, err
	}
	if err := requireJSONField(fields, "type", "trigger"); err != nil {
		return machine.TriggerConfig{}, err
	}
	if err := requireJSONField(fields, "config", "trigger"); err != nil {
		return machine.TriggerConfig{}, err
	}
	var schemaVersion int
	if err := json.Unmarshal(fields["schema_version"], &schemaVersion); err != nil {
		return machine.TriggerConfig{}, errors.New("trigger schema_version must be an integer")
	}
	if schemaVersion != machine.SchemaVersionV1 {
		return machine.TriggerConfig{}, fmt.Errorf("trigger schema_version must be %d", machine.SchemaVersionV1)
	}
	var trigger machine.TriggerConfig
	trigger.SchemaVersion = schemaVersion
	var typeValue string
	if err := json.Unmarshal(fields["type"], &typeValue); err != nil {
		return machine.TriggerConfig{}, errors.New("trigger type must be a string")
	}
	trigger.Type = machine.TriggerType(typeValue)

	configRaw := fields["config"]
	switch trigger.Type {
	case machine.TriggerConversationExplicit:
		if err := requireOnlyFields(configRaw); err != nil {
			return machine.TriggerConfig{}, fmt.Errorf("conversation_explicit trigger: %w", err)
		}
		if len(bytes.TrimSpace(configRaw)) != 0 && string(bytes.TrimSpace(configRaw)) != "{}" {
			return machine.TriggerConfig{}, errors.New("conversation_explicit trigger config must be an empty object")
		}
		trigger.Config = machine.ConversationExplicitConfig{}
	case machine.TriggerConversationAuto:
		var config machine.ConversationAutoConfig
		if err := requireOnlyFields(configRaw, "catalog_key"); err != nil {
			return machine.TriggerConfig{}, fmt.Errorf("conversation_auto trigger: %w", err)
		}
		if err := strictRaw(configRaw, &config); err != nil {
			return machine.TriggerConfig{}, fmt.Errorf("conversation_auto trigger: %w", err)
		}
		if strings.TrimSpace(config.CatalogKey) == "" {
			return machine.TriggerConfig{}, errors.New("conversation_auto trigger requires a non-empty catalog_key")
		}
		trigger.Config = config
	case machine.TriggerSchedule:
		var config machine.ScheduleConfig
		if err := requireOnlyFields(configRaw, "schedule_id"); err != nil {
			return machine.TriggerConfig{}, fmt.Errorf("schedule trigger: %w", err)
		}
		if err := strictRaw(configRaw, &config); err != nil {
			return machine.TriggerConfig{}, fmt.Errorf("schedule trigger: %w", err)
		}
		if strings.TrimSpace(config.ScheduleID) == "" {
			return machine.TriggerConfig{}, errors.New("schedule trigger requires a non-empty schedule_id")
		}
		trigger.Config = config
	case machine.TriggerAPI:
		var config machine.APIConfig
		if err := requireOnlyFields(configRaw, "endpoint_key"); err != nil {
			return machine.TriggerConfig{}, fmt.Errorf("api trigger: %w", err)
		}
		if err := strictRaw(configRaw, &config); err != nil {
			return machine.TriggerConfig{}, fmt.Errorf("api trigger: %w", err)
		}
		if strings.TrimSpace(config.EndpointKey) == "" {
			return machine.TriggerConfig{}, errors.New("api trigger requires a non-empty endpoint_key")
		}
		trigger.Config = config
	case machine.TriggerEvent:
		var config machine.EventConfig
		if err := requireOnlyFields(configRaw, "event_type"); err != nil {
			return machine.TriggerConfig{}, fmt.Errorf("event trigger: %w", err)
		}
		if err := strictRaw(configRaw, &config); err != nil {
			return machine.TriggerConfig{}, fmt.Errorf("event trigger: %w", err)
		}
		if strings.TrimSpace(config.EventType) == "" {
			return machine.TriggerConfig{}, errors.New("event trigger requires a non-empty event_type")
		}
		trigger.Config = config
	default:
		return machine.TriggerConfig{}, fmt.Errorf(
			"trigger type must be conversation_explicit, conversation_auto, schedule, api, or event (got %q)",
			trigger.Type)
	}

	if rawDelivery, ok := fields["delivery"]; ok {
		if trigger.Type == machine.TriggerConversationExplicit || trigger.Type == machine.TriggerConversationAuto {
			return machine.TriggerConfig{}, errors.New("delivery is forbidden for session triggers")
		}
		delivery, err := decodeDeliveryStrict(rawDelivery)
		if err != nil {
			return machine.TriggerConfig{}, err
		}
		trigger.Delivery = &delivery
	} else if trigger.Type != machine.TriggerConversationExplicit &&
		trigger.Type != machine.TriggerConversationAuto {
		return machine.TriggerConfig{}, errors.New("delivery is required for non-session triggers")
	}
	return trigger, nil
}

func decodeDeliveryStrict(raw json.RawMessage) (machine.Delivery, error) {
	fields, err := decodeRawFields(raw)
	if err != nil {
		return machine.Delivery{}, fmt.Errorf("delivery: %w", err)
	}
	if err := rejectJSONFields(fields, "kind", "ref"); err != nil {
		return machine.Delivery{}, err
	}
	if err := requireJSONField(fields, "kind", "delivery"); err != nil {
		return machine.Delivery{}, err
	}
	var delivery machine.Delivery
	if err := strictRaw(raw, &delivery); err != nil {
		return machine.Delivery{}, fmt.Errorf("delivery: %w", err)
	}
	switch delivery.Kind {
	case machine.DeliveryJobRecord:
		if _, ok := fields["ref"]; ok {
			return machine.Delivery{}, errors.New("ref is not allowed for job_record delivery")
		}
	case machine.DeliveryCallbackRef, machine.DeliveryTargetRef:
		if err := requireJSONField(fields, "ref", "delivery"); err != nil {
			return machine.Delivery{}, err
		}
		if strings.TrimSpace(delivery.Ref) == "" {
			return machine.Delivery{}, errors.New("delivery ref must be non-empty")
		}
	default:
		return machine.Delivery{}, fmt.Errorf(
			"delivery kind must be job_record, callback_ref, or target_ref (got %q)",
			delivery.Kind)
	}
	return delivery, nil
}

// --- small field helpers ---

func requireOnlyFields(raw json.RawMessage, allowed ...string) error {
	fields, err := decodeRawFields(raw)
	if err != nil {
		return err
	}
	return rejectJSONFields(fields, allowed...)
}

func decodeRawFields(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := strictRaw(raw, &fields); err != nil {
		return nil, err
	}
	if fields == nil {
		return nil, errors.New("expected object")
	}
	return fields, nil
}

func rejectJSONFields(fields map[string]json.RawMessage, allowed ...string) error {
	known := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		known[name] = struct{}{}
	}
	for name := range fields {
		if _, ok := known[name]; !ok {
			return fmt.Errorf("unknown field %q", name)
		}
	}
	return nil
}

func requireJSONField(fields map[string]json.RawMessage, name, owner string) error {
	if _, ok := fields[name]; !ok {
		return fmt.Errorf("%s requires field %q", owner, name)
	}
	return nil
}
