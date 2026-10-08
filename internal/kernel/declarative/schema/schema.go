// Package schema is the single source of truth for declarative step config
// fields. It mirrors the actual field reads in the factory build steps
// (internal/declarative/factory.go) plus the frozen v1 config wire
// (internal/declarative/frozen_factory.go), so the write-time graph
// validator (internal/teameval) and the freeze-time encoder consume one
// field list instead of two hand-maintained copies.
//
// Field sets are the anti-drift contract: adding a factory field requires
// updating this file, and both the validator and the freezer pick it up
// automatically. The Required flag encodes the validator's stricter write
// contract (a prompt-less llm step is a broken graph); the freezer only
// consumes the allowed-key sets and stays wire-compatible.
package schema

// Kind enumerates the value shapes the factory reads out of a config map.
type Kind int

const (
	// KindString is a single string (factory getStr).
	KindString Kind = iota
	// KindNumber is an integer (factory getInt).
	KindNumber
	// KindBool is a boolean (factory getBool).
	KindBool
	// KindStringArray is an array of strings (factory getStrSlice).
	KindStringArray
	// KindEnum is a string restricted to Enum values.
	KindEnum
	// KindAny is an arbitrary JSON literal (transform set.value).
	KindAny
	// KindExtract is the llm_call extract object.
	KindExtract
	// KindOperations is the transform operations array.
	KindOperations
)

// Field describes one config key the factory reads for a step type.
type Field struct {
	Key      string
	Kind     Kind
	Enum     []string
	Required bool
}

// Step describes the config schema for one declarative step type.
type Step struct {
	Type   string
	Fields []Field
}

// AllowedKeys returns the frozen-v1 allowed key set for the step: every
// field the factory or the router reads, with nothing extra.
func (s Step) AllowedKeys() []string {
	keys := make([]string, 0, len(s.Fields))
	for _, field := range s.Fields {
		keys = append(keys, field.Key)
	}
	return keys
}

// StepFor returns the config schema for one declarative step type. The
// worker step is deliberately absent: employee internal graphs forbid it
// and frozen v1 does not support it.
func StepFor(stepType string) (Step, bool) {
	switch stepType {
	case "chat":
		return Step{
			Type: "chat",
			Fields: []Field{
				{Key: "model", Kind: KindString},
				{Key: "system_prompt", Kind: KindString},
				{Key: "max_iterations", Kind: KindNumber},
				{Key: "max_loops", Kind: KindNumber},
			},
		}, true
	case "llm_call":
		return Step{
			Type: "llm_call",
			Fields: []Field{
				{Key: "model", Kind: KindString},
				{Key: "prompt_template", Kind: KindString, Required: true},
				{Key: "input_keys", Kind: KindStringArray},
				{Key: "output_key", Kind: KindString},
				{Key: "stream", Kind: KindBool},
				{Key: "max_loops", Kind: KindNumber},
				{Key: "extract", Kind: KindExtract},
			},
		}, true
	case "llm_check":
		return Step{
			Type: "llm_check",
			Fields: []Field{
				{Key: "model", Kind: KindString},
				{Key: "prompt_template", Kind: KindString, Required: true},
				{Key: "input_keys", Kind: KindStringArray},
				{Key: "output_key", Kind: KindString, Required: true},
				{Key: "extract_mode", Kind: KindEnum, Enum: []string{"json", "keyword"}},
				{Key: "json_field", Kind: KindString},
				{Key: "keywords_true", Kind: KindStringArray},
				{Key: "max_loops", Kind: KindNumber},
			},
		}, true
	case "yield":
		return Step{
			Type: "yield",
			Fields: []Field{
				{Key: "yield_type", Kind: KindString},
				{Key: "max_loops", Kind: KindNumber},
			},
		}, true
	case "transform":
		return Step{
			Type: "transform",
			Fields: []Field{
				{Key: "operations", Kind: KindOperations, Required: true},
				{Key: "max_loops", Kind: KindNumber},
			},
		}, true
	case "builtin":
		return Step{
			Type: "builtin",
			Fields: []Field{
				{Key: "builtin_type", Kind: KindEnum, Enum: []string{"guard", "prompt_assemble", "memory_retrieve"}, Required: true},
				{Key: "max_loops", Kind: KindNumber},
			},
		}, true
	default:
		return Step{}, false
	}
}

// Extract returns the nested llm_call extract schema. The factory only
// implements keyword scanning (extractBool) and the YES/true default; the
// default branch is exposed as the "yes_no" mode name the design contract
// documents, so the validator refuses invented modes like "json".
func Extract() Step {
	return Step{
		Type: "extract",
		Fields: []Field{
			{Key: "key", Kind: KindString, Required: true},
			{Key: "mode", Kind: KindEnum, Enum: []string{"keyword", "yes_no"}, Required: true},
			{Key: "keywords_true", Kind: KindStringArray},
		},
	}
}

// OperationFor returns the transform operation schema for one op kind.
func OperationFor(op string) (Step, bool) {
	switch op {
	case "set":
		return Step{
			Type: "set",
			Fields: []Field{
				{Key: "op", Kind: KindString},
				{Key: "target", Kind: KindString, Required: true},
				{Key: "value", Kind: KindAny, Required: true},
			},
		}, true
	case "concat":
		return Step{
			Type: "concat",
			Fields: []Field{
				{Key: "op", Kind: KindString},
				{Key: "target", Kind: KindString, Required: true},
				{Key: "keys", Kind: KindStringArray, Required: true},
				{Key: "separator", Kind: KindString},
			},
		}, true
	case "copy":
		return Step{
			Type: "copy",
			Fields: []Field{
				{Key: "op", Kind: KindString},
				{Key: "target", Kind: KindString, Required: true},
				{Key: "source", Kind: KindString, Required: true},
			},
		}, true
	default:
		return Step{}, false
	}
}
