package credentials

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/jinyitao123/weave/internal/base/frozen"
)

// modelParametersJSON is the stored form of a provider's declarations: an
// object keyed by model ID, empty when none are declared.
func modelParametersJSON(values map[string]frozen.ModelParameters) (string, error) {
	if len(values) == 0 {
		return "{}", nil
	}
	raw, err := json.Marshal(values)
	return string(raw), err
}

func decodeModelParameters(raw []byte) (map[string]frozen.ModelParameters, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var values map[string]frozen.ModelParameters
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, err
	}
	if len(values) == 0 {
		return nil, nil
	}
	return values, nil
}

// normalizeModelParameters checks that every declaration names a model the
// provider serves on a protocol that takes them, and orders each one, so the
// same declarations always hash the same.
func normalizeModelParameters(protocol string, models []string, values map[string]frozen.ModelParameters) (map[string]frozen.ModelParameters, error) {
	if len(values) == 0 {
		return nil, nil
	}
	if protocol != "" {
		return nil, fmt.Errorf("model_parameters are declared only for OpenAI-compatible providers")
	}
	normalized := make(map[string]frozen.ModelParameters, len(values))
	for model, declared := range values {
		if !slices.Contains(models, model) {
			return nil, fmt.Errorf("model_parameters names %q, which the provider does not serve", model)
		}
		value, err := frozen.NormalizeModelParameters(declared)
		if err != nil {
			return nil, fmt.Errorf("model_parameters for %q: %w", model, err)
		}
		normalized[model] = value
	}
	return normalized, nil
}

func sameModelParameters(left, right map[string]frozen.ModelParameters) bool {
	a, errA := modelParametersJSON(left)
	b, errB := modelParametersJSON(right)
	return errA == nil && errB == nil && a == b
}

// declaredModelParameters is the declaration a revision makes for one model,
// nil when it makes none.
func declaredModelParameters(values map[string]frozen.ModelParameters, modelID string) *frozen.ModelParameters {
	declared, ok := values[modelID]
	if !ok {
		return nil
	}
	return &declared
}
