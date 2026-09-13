package teamforge

// deliveryContractSchema exposes structured requirements to the team builder.
// Users describe the desired result; the builder persists these rules before
// publication. They are never generated from an executor's final self-review.
func deliveryContractSchema() map[string]any {
	selector := schemaObject(map[string]any{
		"source":   map[string]any{"type": "string", "enum": []string{"input", "output", "artifact", "literal"}},
		"path":     map[string]any{"type": "string", "description": "JSON pointer; empty selects the complete JSON value"},
		"artifact": map[string]any{"type": "string", "description": "Exact final JSON artifact path, required only for source=artifact"},
		"reduce":   map[string]any{"type": "string", "enum": []string{"count", "sum", "values"}},
		"field":    map[string]any{"type": "string", "description": "JSON pointer within each array item for sum or values"},
		"value":    map[string]any{"description": "Explicit expected JSON value, only for source=literal"},
	}, []string{"source"})
	rule := schemaObject(map[string]any{
		"actual":   selector,
		"operator": map[string]any{"type": "string", "enum": []string{"equals", "less_than", "at_most", "greater_than", "at_least", "set_equals", "unique", "schema"}},
		"expected": selector,
		"schema":   map[string]any{"description": "Local JSON Schema for operator=schema; remote references are forbidden"},
	}, []string{"actual", "operator"})
	check := schemaObject(map[string]any{
		"id":               map[string]any{"type": "string"},
		"title":            map[string]any{"type": "string", "maxLength": 300, "description": "User-readable requirement, e.g. Total equals the sum of the supplied rows"},
		"verifier_id":      map[string]any{"type": "string", "const": "weave.deterministic"},
		"verifier_version": map[string]any{"type": "string", "const": "v1"},
		"parameters":       rule,
	}, []string{"id", "title", "verifier_id", "verifier_version", "parameters"})
	contract := schemaObject(map[string]any{
		"version":         map[string]any{"type": "integer", "const": 1},
		"coverage":        map[string]any{"type": "string", "enum": []string{"explicit", "incomplete"}},
		"output":          schemaObject(map[string]any{"type": map[string]any{"type": "string", "enum": []string{"text", "json", "number", "boolean"}}, "schema": map[string]any{}}, []string{"type"}),
		"required_checks": map[string]any{"type": "array", "maxItems": 128, "items": check},
		"required_artifacts": map[string]any{"type": "array", "items": schemaObject(map[string]any{
			"id": map[string]any{"type": "string"}, "path": map[string]any{"type": "string"}, "content_type": map[string]any{"type": "string"}, "sha256": map[string]any{"type": "string"}, "contains": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		}, []string{"id", "path"})},
		"external_effects":          map[string]any{"type": "string", "enum": []string{"none", "required"}},
		"external_effects_check_id": map[string]any{"type": "string"},
		"limitations":               map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
	}, []string{"version", "coverage", "output"})
	contract["description"] = "Persist before publication. Derive only objectively computable requirements from the user's goal. Actual must select final output or a final JSON artifact; expected may select the original structured run input or an explicit constant. Use sum/count/set_equals/unique for mechanical facts and schema for field constraints. Built-in blueprints output text, so select a final JSON artifact for field checks. A model's PASS/review/confidence is not evidence. Leave noncomputable requirements in limitations; do not invent a computable assertion or mark semantic correctness proved."
	return contract
}
