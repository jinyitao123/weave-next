package teamtemplate

import (
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"
)

// ApplyOverrides replaces fields in a trusted sample document with the JSON
// object supplied by an API client. Maps merge recursively and every other
// value replaces the sample value; CompileYAML remains the sole semantic and
// unknown-field validator for the resulting document.
func ApplyOverrides(sample []byte, overrides map[string]any) ([]byte, error) {
	if len(overrides) == 0 {
		return append([]byte(nil), sample...), nil
	}
	var document map[string]any
	if err := yaml.Unmarshal(sample, &document); err != nil {
		return nil, fmt.Errorf("decode sample YAML: %w", err)
	}
	if document == nil {
		return nil, errors.New("decode sample YAML: object is required")
	}
	mergeTemplateMap(document, overrides)
	encoded, err := yaml.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode overridden sample YAML: %w", err)
	}
	return encoded, nil
}

func mergeTemplateMap(target, overrides map[string]any) {
	for key, override := range overrides {
		targetMap, targetIsMap := target[key].(map[string]any)
		overrideMap, overrideIsMap := override.(map[string]any)
		if targetIsMap && overrideIsMap {
			mergeTemplateMap(targetMap, overrideMap)
			continue
		}
		target[key] = override
	}
}
