package frozen

import (
	"errors"
	"fmt"
	"math"
	"slices"
)

// ThinkingModes are the thinking switches a member may request.
var ThinkingModes = []string{"enabled", "disabled"}

// ModelParameters is what a provider declares one of its models accepts. A
// listed field names or bounds the values a member may request; a field left
// out accepts no explicit value. A model without a declaration accepts only a
// reasoning effort, sent unchecked as before declarations existed.
type ModelParameters struct {
	ReasoningEfforts []string        `json:"reasoning_efforts,omitempty"`
	Thinking         []string        `json:"thinking,omitempty"`
	Temperature      *ParameterRange `json:"temperature,omitempty"`
	TopP             *ParameterRange `json:"top_p,omitempty"`
}

// ParameterRange bounds a sampling value, both ends included.
type ParameterRange struct {
	Min float64 `json:"min"`
	Max float64 `json:"max"`
}

// Widest values a declaration may allow: OpenAI-compatible temperature is
// 0..2 and top_p 0..1.
const (
	MaxTemperature = 2
	MaxTopP        = 1
)

// NormalizeModelParameters validates a declaration and orders its lists, so
// the same declaration always freezes to the same bytes.
func NormalizeModelParameters(value ModelParameters) (ModelParameters, error) {
	efforts, err := declaredSubset("reasoning_efforts", value.ReasoningEfforts, ReasoningEfforts)
	if err != nil {
		return ModelParameters{}, err
	}
	thinking, err := declaredSubset("thinking", value.Thinking, ThinkingModes)
	if err != nil {
		return ModelParameters{}, err
	}
	normalized := ModelParameters{ReasoningEfforts: efforts, Thinking: thinking}
	for _, field := range []struct {
		name  string
		value *ParameterRange
		max   float64
		into  **ParameterRange
	}{
		{"temperature", value.Temperature, MaxTemperature, &normalized.Temperature},
		{"top_p", value.TopP, MaxTopP, &normalized.TopP},
	} {
		if field.value == nil {
			continue
		}
		bounds := *field.value
		if !finite(bounds.Min) || !finite(bounds.Max) || bounds.Min < 0 || bounds.Max > field.max || bounds.Min > bounds.Max {
			return ModelParameters{}, fmt.Errorf("%s range must lie within 0..%g with min not above max", field.name, field.max)
		}
		*field.into = &bounds
	}
	return normalized, nil
}

// declaredSubset keeps a declared list in the order of allowed, refusing
// values outside it and repeats.
func declaredSubset(name string, values, allowed []string) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	ordered := make([]string, 0, len(values))
	for _, candidate := range allowed {
		if slices.Contains(values, candidate) {
			ordered = append(ordered, candidate)
		}
	}
	if len(ordered) != len(values) {
		return nil, fmt.Errorf("%s must be distinct values of %v", name, allowed)
	}
	return ordered, nil
}

// ValidateRequestProfileForModel refuses a member request setting the model's
// declaration does not allow, so it is rejected before publication instead
// of being sent to a provider that ignores or rejects it.
func ValidateRequestProfileForModel(profile FrozenRequestProfile, binding FrozenModelBinding) error {
	declared := binding.Parameters
	if declared == nil {
		if profile.Thinking != "" || profile.Temperature != nil || profile.TopP != nil {
			return fmt.Errorf("model %q has no parameter declaration from its provider; thinking, temperature and top_p need one", binding.ModelID)
		}
		return nil
	}
	if profile.ReasoningEffort != "" && !slices.Contains(declared.ReasoningEfforts, profile.ReasoningEffort) {
		return fmt.Errorf("model %q does not declare reasoning_effort %q (declared: %v)", binding.ModelID, profile.ReasoningEffort, declared.ReasoningEfforts)
	}
	if profile.Thinking != "" && !slices.Contains(declared.Thinking, profile.Thinking) {
		return fmt.Errorf("model %q does not declare thinking %q (declared: %v)", binding.ModelID, profile.Thinking, declared.Thinking)
	}
	for _, field := range []struct {
		name   string
		value  *float64
		bounds *ParameterRange
	}{
		{"temperature", profile.Temperature, declared.Temperature},
		{"top_p", profile.TopP, declared.TopP},
	} {
		if field.value == nil {
			continue
		}
		if field.bounds == nil {
			return fmt.Errorf("model %q does not declare %s", binding.ModelID, field.name)
		}
		if *field.value < field.bounds.Min || *field.value > field.bounds.Max {
			return fmt.Errorf("model %q declares %s within %g..%g, not %g", binding.ModelID, field.name, field.bounds.Min, field.bounds.Max, *field.value)
		}
	}
	return nil
}

// ValidateBundleRequestProfile checks a Loom member's request settings against
// every model it may call: its primary model and each fallback.
func ValidateBundleRequestProfile(bundle FrozenExecutionBundle) error {
	profile := bundle.Agent.RequestProfile
	if profile == nil || bundle.Agent.Engine != "loom" {
		return nil
	}
	if bundle.Agent.Model == "" {
		if profile.Thinking != "" || profile.Temperature != nil || profile.TopP != nil {
			return errors.New("a member on the default model has no parameter declaration; choose a model to set thinking, temperature or top_p")
		}
		return nil
	}
	for _, binding := range append([]FrozenModelBinding{bundle.PrimaryModel}, bundle.FallbackModels...) {
		if err := ValidateRequestProfileForModel(*profile, binding); err != nil {
			return err
		}
	}
	return nil
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}
