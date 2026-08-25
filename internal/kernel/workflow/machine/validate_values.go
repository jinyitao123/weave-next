package machine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type valueValidationStats struct {
	ValueRefsVisited int
	PointerTokens    int
	DominatorQueries int
	LoopOwnerLookups int
}

type valueValidator struct {
	graph       GraphDefinition
	analysis    controlFlowAnalysis
	loops       naturalLoopFacts
	nodeOutputs []parsedOutputContract
	nodeHasOut  []bool
	runInput    parsedOutputContract
	workflowOut parsedOutputContract
	report      *Report
	stats       *valueValidationStats
}

type valueUse struct {
	ownerNodeID  string
	consumer     int
	allowMissing bool
	latchSelf    bool
}

func validateValuesAndSchemas(ctx ValidationContext, stats *valueValidationStats) Report {
	report := validateDeclaredSchemas(ctx)
	validator := newValueValidator(ctx.Graph, &report, stats)
	validator.validate()
	return report
}

func newValueValidator(graph GraphDefinition, report *Report, stats *valueValidationStats) *valueValidator {
	analysis := buildControlFlowAnalysis(graph)
	validator := &valueValidator{
		graph:       graph,
		analysis:    analysis,
		loops:       buildNaturalLoopFacts(graph, analysis),
		nodeOutputs: make([]parsedOutputContract, len(graph.Nodes)),
		nodeHasOut:  make([]bool, len(graph.Nodes)),
		report:      report,
		stats:       stats,
	}
	validator.runInput, _ = parseContractQuiet(graph.InputContract)
	validator.workflowOut, _ = parseContractQuiet(graph.OutputContract)

	// Explicit and fixed outputs never depend on graph traversal.
	for index, node := range graph.Nodes {
		contract, ok := derivedNodeOutputContract(node, nil, nil)
		if ok {
			validator.nodeOutputs[index], validator.nodeHasOut[index] = contract, true
		}
	}
	// A loop's latch is guaranteed by phase five to be an explicit-output node.
	for index, node := range graph.Nodes {
		if node.Type != NodeLoop {
			continue
		}
		config := node.Config.(LoopConfig)
		latch := analysis.nodeIndex[config.LatchNodeID]
		if !validator.nodeHasOut[latch] {
			continue
		}
		contract, ok := derivedNodeOutputContract(node, &validator.nodeOutputs[latch], nil)
		if ok {
			validator.nodeOutputs[index], validator.nodeHasOut[index] = contract, true
		}
	}
	return validator
}

func (validator *valueValidator) validate() {
	for nodeIndex, node := range validator.graph.Nodes {
		nodePath := fmt.Sprintf("/nodes/%d", nodeIndex)
		for _, name := range sortedInputNames(node.Inputs) {
			binding := node.Inputs[name]
			path := joinPath(joinPath(joinPath(nodePath, "inputs"), name), "value")
			contract, ok := validator.resolveRef(path, binding.Value, valueUse{ownerNodeID: node.ID, consumer: nodeIndex})
			target := parsedOutputContract{Type: binding.ExpectedType}
			compatible := outputContractsCompatible(contract, target)
			if binding.Value.Source == ValueLiteral {
				compatible = literalMatchesContract(binding.Value.Value, target)
			}
			if ok && !compatible {
				validator.add(node.ID, path, CodeValueTypeIncompatible, "value is incompatible with the input expected_type")
			}
		}

		configPath := joinPath(nodePath, "config")
		switch config := node.Config.(type) {
		case TransformConfig:
			validator.validateTransform(nodeIndex, node, configPath, config)
		case LoopConfig:
			validator.validatePredicate(
				joinPath(configPath, "continue_predicate"),
				config.ContinuePredicate,
				valueUse{ownerNodeID: node.ID, consumer: validator.analysis.nodeIndex[config.LatchNodeID], latchSelf: true},
			)
		case DeliverConfig:
			path := joinPath(configPath, "result")
			contract, ok := validator.resolveRef(path, config.Result, valueUse{ownerNodeID: node.ID, consumer: nodeIndex})
			if ok && config.Result.Source == ValueLiteral {
				if !literalMatchesContract(config.Result.Value, validator.workflowOut) {
					validator.add(node.ID, path, CodeOutputContractIncompatible, "literal result does not satisfy the workflow output contract")
				}
			} else if ok {
				validateOutputContractCompatibility(validator.report, node.ID, path, contract, validator.workflowOut)
			}
		}
	}

	for edgeIndex, edge := range validator.graph.Edges {
		if edge.Predicate == nil {
			continue
		}
		source := validator.analysis.nodeIndex[edge.FromNodeID]
		validator.validatePredicate(
			fmt.Sprintf("/edges/%d/predicate", edgeIndex),
			*edge.Predicate,
			valueUse{ownerNodeID: edge.FromNodeID, consumer: source},
		)
	}
}

func (validator *valueValidator) validateTransform(nodeIndex int, node Node, path string, config TransformConfig) {
	if node.Output == nil {
		return
	}
	target, targetOK := parseContractQuiet(*node.Output)
	var derived parsedOutputContract
	valid := true
	use := valueUse{ownerNodeID: node.ID, consumer: nodeIndex}
	switch config.Operation {
	case TransformIdentity:
		if config.Value == nil {
			return
		}
		derived, valid = validator.resolveRef(joinPath(path, "value"), *config.Value, use)
	case TransformObject:
		properties := make(map[string]*FixedJSONSchema, len(config.Fields))
		required := make([]string, 0, len(config.Fields))
		for _, name := range sortedValueRefNames(config.Fields) {
			contract, ok := validator.resolveRef(joinPath(joinPath(path, "fields"), name), config.Fields[name], use)
			if !ok {
				valid = false
				continue
			}
			properties[name] = contractAsSchema(contract)
			if targetOK && config.Fields[name].Source == ValueLiteral {
				if targetProperty, ok := objectContractProperty(target, name); ok &&
					literalMatchesSchema(config.Fields[name].Value, targetProperty) {
					properties[name] = targetProperty
				}
			}
			required = append(required, name)
		}
		derived = parsedOutputContract{Type: ValueJSON, Schema: objectSchema(properties, required)}
	case TransformArray:
		contracts := make([]parsedOutputContract, 0, len(config.Items))
		for index, ref := range config.Items {
			contract, ok := validator.resolveRef(fmt.Sprintf("%s/%d", joinPath(path, "items"), index), ref, use)
			if !ok {
				valid = false
				continue
			}
			contracts = append(contracts, contract)
		}
		array := typedSchema(JSONSchemaArray)
		if len(contracts) != 0 {
			homogeneous := true
			for index := 1; index < len(contracts); index++ {
				if !contractsEqual(contracts[0], contracts[index]) {
					homogeneous = false
					break
				}
			}
			if homogeneous {
				array.itemsPresent = true
				array.items = contractAsSchema(contracts[0])
			}
			cacheSchemaSemanticKeys(array)
		}
		derived = parsedOutputContract{Type: ValueJSON, Schema: array}
	default:
		return
	}
	if !valid || !targetOK {
		return
	}
	outputPath := fmt.Sprintf("/nodes/%d/output", nodeIndex)
	if literalValue, allLiteral := transformLiteralValue(config); allLiteral {
		if !jsonValueMatchesContract(literalValue, target) {
			validator.add(node.ID, outputPath, CodeOutputContractIncompatible, "literal transform result does not satisfy its output contract")
		}
		return
	}
	validateOutputContractCompatibility(validator.report, node.ID, outputPath, derived, target)
}

func (validator *valueValidator) validatePredicate(path string, predicate Predicate, use valueUse) {
	leftUse := use
	leftUse.allowMissing = predicate.Operator == OperatorExists
	left, leftOK := validator.resolveRef(joinPath(path, "left"), predicate.Left, leftUse)
	if predicate.Operator == OperatorExists || predicate.Right == nil {
		return
	}
	right, rightOK := validator.resolveRef(joinPath(path, "right"), *predicate.Right, use)
	if !leftOK || !rightOK {
		return
	}
	issuePaths := make([]string, 0, 2)
	switch predicate.Operator {
	case OperatorEQ, OperatorNEQ:
		compatible := contractsEqual(left, right)
		if predicate.Left.Source == ValueLiteral {
			compatible = literalMatchesContract(predicate.Left.Value, right)
		} else if predicate.Right.Source == ValueLiteral {
			compatible = literalMatchesContract(predicate.Right.Value, left)
		}
		if !compatible {
			if predicate.Right.Source == ValueLiteral {
				issuePaths = append(issuePaths, joinPath(path, "right"))
			} else {
				issuePaths = append(issuePaths, joinPath(path, "left"))
			}
		}
	case OperatorGT, OperatorGTE, OperatorLT, OperatorLTE:
		if left.Type != ValueNumber {
			issuePaths = append(issuePaths, joinPath(path, "left"))
		}
		if right.Type != ValueNumber {
			issuePaths = append(issuePaths, joinPath(path, "right"))
		}
	case OperatorContains:
		if left.Type == ValueText {
			if right.Type != ValueText {
				issuePaths = append(issuePaths, joinPath(path, "right"))
			}
		} else if item, ok := arrayItemContract(left); ok {
			if !contractsEqual(item, right) {
				issuePaths = append(issuePaths, joinPath(path, "right"))
			}
		} else {
			issuePaths = append(issuePaths, joinPath(path, "left"))
		}
	case OperatorIn:
		if item, ok := arrayItemContract(right); ok {
			if !contractsEqual(left, item) {
				issuePaths = append(issuePaths, joinPath(path, "left"))
			}
		} else {
			issuePaths = append(issuePaths, joinPath(path, "right"))
		}
	}
	for _, issuePath := range issuePaths {
		validator.add(use.ownerNodeID, issuePath, CodeValueTypeIncompatible, "predicate operand does not match the operator signature")
	}
}

func (validator *valueValidator) resolveRef(path string, ref ValueRef, use valueUse) (parsedOutputContract, bool) {
	if validator.stats != nil {
		validator.stats.ValueRefsVisited++
	}
	switch ref.Source {
	case ValueLiteral:
		contract, ok := literalContract(ref.Value)
		return contract, ok
	case ValueRunInput:
		contract, code := resolvePointerContract(validator.runInput, ref.Path, use.allowMissing, validator.stats)
		if code != "" {
			validator.add(use.ownerNodeID, joinPath(path, "path"), code, "run_input pointer cannot be proven")
			return parsedOutputContract{}, false
		}
		return contract, true
	case ValueNodeOutput:
		producer := validator.analysis.nodeIndex[ref.NodeID]
		if !validator.nodeHasOut[producer] {
			validator.add(use.ownerNodeID, joinPath(path, "node_id"), CodeValueSourceUnavailable, "referenced node has no readable output")
			return parsedOutputContract{}, false
		}
		if !validator.validateNodeOutputScope(path, ref, producer, use) {
			return parsedOutputContract{}, false
		}
		contract, code := resolvePointerContract(validator.nodeOutputs[producer], ref.Path, use.allowMissing, validator.stats)
		if code != "" {
			validator.add(use.ownerNodeID, joinPath(path, "path"), code, "node_output pointer cannot be proven")
			return parsedOutputContract{}, false
		}
		if ref.Iteration == IterationPrevious && ref.Default != nil {
			if !literalMatchesContract(ref.Default.Value, contract) {
				validator.add(use.ownerNodeID, joinPath(path, "default"), CodeValueTypeIncompatible, "previous_iteration default is incompatible with the referenced output")
			}
		}
		return contract, true
	default:
		return parsedOutputContract{}, false
	}
}

func (validator *valueValidator) validateNodeOutputScope(path string, ref ValueRef, producer int, use valueUse) bool {
	consumer := use.consumer
	producerLoop := validator.loopOwner(producer)
	consumerLoop := validator.loopOwner(consumer)

	if consumerLoop >= 0 {
		loop := validator.loops.loops[consumerLoop]
		if producerLoop == consumerLoop {
			if ref.Iteration == "" {
				validator.add(use.ownerNodeID, joinPath(path, "iteration"), CodeValueIterationRequired, "same-loop body reference requires iteration")
				if ref.Default != nil {
					validator.add(use.ownerNodeID, joinPath(path, "default"), CodeValueDefaultForbidden, "default is only valid with previous_iteration")
				}
				return false
			}
			switch ref.Iteration {
			case IterationCurrent:
				if ref.Default != nil {
					validator.add(use.ownerNodeID, joinPath(path, "default"), CodeValueDefaultForbidden, "current_iteration forbids default")
				}
				if producer == consumer && use.latchSelf && consumer == loop.latch {
					return true
				}
				if !validator.strictlyDominates(producer, consumer) {
					validator.add(use.ownerNodeID, joinPath(path, "node_id"), CodeValueNotDominating, "current_iteration producer must strictly dominate its consumer")
					return false
				}
				return true
			case IterationPrevious:
				if ref.Default == nil {
					validator.add(use.ownerNodeID, joinPath(path, "default"), CodeValuePreviousDefaultRequired, "previous_iteration requires a first-iteration literal default")
				}
				if !validator.dominates(producer, loop.latch) {
					validator.add(use.ownerNodeID, joinPath(path, "node_id"), CodeValueNotDominating, "previous_iteration producer must dominate the loop latch")
					return false
				}
				return ref.Default != nil
			}
		}
		if producerLoop >= 0 {
			validator.add(use.ownerNodeID, joinPath(path, "node_id"), CodeValueLoopScopeInvalid, "ValueRef crosses loop bodies")
			return false
		}
		if validator.loops.loopByHeader[producer] == consumerLoop {
			validator.add(use.ownerNodeID, joinPath(path, "node_id"), CodeValueLoopScopeInvalid, "loop output is unavailable inside its body")
			return false
		}
		invalidFields := false
		if ref.Iteration != "" {
			validator.add(use.ownerNodeID, joinPath(path, "iteration"), CodeValueIterationForbidden, "loop invariant reference forbids iteration")
			invalidFields = true
		}
		if ref.Default != nil {
			validator.add(use.ownerNodeID, joinPath(path, "default"), CodeValueDefaultForbidden, "loop invariant reference forbids default")
			invalidFields = true
		}
		if invalidFields {
			return false
		}
		if !validator.strictlyDominates(producer, loop.header) {
			validator.add(use.ownerNodeID, joinPath(path, "node_id"), CodeValueNotDominating, "loop invariant producer must strictly dominate the header")
			return false
		}
		return true
	}

	if producerLoop >= 0 {
		validator.add(use.ownerNodeID, joinPath(path, "node_id"), CodeValueLoopScopeInvalid, "loop body output cannot be read outside the loop")
		return false
	}
	invalidFields := false
	if ref.Iteration != "" {
		validator.add(use.ownerNodeID, joinPath(path, "iteration"), CodeValueIterationForbidden, "non-loop reference forbids iteration")
		invalidFields = true
	}
	if ref.Default != nil {
		validator.add(use.ownerNodeID, joinPath(path, "default"), CodeValueDefaultForbidden, "non-loop reference forbids default")
		invalidFields = true
	}
	if invalidFields {
		return false
	}
	if !validator.strictlyDominates(producer, consumer) {
		validator.add(use.ownerNodeID, joinPath(path, "node_id"), CodeValueNotDominating, "producer must strictly dominate its consumer")
		return false
	}
	return true
}

func (validator *valueValidator) loopOwner(node int) int {
	if validator.stats != nil {
		validator.stats.LoopOwnerLookups++
	}
	if node < 0 || node >= len(validator.loops.bodyOwnerByNode) {
		return -1
	}
	return validator.loops.bodyOwnerByNode[node]
}

func (validator *valueValidator) dominates(producer, consumer int) bool {
	if validator.stats != nil {
		validator.stats.DominatorQueries++
	}
	return validator.analysis.dominates(producer, consumer)
}

func (validator *valueValidator) strictlyDominates(producer, consumer int) bool {
	return producer != consumer && validator.dominates(producer, consumer)
}

func (validator *valueValidator) add(nodeID, path, code, message string) {
	addSchemaIssue(validator.report, nodeID, path, code, message)
}

func decodeJSONPointer(path string) ([]string, bool) {
	if path == "" {
		return []string{}, true
	}
	if path[0] != '/' {
		return nil, false
	}
	rawTokens := strings.Split(path[1:], "/")
	tokens := make([]string, len(rawTokens))
	for index, raw := range rawTokens {
		var builder strings.Builder
		for offset := 0; offset < len(raw); offset++ {
			if raw[offset] != '~' {
				builder.WriteByte(raw[offset])
				continue
			}
			if offset+1 >= len(raw) || (raw[offset+1] != '0' && raw[offset+1] != '1') {
				return nil, false
			}
			offset++
			if raw[offset] == '0' {
				builder.WriteByte('~')
			} else {
				builder.WriteByte('/')
			}
		}
		tokens[index] = builder.String()
	}
	return tokens, true
}

func resolvePointerContract(contract parsedOutputContract, path string, allowMissing bool, stats *valueValidationStats) (parsedOutputContract, string) {
	tokens, valid := decodeJSONPointer(path)
	if !valid {
		return parsedOutputContract{}, CodeJSONPointerInvalid
	}
	if stats != nil {
		stats.PointerTokens += len(tokens)
	}
	if len(tokens) == 0 {
		return contract, ""
	}
	if contract.Type != ValueJSON {
		return parsedOutputContract{}, CodeJSONPointerUnprovable
	}
	schema := contract.Schema
	for _, token := range tokens {
		if schema == nil {
			if allowMissing {
				continue
			}
			return parsedOutputContract{}, CodeJSONPointerUnprovable
		}
		schemaType, hasType := schema.schemaTypeValue()
		if !hasType {
			if allowMissing {
				schema = nil
				continue
			}
			return parsedOutputContract{}, CodeJSONPointerUnprovable
		}
		switch schemaType {
		case JSONSchemaObject:
			property, exists := schema.property(token)
			if !exists {
				mode, additional := schema.additionalPropertiesView()
				if mode == additionalPropertiesSchema {
					schema = additional
					continue
				}
				if allowMissing {
					schema = nil
					continue
				}
				return parsedOutputContract{}, CodeJSONPointerUnprovable
			}
			if !allowMissing && !schema.isRequired(token) {
				return parsedOutputContract{}, CodeJSONPointerUnprovable
			}
			schema = property
		case JSONSchemaArray:
			if !validArrayPointerToken(token) {
				return parsedOutputContract{}, CodeJSONPointerInvalid
			}
			item, exists := schema.itemsSchema()
			if !exists {
				if allowMissing {
					schema = nil
					continue
				}
				return parsedOutputContract{}, CodeJSONPointerUnprovable
			}
			schema = item
		default:
			if allowMissing {
				schema = nil
				continue
			}
			return parsedOutputContract{}, CodeJSONPointerUnprovable
		}
	}
	if schema == nil {
		return parsedOutputContract{Type: ValueJSON}, ""
	}
	return contractFromSchema(schema), ""
}

func validArrayPointerToken(token string) bool {
	if token == "0" {
		return true
	}
	if token == "" || token[0] == '0' {
		return false
	}
	for _, value := range []byte(token) {
		if value < '0' || value > '9' {
			return false
		}
	}
	_, err := strconv.ParseInt(token, 10, strconv.IntSize)
	return err == nil
}

func literalContract(raw json.RawMessage) (parsedOutputContract, bool) {
	value, ok := decodeLiteralValue(raw)
	if !ok {
		return parsedOutputContract{}, false
	}
	switch value.(type) {
	case string:
		return parsedOutputContract{Type: ValueText}, true
	case bool:
		return parsedOutputContract{Type: ValueBoolean}, true
	case json.Number:
		return parsedOutputContract{Type: ValueNumber}, true
	case map[string]any:
		return parsedOutputContract{Type: ValueJSON, Schema: typedSchema(JSONSchemaObject)}, true
	case []any:
		return parsedOutputContract{Type: ValueJSON, Schema: typedSchema(JSONSchemaArray)}, true
	case nil:
		return parsedOutputContract{Type: ValueJSON, Schema: typedSchema(JSONSchemaNull)}, true
	default:
		return parsedOutputContract{}, false
	}
}

func decodeLiteralValue(raw json.RawMessage) (any, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, false
	}
	return value, true
}

func transformLiteralValue(config TransformConfig) (any, bool) {
	switch config.Operation {
	case TransformIdentity:
		if config.Value == nil || config.Value.Source != ValueLiteral {
			return nil, false
		}
		return decodeLiteralValue(config.Value.Value)
	case TransformObject:
		result := make(map[string]any, len(config.Fields))
		for _, name := range sortedValueRefNames(config.Fields) {
			ref := config.Fields[name]
			if ref.Source != ValueLiteral {
				return nil, false
			}
			value, ok := decodeLiteralValue(ref.Value)
			if !ok {
				return nil, false
			}
			result[name] = value
		}
		return result, true
	case TransformArray:
		result := make([]any, len(config.Items))
		for index, ref := range config.Items {
			if ref.Source != ValueLiteral {
				return nil, false
			}
			value, ok := decodeLiteralValue(ref.Value)
			if !ok {
				return nil, false
			}
			result[index] = value
		}
		return result, true
	default:
		return nil, false
	}
}

func literalMatchesContract(raw json.RawMessage, target parsedOutputContract) bool {
	value, ok := decodeLiteralValue(raw)
	return ok && jsonValueMatchesContract(value, target)
}

func literalMatchesSchema(raw json.RawMessage, target *FixedJSONSchema) bool {
	value, ok := decodeLiteralValue(raw)
	return ok && jsonValueMatchesSchema(value, target)
}

func objectContractProperty(contract parsedOutputContract, name string) (*FixedJSONSchema, bool) {
	if contract.Type != ValueJSON || contract.Schema == nil {
		return nil, false
	}
	schemaType, ok := contract.Schema.schemaTypeValue()
	if !ok || schemaType != JSONSchemaObject || !contract.Schema.isRequired(name) {
		return nil, false
	}
	return contract.Schema.property(name)
}

func jsonValueMatchesContract(value any, target parsedOutputContract) bool {
	switch target.Type {
	case ValueText:
		_, ok := value.(string)
		return ok && target.Schema == nil
	case ValueBoolean:
		_, ok := value.(bool)
		return ok && target.Schema == nil
	case ValueNumber:
		_, ok := value.(json.Number)
		return ok && target.Schema == nil
	case ValueJSON:
		return jsonValueMatchesSchema(value, target.Schema)
	default:
		return false
	}
}

func jsonValueMatchesSchema(value any, schema *FixedJSONSchema) bool {
	if schema == nil {
		return true
	}
	if schemaType, present := schema.schemaTypeValue(); present && !jsonValueMatchesSchemaType(value, schemaType) {
		return false
	}
	valueKey := semanticJSONValueKey(value, nil)
	if schema.hasConst() && schema.constValue.key != valueKey {
		return false
	}
	if schema.enumPresent {
		matched := false
		for _, candidate := range schema.enumSemanticKeys() {
			if candidate == valueKey {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if object, ok := value.(map[string]any); ok {
		for _, required := range schema.requiredNames() {
			if _, present := object[required]; !present {
				return false
			}
		}
		for name, childValue := range object {
			if child, declared := schema.property(name); declared {
				if !jsonValueMatchesSchema(childValue, child) {
					return false
				}
				continue
			}
			mode, additional := schema.additionalPropertiesView()
			switch mode {
			case additionalPropertiesDenied:
				return false
			case additionalPropertiesSchema:
				if !jsonValueMatchesSchema(childValue, additional) {
					return false
				}
			}
		}
	}
	if array, ok := value.([]any); ok {
		if item, present := schema.itemsSchema(); present {
			for _, child := range array {
				if !jsonValueMatchesSchema(child, item) {
					return false
				}
			}
		}
	}
	return true
}

func jsonValueMatchesSchemaType(value any, schemaType JSONSchemaType) bool {
	switch schemaType {
	case JSONSchemaNull:
		return value == nil
	case JSONSchemaBoolean:
		_, ok := value.(bool)
		return ok
	case JSONSchemaObject:
		_, ok := value.(map[string]any)
		return ok
	case JSONSchemaArray:
		_, ok := value.([]any)
		return ok
	case JSONSchemaNumber:
		_, ok := value.(json.Number)
		return ok
	case JSONSchemaInteger:
		number, ok := value.(json.Number)
		return ok && jsonNumberIsInteger(number.String())
	case JSONSchemaString:
		_, ok := value.(string)
		return ok
	default:
		return false
	}
}

func jsonNumberIsInteger(value string) bool {
	normalized := normalizeJSONNumber(value)
	if normalized == "0e0" {
		return true
	}
	exponent := normalized[strings.LastIndexByte(normalized, 'e')+1:]
	return exponent == "0" || exponent[0] != '-'
}

func parseContractQuiet(contract OutputContract) (parsedOutputContract, bool) {
	result := parsedOutputContract{Type: contract.Type}
	if len(contract.Schema) == 0 {
		return result, true
	}
	schema, report := parseFixedJSONSchema(contract.Schema, "", "")
	if len(report.Issues) != 0 {
		return parsedOutputContract{}, false
	}
	result.Schema = schema
	return result, true
}

func derivedNodeOutputContract(node Node, latchContract *parsedOutputContract, _ []parsedOutputContract) (parsedOutputContract, bool) {
	if node.Output != nil {
		return parseContractQuiet(*node.Output)
	}
	switch config := node.Config.(type) {
	case WaitConfig:
		schema, report := parseFixedJSONSchema(config.ResumeSchema, "", "")
		return parsedOutputContract{Type: ValueJSON, Schema: schema}, len(report.Issues) == 0
	case JoinConfig:
		return parsedOutputContract{Type: ValueJSON, Schema: joinOutputSchema(config)}, true
	case LoopConfig:
		if latchContract == nil {
			return parsedOutputContract{}, false
		}
		return parsedOutputContract{Type: ValueJSON, Schema: loopOutputSchema(*latchContract)}, true
	default:
		return parsedOutputContract{}, false
	}
}

func typedSchema(value JSONSchemaType) *FixedJSONSchema {
	result := &FixedJSONSchema{schemaType: &value}
	cacheSchemaSemanticKeys(result)
	return result
}

func objectSchema(properties map[string]*FixedJSONSchema, required []string) *FixedJSONSchema {
	value := JSONSchemaObject
	deny := false
	result := &FixedJSONSchema{
		schemaType:           &value,
		properties:           properties,
		required:             append([]string(nil), required...),
		additionalProperties: &fixedAdditionalProperties{booleanValue: &deny},
		propertiesPresent:    true,
		requiredPresent:      true,
	}
	cacheSchemaSemanticKeys(result)
	return result
}

func stringEnumSchema(values ...string) *FixedJSONSchema {
	valueType := JSONSchemaString
	result := &FixedJSONSchema{schemaType: &valueType, enumPresent: true, enum: make([]semanticJSONValue, len(values))}
	for index, value := range values {
		result.enum[index] = semanticJSONValue{key: semanticJSONValueKey(value, nil)}
	}
	cacheSchemaSemanticKeys(result)
	return result
}

func stringConstSchema(value string) *FixedJSONSchema {
	valueType := JSONSchemaString
	constant := semanticJSONValue{key: semanticJSONValueKey(value, nil)}
	result := &FixedJSONSchema{schemaType: &valueType, constPresent: true, constValue: &constant}
	cacheSchemaSemanticKeys(result)
	return result
}

func loopOutputSchema(latch parsedOutputContract) *FixedJSONSchema {
	return objectSchema(map[string]*FixedJSONSchema{
		"iteration_count": typedSchema(JSONSchemaInteger),
		"limit_reached":   typedSchema(JSONSchemaBoolean),
		"latch_result":    contractAsSchema(latch),
	}, []string{"iteration_count", "limit_reached", "latch_result"})
}

func joinOutputSchema(config JoinConfig) *FixedJSONSchema {
	leg := objectSchema(map[string]*FixedJSONSchema{
		"node_id":              typedSchema(JSONSchemaString),
		"decision_disposition": stringEnumSchema("cut", "failed", "pending", "succeeded"),
		"result":               {},
		"error":                {},
	}, []string{"node_id", "decision_disposition", "result", "error"})
	legs := typedSchema(JSONSchemaArray)
	legs.itemsPresent = true
	legs.items = leg
	cacheSchemaSemanticKeys(legs)
	return objectSchema(map[string]*FixedJSONSchema{
		"decision": stringEnumSchema("failed", "succeeded"),
		"policy":   stringConstSchema(string(config.Policy)),
		"legs":     legs,
	}, []string{"decision", "policy", "legs"})
}

func contractAsSchema(contract parsedOutputContract) *FixedJSONSchema {
	switch contract.Type {
	case ValueText:
		return typedSchema(JSONSchemaString)
	case ValueBoolean:
		return typedSchema(JSONSchemaBoolean)
	case ValueNumber:
		return typedSchema(JSONSchemaNumber)
	case ValueJSON:
		if contract.Schema != nil {
			return contract.Schema
		}
		result := &FixedJSONSchema{}
		cacheSchemaSemanticKeys(result)
		return result
	default:
		return nil
	}
}

func contractFromSchema(schema *FixedJSONSchema) parsedOutputContract {
	value, ok := schema.schemaTypeValue()
	if !ok {
		return parsedOutputContract{Type: ValueJSON, Schema: schema}
	}
	switch value {
	case JSONSchemaString:
		return parsedOutputContract{Type: ValueText}
	case JSONSchemaBoolean:
		return parsedOutputContract{Type: ValueBoolean}
	case JSONSchemaNumber, JSONSchemaInteger:
		return parsedOutputContract{Type: ValueNumber}
	default:
		return parsedOutputContract{Type: ValueJSON, Schema: schema}
	}
}

func contractsEqual(left, right parsedOutputContract) bool {
	return outputContractsCompatible(left, right) && outputContractsCompatible(right, left)
}

func arrayItemContract(contract parsedOutputContract) (parsedOutputContract, bool) {
	if contract.Type != ValueJSON || contract.Schema == nil {
		return parsedOutputContract{}, false
	}
	value, ok := contract.Schema.schemaTypeValue()
	if !ok || value != JSONSchemaArray {
		return parsedOutputContract{}, false
	}
	item, ok := contract.Schema.itemsSchema()
	if !ok {
		return parsedOutputContract{}, false
	}
	return contractFromSchema(item), true
}
