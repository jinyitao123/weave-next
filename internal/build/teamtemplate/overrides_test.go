package teamtemplate

import (
	"errors"
	"testing"
)

func TestApplyOverridesMergesOnlyTemplateFields(t *testing.T) {
	overridden, err := ApplyOverrides([]byte(validResearchTemplateYAML), map[string]any{
		"display_name": "新版调研团队",
		"budget":       map[string]any{"max_cost_usd": 4.0},
	})
	if err != nil {
		t.Fatalf("ApplyOverrides() error = %v", err)
	}
	compiled, err := CompileYAML(overridden)
	if err != nil {
		t.Fatalf("CompileYAML() error = %v", err)
	}
	if compiled.Template.DisplayName != "新版调研团队" || compiled.Brief.TotalBudget.MaxCostUSD != 4 {
		t.Fatalf("compiled override = %#v", compiled.Template)
	}
}

func TestApplyOverridesLeavesUnknownFieldsForStrictCompiler(t *testing.T) {
	overridden, err := ApplyOverrides([]byte(validResearchTemplateYAML), map[string]any{"invented_semantic": true})
	if err != nil {
		t.Fatalf("ApplyOverrides() error = %v", err)
	}
	_, err = CompileYAML(overridden)
	var validation *ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("CompileYAML() error = %v, want unknown field problem", err)
	}
	for _, problem := range validation.Problems {
		if problem.Path == "/invented_semantic" {
			return
		}
	}
	t.Fatalf("CompileYAML() problems = %#v, want unknown field path", validation.Problems)
}
