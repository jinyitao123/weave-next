package frozen

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func floatPointer(value float64) *float64 { return &value }

// A declaration freezes in one canonical order and refuses values that no
// member could be asked for.
func TestModelParametersNormalizeAndRefuse(t *testing.T) {
	normalized, err := NormalizeModelParameters(ModelParameters{
		ReasoningEfforts: []string{"max", "low", "high"}, Thinking: []string{"disabled", "enabled"},
		Temperature: &ParameterRange{Min: 0, Max: 1}, TopP: &ParameterRange{Min: 0.1, Max: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(normalized.ReasoningEfforts, []string{"low", "high", "max"}) ||
		!reflect.DeepEqual(normalized.Thinking, []string{"enabled", "disabled"}) {
		t.Fatalf("normalized = %+v", normalized)
	}
	for name, value := range map[string]ModelParameters{
		"unknown effort":      {ReasoningEfforts: []string{"extreme"}},
		"repeated effort":     {ReasoningEfforts: []string{"low", "low"}},
		"unknown thinking":    {Thinking: []string{"auto"}},
		"temperature above 2": {Temperature: &ParameterRange{Min: 0, Max: 2.5}},
		"top_p above 1":       {TopP: &ParameterRange{Min: 0, Max: 1.5}},
		"inverted range":      {Temperature: &ParameterRange{Min: 1, Max: 0.5}},
		"negative range":      {TopP: &ParameterRange{Min: -0.1, Max: 1}},
	} {
		if _, err := NormalizeModelParameters(value); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// Thinking, temperature and top_p are frozen with the member, kept apart from
// unset (an explicit 0 included), and bounded before any model is consulted.
func TestRequestProfileFreezesThinkingAndSampling(t *testing.T) {
	base := FrozenAgentRecord{SchemaVersion: 1, WorkspaceID: "ws", AgentID: "a", AgentVersion: 1, Name: "a",
		Profiles: map[string]FrozenAgentProfile{}, OutputSchema: []byte("null"), FactoryInput: []byte("{}")}
	plain, err := Canonicalize(base, PreorderFrozenAgentRecord)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{string(plain): "none"}
	for name, profile := range map[string]FrozenRequestProfile{
		"temperature 0":   {Temperature: floatPointer(0)},
		"temperature 0.7": {Temperature: floatPointer(0.7)},
		"top_p 0.9":       {TopP: floatPointer(0.9)},
		"thinking":        {Thinking: "enabled"},
	} {
		record := base
		record.RequestProfile = &profile
		encoded, err := Canonicalize(record, PreorderFrozenAgentRecord)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if other, ok := seen[string(encoded)]; ok {
			t.Fatalf("%s froze like %s", name, other)
		}
		seen[string(encoded)] = name
	}
	for _, tc := range []struct {
		profile FrozenRequestProfile
		valid   bool
	}{
		{FrozenRequestProfile{Thinking: "disabled", ReasoningEffort: "none"}, true},
		{FrozenRequestProfile{Thinking: "enabled", ReasoningEffort: "high", Temperature: floatPointer(2), TopP: floatPointer(0)}, true},
		{FrozenRequestProfile{Thinking: "auto"}, false},
		{FrozenRequestProfile{Thinking: "disabled", ReasoningEffort: "low"}, false},
		{FrozenRequestProfile{Temperature: floatPointer(2.1)}, false},
		{FrozenRequestProfile{TopP: floatPointer(-0.1)}, false},
	} {
		if err := ValidateRequestProfile(tc.profile); (err == nil) != tc.valid {
			t.Errorf("%+v: err=%v, want valid=%v", tc.profile, err, tc.valid)
		}
	}
}

// A model's declaration enters its binding's hash only when there is one.
func TestFrozenModelBindingHashCoversADeclarationOnlyWhenPresent(t *testing.T) {
	plain := protocolTestBinding("")
	canonical, err := Canonicalize(plain, PreorderFrozenModelBinding)
	if err != nil || bytes.Contains(canonical, []byte("model_parameters")) {
		t.Fatalf("an undeclared binding changed its canonical bytes: %v %s", err, canonical)
	}
	declared := protocolTestBinding("")
	declared.Parameters = &ModelParameters{ReasoningEfforts: []string{"high", "low"}}
	reordered := protocolTestBinding("")
	reordered.Parameters = &ModelParameters{ReasoningEfforts: []string{"low", "high"}}
	plainHash, _ := HashDTO(plain, PreorderFrozenModelBinding)
	declaredHash, err := HashDTO(declared, PreorderFrozenModelBinding)
	if err != nil || declaredHash == plainHash {
		t.Fatalf("a declaration is not part of the binding hash: %v", err)
	}
	if reorderedHash, _ := HashDTO(reordered, PreorderFrozenModelBinding); reorderedHash != declaredHash {
		t.Fatal("the same declaration in another order hashed apart")
	}
	systemOne := protocolTestBinding(ModelProtocolSystemOne)
	systemOne.Parameters = &ModelParameters{}
	if _, err := HashDTO(systemOne, PreorderFrozenModelBinding); err == nil {
		t.Fatal("a systemone binding accepted a declaration")
	}
}

// A member's settings are checked against every model it may call; a model
// without a declaration takes only a reasoning effort, as before.
func TestBundleRequestProfileIsCheckedAgainstEveryModel(t *testing.T) {
	declared := protocolTestBinding("")
	declared.ModelID = "declared"
	declared.Parameters = &ModelParameters{ReasoningEfforts: []string{"low", "high"}, Thinking: []string{"enabled"},
		Temperature: &ParameterRange{Min: 0, Max: 1}}
	undeclared := protocolTestBinding("")
	undeclared.ModelID = "undeclared"
	bundle := func(profile FrozenRequestProfile, model string, fallbacks ...FrozenModelBinding) FrozenExecutionBundle {
		primary := declared
		if model == "undeclared" {
			primary = undeclared
		}
		return FrozenExecutionBundle{Agent: FrozenAgentRecord{Engine: "loom", Model: model, RequestProfile: &profile},
			PrimaryModel: primary, FallbackModels: fallbacks}
	}
	for _, tc := range []struct {
		name   string
		bundle FrozenExecutionBundle
		refuse string
	}{
		{"declared values", bundle(FrozenRequestProfile{ReasoningEffort: "high", Thinking: "enabled", Temperature: floatPointer(0.5)}, "declared"), ""},
		{"effort on an undeclared model", bundle(FrozenRequestProfile{ReasoningEffort: "medium"}, "undeclared"), ""},
		{"undeclared effort value", bundle(FrozenRequestProfile{ReasoningEffort: "max"}, "declared"), `reasoning_effort "max"`},
		{"undeclared thinking mode", bundle(FrozenRequestProfile{Thinking: "disabled"}, "declared"), `thinking "disabled"`},
		{"temperature outside the range", bundle(FrozenRequestProfile{Temperature: floatPointer(1.5)}, "declared"), "temperature within 0..1"},
		{"top_p not declared", bundle(FrozenRequestProfile{TopP: floatPointer(0.5)}, "declared"), "does not declare top_p"},
		{"thinking on an undeclared model", bundle(FrozenRequestProfile{Thinking: "enabled"}, "undeclared"), "no parameter declaration"},
		{"undeclared fallback", bundle(FrozenRequestProfile{Temperature: floatPointer(0.5)}, "declared", undeclared), `model "undeclared"`},
		{"default model", FrozenExecutionBundle{Agent: FrozenAgentRecord{Engine: "loom", RequestProfile: &FrozenRequestProfile{TopP: floatPointer(0.5)}}}, "default model"},
	} {
		err := ValidateBundleRequestProfile(tc.bundle)
		if tc.refuse == "" && err != nil {
			t.Errorf("%s refused: %v", tc.name, err)
		}
		if tc.refuse != "" && (err == nil || !strings.Contains(err.Error(), tc.refuse)) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.refuse)
		}
	}
}
