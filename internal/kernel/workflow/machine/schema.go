package machine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// JSONSchemaType is the single-string type discriminator supported by the v1
// executable JSON Schema subset. Union type arrays and boolean schemas are
// intentionally outside this contract.
type JSONSchemaType string

const (
	JSONSchemaNull    JSONSchemaType = "null"
	JSONSchemaBoolean JSONSchemaType = "boolean"
	JSONSchemaObject  JSONSchemaType = "object"
	JSONSchemaArray   JSONSchemaType = "array"
	JSONSchemaNumber  JSONSchemaType = "number"
	JSONSchemaInteger JSONSchemaType = "integer"
	JSONSchemaString  JSONSchemaType = "string"
)

// FixedJSONSchema is the parsed v1 executable subset. Private presence bits
// preserve the semantic difference between an omitted keyword and a keyword
// whose value is explicitly empty.
type FixedJSONSchema struct {
	schemaType           *JSONSchemaType
	properties           map[string]*FixedJSONSchema
	required             []string
	additionalProperties *fixedAdditionalProperties
	items                *FixedJSONSchema
	enum                 []semanticJSONValue
	constValue           *semanticJSONValue

	propertiesPresent bool
	requiredPresent   bool
	itemsPresent      bool
	enumPresent       bool
	constPresent      bool
	semanticKey       string
}

type fixedAdditionalProperties struct {
	booleanValue *bool
	schema       *FixedJSONSchema
}

// semanticJSONValue retains only the deterministic equality key needed by the
// fixed-schema algebra. It is not RFC 8785/JCS bytes and must never be used as
// an artifact hash input.
type semanticJSONValue struct {
	key string
}

type additionalPropertiesMode uint8

const (
	additionalPropertiesUnspecified additionalPropertiesMode = iota
	additionalPropertiesAllowed
	additionalPropertiesDenied
	additionalPropertiesSchema
)

type schemaComplexityStats struct {
	ValuesVisited         int
	NumberBytesVisited    int
	ExponentDigitsScanned int
	ExponentDigitOps      int
	ExponentOutputDigits  int
}

type parsedOutputContract struct {
	Type   ValueType
	Schema *FixedJSONSchema
}

type fixedSchemaProblem struct {
	Path    string
	Code    string
	Message string
}

// RuntimeSchemaProblem is one field-level problem found while parsing a
// frozen yield schema or validating a resume value against it.
type RuntimeSchemaProblem struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ValidateRuntimeInputSchema validates the executable fixed JSON Schema
// subset used by parked workflow inputs.
func ValidateRuntimeInputSchema(raw json.RawMessage) []RuntimeSchemaProblem {
	_, problems := parseFixedJSONSchemaProblems(raw, "", nil)
	return runtimeSchemaProblems(problems)
}

// ValidateRuntimeInput parses one frozen schema and validates the supplied
// JSON value without consulting mutable workflow configuration.
func ValidateRuntimeInput(
	schemaRaw json.RawMessage,
	inputRaw json.RawMessage,
) (json.RawMessage, []RuntimeSchemaProblem) {
	schema, schemaProblems := parseFixedJSONSchemaProblems(schemaRaw, "", nil)
	if len(schemaProblems) > 0 {
		return nil, runtimeSchemaProblems(schemaProblems)
	}
	return validateRuntimeValue(schema, inputRaw)
}

// ValidateRuntimeOutput validates one final value against the frozen workflow
// output contract without consulting mutable workflow configuration.
func ValidateRuntimeOutput(
	contract OutputContract,
	outputRaw json.RawMessage,
) (json.RawMessage, []RuntimeSchemaProblem) {
	parsed, ok := parseContractQuiet(contract)
	if !ok {
		return nil, []RuntimeSchemaProblem{{
			Code:    CodeSchemaKeywordInvalid,
			Message: "workflow output contract is invalid",
		}}
	}
	return validateRuntimeValue(contractAsSchema(parsed), outputRaw)
}

func validateRuntimeValue(
	schema *FixedJSONSchema,
	inputRaw json.RawMessage,
) (json.RawMessage, []RuntimeSchemaProblem) {
	if issue := inspectJSON(inputRaw); issue != nil {
		return nil, []RuntimeSchemaProblem{{
			Path: prefixedPath("", issue.path), Code: issue.code, Message: issue.Error(),
		}}
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(inputRaw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, []RuntimeSchemaProblem{{
			Code: CodeJSONInvalid, Message: "resume input must be valid JSON",
		}}
	}
	var problems []RuntimeSchemaProblem
	validateRuntimeSchemaValue(value, schema, "", &problems)
	if len(problems) > 0 {
		sort.SliceStable(problems, func(i, j int) bool {
			if problems[i].Path != problems[j].Path {
				return problems[i].Path < problems[j].Path
			}
			return problems[i].Code < problems[j].Code
		})
		return nil, problems
	}
	return append(json.RawMessage(nil), inputRaw...), nil
}

func runtimeSchemaProblems(problems []fixedSchemaProblem) []RuntimeSchemaProblem {
	result := make([]RuntimeSchemaProblem, 0, len(problems))
	for _, problem := range problems {
		result = append(result, RuntimeSchemaProblem{
			Path: problem.Path, Code: problem.Code, Message: problem.Message,
		})
	}
	return result
}

func validateRuntimeSchemaValue(
	value any,
	schema *FixedJSONSchema,
	path string,
	problems *[]RuntimeSchemaProblem,
) {
	if schema == nil {
		return
	}
	if schemaType, present := schema.schemaTypeValue(); present &&
		!jsonValueMatchesSchemaType(value, schemaType) {
		*problems = append(*problems, RuntimeSchemaProblem{
			Path: path, Code: CodeTypeInvalid, Message: "value does not match schema type",
		})
		return
	}
	valueKey := semanticJSONValueKey(value, nil)
	if schema.hasConst() && schema.constValue.key != valueKey {
		*problems = append(*problems, RuntimeSchemaProblem{
			Path: path, Code: CodeValueTypeIncompatible, Message: "value does not match const",
		})
	}
	if schema.enumPresent {
		matched := false
		for _, candidate := range schema.enum {
			if candidate.key == valueKey {
				matched = true
				break
			}
		}
		if !matched {
			*problems = append(*problems, RuntimeSchemaProblem{
				Path: path, Code: CodeValueTypeIncompatible, Message: "value is not in enum",
			})
		}
	}
	switch typed := value.(type) {
	case map[string]any:
		for _, required := range schema.requiredNames() {
			if _, ok := typed[required]; !ok {
				*problems = append(*problems, RuntimeSchemaProblem{
					Path: joinPath(path, required), Code: CodeFieldRequired,
					Message: "required field is missing",
				})
			}
		}
		additionalMode, additionalSchema := schema.additionalPropertiesView()
		for _, name := range sortedAnyKeys(typed) {
			childPath := joinPath(path, name)
			if child, ok := schema.property(name); ok {
				validateRuntimeSchemaValue(typed[name], child, childPath, problems)
				continue
			}
			switch additionalMode {
			case additionalPropertiesDenied:
				*problems = append(*problems, RuntimeSchemaProblem{
					Path: childPath, Code: CodeUnknownField,
					Message: "additional property is not allowed",
				})
			case additionalPropertiesSchema:
				validateRuntimeSchemaValue(typed[name], additionalSchema, childPath, problems)
			}
		}
	case []any:
		if item, present := schema.itemsSchema(); present {
			for index, child := range typed {
				validateRuntimeSchemaValue(
					child, item, fmt.Sprintf("%s/%d", path, index), problems,
				)
			}
		}
	}
}

func parseFixedJSONSchema(raw json.RawMessage, path, nodeID string) (*FixedJSONSchema, Report) {
	var report Report
	schema, problems := parseFixedJSONSchemaProblems(raw, path, nil)
	for _, problem := range problems {
		addSchemaIssue(&report, nodeID, problem.Path, problem.Code, problem.Message)
	}
	report.Sort()
	return schema, report
}

// parseFixedJSONSchemaProblems is the phase-neutral fixed-subset parser. Its
// callers decide whether problems belong to declared-schema phase 8 or an
// exact AgentVersion proof phase.
func parseFixedJSONSchemaProblems(
	raw json.RawMessage,
	path string,
	stats *schemaComplexityStats,
) (*FixedJSONSchema, []fixedSchemaProblem) {
	var problems []fixedSchemaProblem
	if issue := inspectJSON(raw); issue != nil {
		addFixedSchemaProblem(
			&problems,
			prefixedPath(path, issue.path),
			issue.code,
			issue.Error(),
		)
		return nil, problems
	}

	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		addFixedSchemaProblem(
			&problems,
			path,
			CodeSchemaKeywordInvalid,
			"schema must be an object",
		)
		return nil, problems
	}
	schema := parseFixedJSONSchemaValue(value, path, &problems, stats)
	cacheSchemaSemanticKeys(schema)
	return schema, problems
}

func parseFixedJSONSchemaValue(
	value any,
	path string,
	problems *[]fixedSchemaProblem,
	stats *schemaComplexityStats,
) *FixedJSONSchema {
	if stats != nil {
		stats.ValuesVisited++
	}
	object, ok := value.(map[string]any)
	if !ok {
		addFixedSchemaProblem(problems, path, CodeSchemaKeywordInvalid, "schema must be an object")
		return nil
	}

	schema := &FixedJSONSchema{}
	for _, keyword := range sortedAnyKeys(object) {
		keywordPath := joinPath(path, keyword)
		switch keyword {
		case "type":
			typeName, validString := object[keyword].(string)
			if !validString {
				addFixedSchemaProblem(problems, keywordPath, CodeSchemaKeywordInvalid, "type must be a single string")
				continue
			}
			schemaType := JSONSchemaType(typeName)
			if !validJSONSchemaType(schemaType) {
				addFixedSchemaProblem(problems, keywordPath, CodeSchemaTypeUnsupported, "unsupported schema type")
				continue
			}
			schema.schemaType = &schemaType

		case "properties":
			schema.propertiesPresent = true
			properties, validObject := object[keyword].(map[string]any)
			if !validObject {
				addFixedSchemaProblem(problems, keywordPath, CodeSchemaKeywordInvalid, "properties must be an object")
				continue
			}
			schema.properties = make(map[string]*FixedJSONSchema, len(properties))
			for _, name := range sortedAnyKeys(properties) {
				child := parseFixedJSONSchemaValue(
					properties[name],
					joinPath(keywordPath, name),
					problems,
					stats,
				)
				if child != nil {
					schema.properties[name] = child
				}
			}

		case "required":
			schema.requiredPresent = true
			values, validArray := object[keyword].([]any)
			if !validArray {
				addFixedSchemaProblem(problems, keywordPath, CodeSchemaKeywordInvalid, "required must be an array of strings")
				continue
			}
			seen := make(map[string]struct{}, len(values))
			schema.required = make([]string, 0, len(values))
			for index, value := range values {
				itemPath := fmt.Sprintf("%s/%d", keywordPath, index)
				name, validString := value.(string)
				if !validString {
					addFixedSchemaProblem(problems, itemPath, CodeSchemaKeywordInvalid, "required member must be a string")
					continue
				}
				if _, duplicate := seen[name]; duplicate {
					addFixedSchemaProblem(problems, itemPath, CodeSchemaRequiredDuplicate, "required member must be unique")
					continue
				}
				seen[name] = struct{}{}
				schema.required = append(schema.required, name)
			}

		case "additionalProperties":
			switch additional := object[keyword].(type) {
			case bool:
				value := additional
				schema.additionalProperties = &fixedAdditionalProperties{booleanValue: &value}
			case map[string]any:
				child := parseFixedJSONSchemaValue(additional, keywordPath, problems, stats)
				if child != nil {
					schema.additionalProperties = &fixedAdditionalProperties{schema: child}
				}
			default:
				addFixedSchemaProblem(problems, keywordPath, CodeSchemaKeywordInvalid, "additionalProperties must be a boolean or schema object")
			}

		case "items":
			schema.itemsPresent = true
			child := parseFixedJSONSchemaValue(object[keyword], keywordPath, problems, stats)
			if child != nil {
				schema.items = child
			}

		case "enum":
			schema.enumPresent = true
			values, validArray := object[keyword].([]any)
			if !validArray || len(values) == 0 {
				addFixedSchemaProblem(problems, keywordPath, CodeSchemaKeywordInvalid, "enum must be a non-empty array")
				continue
			}
			seen := make(map[string]struct{}, len(values))
			schema.enum = make([]semanticJSONValue, 0, len(values))
			for index, value := range values {
				key := semanticJSONValueKey(value, stats)
				if _, duplicate := seen[key]; duplicate {
					addFixedSchemaProblem(
						problems,
						fmt.Sprintf("%s/%d", keywordPath, index),
						CodeSchemaEnumDuplicate,
						"enum member must be unique by JSON semantics",
					)
					continue
				}
				seen[key] = struct{}{}
				schema.enum = append(schema.enum, semanticJSONValue{key: key})
			}

		case "const":
			schema.constPresent = true
			schema.constValue = &semanticJSONValue{key: semanticJSONValueKey(object[keyword], stats)}

		default:
			addFixedSchemaProblem(problems, keywordPath, CodeSchemaKeywordUnsupported, "schema keyword is not supported in v1")
		}
	}
	return schema
}

func addFixedSchemaProblem(
	problems *[]fixedSchemaProblem,
	path string,
	code string,
	message string,
) {
	*problems = append(*problems, fixedSchemaProblem{
		Path:    path,
		Code:    code,
		Message: message,
	})
}

func semanticJSONKey(raw json.RawMessage, path string, stats *schemaComplexityStats) (string, *dtoError) {
	if issue := inspectJSON(raw); issue != nil {
		return "", newDTOError(prefixedPath(path, issue.path), issue.code, issue.Error())
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return "", newDTOError(path, CodeJSONInvalid, "invalid JSON")
	}
	return semanticJSONValueKey(value, stats), nil
}

func semanticJSONValueKey(value any, stats *schemaComplexityStats) string {
	var builder strings.Builder
	appendSemanticJSONValueKey(&builder, value, stats)
	return builder.String()
}

func appendSemanticJSONValueKey(builder *strings.Builder, value any, stats *schemaComplexityStats) {
	if stats != nil {
		stats.ValuesVisited++
	}
	switch value := value.(type) {
	case nil:
		builder.WriteByte('z')
	case bool:
		if value {
			builder.WriteString("b1")
		} else {
			builder.WriteString("b0")
		}
	case string:
		builder.WriteByte('s')
		builder.WriteString(strconv.Quote(value))
	case json.Number:
		text := value.String()
		if stats != nil {
			stats.NumberBytesVisited += len(text)
		}
		builder.WriteByte('n')
		builder.WriteString(normalizeJSONNumberWithStats(text, stats))
		builder.WriteByte(';')
	case []any:
		builder.WriteByte('[')
		for _, item := range value {
			appendSemanticJSONValueKey(builder, item, stats)
			builder.WriteByte(',')
		}
		builder.WriteByte(']')
	case map[string]any:
		keys := sortedAnyKeys(value)
		builder.WriteByte('{')
		for _, key := range keys {
			builder.WriteString(strconv.Quote(key))
			builder.WriteByte(':')
			appendSemanticJSONValueKey(builder, value[key], stats)
			builder.WriteByte(',')
		}
		builder.WriteByte('}')
	default:
		panic(fmt.Sprintf("semantic JSON contains unexpected %T", value))
	}
}

func normalizeJSONNumber(value string) string {
	return normalizeJSONNumberWithStats(value, nil)
}

func normalizeJSONNumberWithStats(value string, stats *schemaComplexityStats) string {
	sign := ""
	if strings.HasPrefix(value, "-") {
		sign = "-"
		value = value[1:]
	}

	mantissa := value
	exponentText := "0"
	explicitExponent := false
	if index := strings.IndexAny(value, "eE"); index >= 0 {
		mantissa = value[:index]
		exponentText = value[index+1:]
		explicitExponent = true
	}

	fractionDigits := 0
	if index := strings.IndexByte(mantissa, '.'); index >= 0 {
		fractionDigits = len(mantissa) - index - 1
		mantissa = mantissa[:index] + mantissa[index+1:]
	}

	mantissa = strings.TrimLeft(mantissa, "0")
	if mantissa == "" {
		return "0e0"
	}

	trailingZeros := len(mantissa) - len(strings.TrimRight(mantissa, "0"))
	if trailingZeros != 0 {
		mantissa = mantissa[:len(mantissa)-trailingZeros]
	}
	exponent := parseSignedDecimalText(exponentText, explicitExponent, stats)
	exponent = addSmallSignedDecimal(exponent, trailingZeros-fractionDigits, stats)
	if stats != nil {
		stats.ExponentOutputDigits += len(exponent.digits)
	}
	return sign + mantissa + "e" + exponent.String()
}

type signedDecimalText struct {
	negative bool
	digits   string
}

func parseSignedDecimalText(value string, countInput bool, stats *schemaComplexityStats) signedDecimalText {
	negative := false
	if strings.HasPrefix(value, "+") || strings.HasPrefix(value, "-") {
		negative = value[0] == '-'
		value = value[1:]
	}
	if countInput && stats != nil {
		stats.ExponentDigitsScanned += len(value)
	}
	firstNonZero := 0
	for firstNonZero < len(value) && value[firstNonZero] == '0' {
		firstNonZero++
		if stats != nil {
			stats.ExponentDigitOps++
		}
	}
	if firstNonZero == len(value) {
		return signedDecimalText{digits: "0"}
	}
	return signedDecimalText{negative: negative, digits: value[firstNonZero:]}
}

func addSmallSignedDecimal(value signedDecimalText, delta int, stats *schemaComplexityStats) signedDecimalText {
	if delta == 0 {
		return value
	}
	deltaText := strconv.Itoa(delta)
	other := signedDecimalText{}
	if deltaText[0] == '-' {
		other.negative = true
		deltaText = deltaText[1:]
	}
	other.digits = deltaText
	return addSignedDecimal(value, other, stats)
}

func addSignedDecimal(left, right signedDecimalText, stats *schemaComplexityStats) signedDecimalText {
	if left.digits == "0" {
		return right
	}
	if right.digits == "0" {
		return left
	}
	if left.negative == right.negative {
		return signedDecimalText{
			negative: left.negative,
			digits:   addDecimalMagnitude(left.digits, right.digits, stats),
		}
	}

	switch compareDecimalMagnitude(left.digits, right.digits, stats) {
	case 0:
		return signedDecimalText{digits: "0"}
	case 1:
		return signedDecimalText{
			negative: left.negative,
			digits:   subtractDecimalMagnitude(left.digits, right.digits, stats),
		}
	default:
		return signedDecimalText{
			negative: right.negative,
			digits:   subtractDecimalMagnitude(right.digits, left.digits, stats),
		}
	}
}

func compareDecimalMagnitude(left, right string, stats *schemaComplexityStats) int {
	if len(left) > len(right) {
		return 1
	}
	if len(left) < len(right) {
		return -1
	}
	for index := range len(left) {
		if stats != nil {
			stats.ExponentDigitOps++
		}
		if left[index] > right[index] {
			return 1
		}
		if left[index] < right[index] {
			return -1
		}
	}
	return 0
}

func addDecimalMagnitude(left, right string, stats *schemaComplexityStats) string {
	size := max(len(left), len(right))
	result := make([]byte, size+1)
	leftIndex, rightIndex, outputIndex := len(left)-1, len(right)-1, size
	carry := 0
	for outputIndex > 0 {
		sum := carry
		if leftIndex >= 0 {
			sum += int(left[leftIndex] - '0')
			leftIndex--
		}
		if rightIndex >= 0 {
			sum += int(right[rightIndex] - '0')
			rightIndex--
		}
		result[outputIndex] = byte(sum%10) + '0'
		carry = sum / 10
		outputIndex--
		if stats != nil {
			stats.ExponentDigitOps++
		}
	}
	if carry != 0 {
		result[0] = byte(carry) + '0'
		return string(result)
	}
	return string(result[1:])
}

// subtractDecimalMagnitude returns left-right for left > right.
func subtractDecimalMagnitude(left, right string, stats *schemaComplexityStats) string {
	result := make([]byte, len(left))
	rightIndex := len(right) - 1
	borrow := 0
	for leftIndex := len(left) - 1; leftIndex >= 0; leftIndex-- {
		digit := int(left[leftIndex]-'0') - borrow
		if rightIndex >= 0 {
			digit -= int(right[rightIndex] - '0')
			rightIndex--
		}
		if digit < 0 {
			digit += 10
			borrow = 1
		} else {
			borrow = 0
		}
		result[leftIndex] = byte(digit) + '0'
		if stats != nil {
			stats.ExponentDigitOps++
		}
	}
	firstNonZero := 0
	for firstNonZero < len(result)-1 && result[firstNonZero] == '0' {
		firstNonZero++
		if stats != nil {
			stats.ExponentDigitOps++
		}
	}
	return string(result[firstNonZero:])
}

func (value signedDecimalText) String() string {
	if value.negative && value.digits != "0" {
		return "-" + value.digits
	}
	return value.digits
}

func (schema *FixedJSONSchema) SemanticKey() string {
	if schema == nil {
		return "invalid"
	}
	if schema.semanticKey != "" {
		return schema.semanticKey
	}
	var builder strings.Builder
	appendSchemaSemanticKey(&builder, schema)
	return builder.String()
}

func (schema *FixedJSONSchema) schemaTypeValue() (JSONSchemaType, bool) {
	if schema == nil || schema.schemaType == nil {
		return "", false
	}
	return *schema.schemaType, true
}

func (schema *FixedJSONSchema) property(name string) (*FixedJSONSchema, bool) {
	if schema == nil {
		return nil, false
	}
	value, ok := schema.properties[name]
	return value, ok
}

func (schema *FixedJSONSchema) propertyNames() []string {
	if schema == nil {
		return nil
	}
	names := make([]string, 0, len(schema.properties))
	for name := range schema.properties {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (schema *FixedJSONSchema) requiredNames() []string {
	if schema == nil {
		return nil
	}
	return append([]string(nil), schema.required...)
}

func (schema *FixedJSONSchema) isRequired(name string) bool {
	if schema == nil || !schema.requiredPresent {
		return false
	}
	for _, required := range schema.required {
		if required == name {
			return true
		}
	}
	return false
}

func (schema *FixedJSONSchema) enumSemanticKeys() []string {
	if schema == nil {
		return nil
	}
	keys := make([]string, len(schema.enum))
	for index, value := range schema.enum {
		keys[index] = value.key
	}
	return keys
}

func (schema *FixedJSONSchema) hasConst() bool {
	return schema != nil && schema.constPresent && schema.constValue != nil
}

func (schema *FixedJSONSchema) additionalPropertiesView() (additionalPropertiesMode, *FixedJSONSchema) {
	if schema == nil || schema.additionalProperties == nil {
		return additionalPropertiesUnspecified, nil
	}
	switch {
	case schema.additionalProperties.booleanValue != nil && *schema.additionalProperties.booleanValue:
		return additionalPropertiesAllowed, nil
	case schema.additionalProperties.booleanValue != nil:
		return additionalPropertiesDenied, nil
	case schema.additionalProperties.schema != nil:
		return additionalPropertiesSchema, schema.additionalProperties.schema
	default:
		return additionalPropertiesUnspecified, nil
	}
}

func (schema *FixedJSONSchema) itemsSchema() (*FixedJSONSchema, bool) {
	if schema == nil || !schema.itemsPresent || schema.items == nil {
		return nil, false
	}
	return schema.items, true
}

func cacheSchemaSemanticKeys(schema *FixedJSONSchema) {
	if schema == nil {
		return
	}
	for _, child := range schema.properties {
		cacheSchemaSemanticKeys(child)
	}
	if schema.additionalProperties != nil {
		cacheSchemaSemanticKeys(schema.additionalProperties.schema)
	}
	cacheSchemaSemanticKeys(schema.items)
	var builder strings.Builder
	appendSchemaSemanticKey(&builder, schema)
	schema.semanticKey = builder.String()
}

func appendSchemaSemanticKey(builder *strings.Builder, schema *FixedJSONSchema) {
	builder.WriteByte('{')
	if schema.schemaType != nil {
		builder.WriteString("T:")
		builder.WriteString(strconv.Quote(string(*schema.schemaType)))
		builder.WriteByte(';')
	}
	if schema.propertiesPresent {
		keys := make([]string, 0, len(schema.properties))
		for key := range schema.properties {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		builder.WriteString("P:{")
		for _, key := range keys {
			builder.WriteString(strconv.Quote(key))
			builder.WriteByte(':')
			builder.WriteString(schema.properties[key].SemanticKey())
			builder.WriteByte(';')
		}
		builder.WriteString("};")
	}
	if schema.requiredPresent {
		values := append([]string(nil), schema.required...)
		sort.Strings(values)
		builder.WriteString("R:[")
		for _, value := range values {
			builder.WriteString(strconv.Quote(value))
			builder.WriteByte(';')
		}
		builder.WriteString("];")
	}
	if schema.additionalProperties != nil {
		switch {
		case schema.additionalProperties.booleanValue != nil:
			builder.WriteString("A:")
			builder.WriteString(strconv.FormatBool(*schema.additionalProperties.booleanValue))
			builder.WriteByte(';')
		case schema.additionalProperties.schema != nil:
			builder.WriteString("A:")
			builder.WriteString(schema.additionalProperties.schema.SemanticKey())
			builder.WriteByte(';')
		}
	}
	if schema.itemsPresent {
		builder.WriteString("I:")
		if schema.items != nil {
			builder.WriteString(schema.items.SemanticKey())
		} else {
			builder.WriteString("invalid")
		}
		builder.WriteByte(';')
	}
	if schema.enumPresent {
		values := make([]string, 0, len(schema.enum))
		for _, value := range schema.enum {
			values = append(values, value.key)
		}
		sort.Strings(values)
		builder.WriteString("E:[")
		for _, value := range values {
			builder.WriteString(strconv.Quote(value))
			builder.WriteByte(';')
		}
		builder.WriteString("];")
	}
	if schema.constPresent {
		builder.WriteString("C:")
		if schema.constValue != nil {
			builder.WriteString(strconv.Quote(schema.constValue.key))
		} else {
			builder.WriteString("invalid")
		}
		builder.WriteByte(';')
	}
	builder.WriteByte('}')
}

func outputContractsCompatible(source, target parsedOutputContract) bool {
	if source.Type != target.Type {
		return false
	}
	if source.Type != ValueJSON {
		return source.Schema == nil && target.Schema == nil
	}
	if target.Schema == nil {
		return true
	}
	if source.Schema == nil {
		return false
	}
	return source.Schema.SemanticKey() == target.Schema.SemanticKey()
}

func validateOutputContractCompatibility(
	report *Report,
	nodeID string,
	path string,
	source parsedOutputContract,
	target parsedOutputContract,
) bool {
	if outputContractsCompatible(source, target) {
		return true
	}
	addSchemaIssue(
		report,
		nodeID,
		path,
		CodeOutputContractIncompatible,
		"source output contract cannot be proven compatible with target",
	)
	return false
}

func validJSONSchemaType(value JSONSchemaType) bool {
	switch value {
	case JSONSchemaNull, JSONSchemaBoolean, JSONSchemaObject, JSONSchemaArray,
		JSONSchemaNumber, JSONSchemaInteger, JSONSchemaString:
		return true
	default:
		return false
	}
}

func sortedAnyKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func prefixedPath(prefix, suffix string) string {
	if suffix == "" {
		return prefix
	}
	return prefix + suffix
}

func addSchemaIssue(report *Report, nodeID, path, code, message string) {
	if nodeID == "" {
		report.Add(PhaseValuesAndSchemas, path, code, message)
		return
	}
	report.AddNode(PhaseValuesAndSchemas, path, nodeID, code, message)
}
