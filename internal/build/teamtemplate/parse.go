package teamtemplate

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

type yamlShape struct {
	fields   map[string]*yamlShape
	element  *yamlShape
	allowAny bool
}

var templateYAMLShape = &yamlShape{fields: map[string]*yamlShape{
	"schema": nil, "name": nil, "display_name": nil, "purpose": nil, "template": nil,
	"template_parameters": {fields: map[string]*yamlShape{
		"lead_instruction": nil, "primary_ref": nil, "reviewer_ref": nil,
		"parallel_worker_refs": {element: nil}, "finalizer_ref": nil,
		"max_iterations": nil, "result_requirements": {allowAny: true},
	}},
	"members": {element: &yamlShape{fields: map[string]*yamlShape{
		"name": nil, "display_name": nil, "role": nil,
		"responsibilities": {element: nil}, "capabilities": {element: nil},
		"model_ref": nil, "execution_policy": {fields: map[string]*yamlShape{
			"engine_class": nil, "engine": nil, "execution_mode": nil,
			"runtime_ref": nil, "internal_graph_ref": nil,
		}},
	}}},
	"lead":     nil,
	"delivery": {fields: map[string]*yamlShape{"success_criteria": {element: nil}}},
	"budget":   {fields: map[string]*yamlShape{"max_cost_usd": nil}},
}}

// ParseYAML strictly parses one bounded team-template/v1 YAML document.
// It rejects aliases, excessive nesting, unknown fields, and extra documents
// before schema validation.
func ParseYAML(data []byte) (Template, error) {
	var zero Template
	if len(data) > MaxYAMLBytes {
		return zero, &ValidationError{Problems: []Problem{{
			Path: "/", Code: "template_yaml_too_large",
			Message: fmt.Sprintf("YAML must not exceed %d bytes", MaxYAMLBytes),
		}}}
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return zero, &ValidationError{Problems: []Problem{{Path: "/", Code: "template_yaml_empty", Message: "YAML document is required"}}}
	}

	var document yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&document); err != nil {
		return zero, yamlDecodeError(err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return zero, yamlDecodeError(err)
		}
		return zero, &ValidationError{Problems: []Problem{{Path: "/", Code: "template_yaml_multiple_documents", Message: "exactly one YAML document is allowed"}}}
	}

	c := &problemCollector{}
	scanYAMLNode(c, &document, 0)
	if len(document.Content) == 1 {
		checkYAMLShape(c, document.Content[0], templateYAMLShape, "")
	}
	if err := c.err(); err != nil {
		return zero, err
	}

	typed := yaml.NewDecoder(bytes.NewReader(data))
	typed.KnownFields(true)
	var result Template
	if err := typed.Decode(&result); err != nil {
		return zero, yamlDecodeError(err)
	}
	return result, nil
}

func yamlDecodeError(err error) error {
	message := strings.TrimSpace(err.Error())
	return &ValidationError{Problems: []Problem{{Path: "/", Code: "template_yaml_invalid", Message: message}}}
}

func scanYAMLNode(c *problemCollector, node *yaml.Node, depth int) {
	if node == nil {
		return
	}
	if node.Kind == yaml.AliasNode {
		c.add("/", "template_yaml_alias_forbidden", "YAML aliases are not allowed")
	}
	if node.Kind == yaml.MappingNode || node.Kind == yaml.SequenceNode {
		depth++
		if depth > MaxYAMLDepth {
			c.add("/", "template_yaml_depth_exceeded", fmt.Sprintf("YAML nesting depth must not exceed %d", MaxYAMLDepth))
			return
		}
	}
	for _, child := range node.Content {
		scanYAMLNode(c, child, depth)
	}
}

func checkYAMLShape(c *problemCollector, node *yaml.Node, shape *yamlShape, path string) {
	if node == nil || shape == nil || shape.allowAny {
		return
	}
	if shape.fields != nil {
		if node.Kind != yaml.MappingNode {
			return
		}
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			name := key.Value
			childPath := path + "/" + jsonPointerSegment(name)
			childShape, ok := shape.fields[name]
			if !ok {
				c.add(childPath, "template_yaml_unknown_field", "unknown field")
				continue
			}
			checkYAMLShape(c, value, childShape, childPath)
		}
		return
	}
	if shape.element != nil && node.Kind == yaml.SequenceNode {
		for i, child := range node.Content {
			checkYAMLShape(c, child, shape.element, fmt.Sprintf("%s/%d", path, i))
		}
	}
}
