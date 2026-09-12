package capabilities

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

func validateInputContract(contractRaw, input json.RawMessage) error {
	var contract machine.OutputContract
	if err := decodeExactJSON(contractRaw, &contract); err != nil ||
		contract.Type != machine.ValueJSON || len(contract.Schema) == 0 {
		return fmt.Errorf("%w: frozen input contract is invalid", ErrReleaseUnsafe)
	}
	_, problems := machine.ValidateRuntimeInput(contract.Schema, input)
	if len(problems) == 0 {
		return nil
	}
	return &SchemaViolationError{Problems: publicSchemaProblems(problems)}
}

func validateOutputContract(contractRaw, output json.RawMessage) []SchemaProblem {
	var contract machine.OutputContract
	if err := decodeExactJSON(contractRaw, &contract); err != nil ||
		contract.Type != machine.ValueJSON || len(contract.Schema) == 0 {
		return []SchemaProblem{{Code: "contract_invalid"}}
	}
	_, problems := machine.ValidateRuntimeOutput(contract, output)
	return publicSchemaProblems(problems)
}

func publicSchemaProblems(problems []machine.RuntimeSchemaProblem) []SchemaProblem {
	result := make([]SchemaProblem, 0, len(problems))
	for _, problem := range problems {
		// Messages are deliberately omitted from the service DTO. Codes and
		// JSON-pointer paths are stable and do not expose internal workflow text.
		result = append(result, SchemaProblem{Path: problem.Path, Code: problem.Code})
	}
	return result
}

func validateValueResources(raw json.RawMessage, limits ExecutionLimits, output bool) error {
	byteLimit := limits.MaxInputBytes
	byteName := "max_input_bytes"
	if output {
		byteLimit = limits.MaxOutputBytes
		byteName = "max_output_bytes"
	}
	if len(raw) > byteLimit {
		return &ResourceLimitError{Limit: byteName}
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("%w: JSON value is invalid", ErrInvalid)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing JSON value", ErrInvalid)
	}
	stats := resourceStats{}
	if err := inspectValueResources(value, 1, limits, &stats); err != nil {
		return err
	}
	return nil
}

type resourceStats struct {
	objectFields int
	arrayItems   int
}

func inspectValueResources(value any, depth int, limits ExecutionLimits, stats *resourceStats) error {
	if depth > limits.MaxNestingDepth {
		return &ResourceLimitError{Limit: "max_nesting_depth"}
	}
	switch typed := value.(type) {
	case map[string]any:
		stats.objectFields += len(typed)
		if stats.objectFields > limits.MaxObjectFields {
			return &ResourceLimitError{Limit: "max_object_fields"}
		}
		for key, child := range typed {
			if len([]byte(key)) > limits.MaxStringBytes {
				return &ResourceLimitError{Limit: "max_string_bytes"}
			}
			if err := inspectValueResources(child, depth+1, limits, stats); err != nil {
				return err
			}
		}
	case []any:
		stats.arrayItems += len(typed)
		if stats.arrayItems > limits.MaxArrayItems {
			return &ResourceLimitError{Limit: "max_array_items"}
		}
		for _, child := range typed {
			if err := inspectValueResources(child, depth+1, limits, stats); err != nil {
				return err
			}
		}
	case string:
		if len([]byte(typed)) > limits.MaxStringBytes {
			return &ResourceLimitError{Limit: "max_string_bytes"}
		}
	}
	return nil
}

func decodeExactJSON(raw json.RawMessage, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON value")
	}
	return nil
}
