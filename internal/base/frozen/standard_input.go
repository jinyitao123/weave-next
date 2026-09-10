package frozen

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
)

// StandardFactoryInputV2 preserves declared MCP access in the published agent.
// Resolved revisions and tool definitions live in the bundle's MCP bindings.
type StandardFactoryInputV2 struct {
	SchemaVersion int                      `json:"schema_version"`
	MCPServers    []StandardMCPDeclaration `json:"mcp_servers"`
}

type StandardMCPDeclaration struct {
	ServerID   string   `json:"server_id"`
	Filter     []string `json:"filter"`
	WriteTools []string `json:"write_tools"`
}

func DecodeStandardFactoryInputV2(raw []byte) (StandardFactoryInputV2, error) {
	return decodeStandardFactoryInput(raw, 2)
}

// DecodeStandardFactoryInputV3 preserves CLI MCP declarations independently of
// the Loom journal contract carried by version 2.
func DecodeStandardFactoryInputV3(raw []byte) (StandardFactoryInputV2, error) {
	return decodeStandardFactoryInput(raw, 3)
}

func decodeStandardFactoryInput(raw []byte, version int) (StandardFactoryInputV2, error) {
	input, err := strictDecode[StandardFactoryInputV2](raw)
	if err != nil {
		return StandardFactoryInputV2{}, err
	}
	if input.SchemaVersion != version || input.MCPServers == nil {
		return StandardFactoryInputV2{}, fmt.Errorf("standard input requires schema_version %d and mcp_servers", version)
	}
	for index := range input.MCPServers {
		server := &input.MCPServers[index]
		if server.ServerID == "" || strings.TrimSpace(server.ServerID) != server.ServerID {
			return StandardFactoryInputV2{}, errors.New("standard MCP server identity is invalid")
		}
		if server.Filter, err = standardDeclarationStringSet(server.Filter); err != nil {
			return StandardFactoryInputV2{}, err
		}
		if server.WriteTools, err = standardDeclarationStringSet(server.WriteTools); err != nil {
			return StandardFactoryInputV2{}, err
		}
	}
	sort.Slice(input.MCPServers, func(i, j int) bool { return input.MCPServers[i].ServerID < input.MCPServers[j].ServerID })
	for index := 1; index < len(input.MCPServers); index++ {
		if input.MCPServers[index-1].ServerID == input.MCPServers[index].ServerID {
			return StandardFactoryInputV2{}, errors.New("standard MCP server identity is repeated")
		}
	}
	return input, nil
}

// FrozenToolDefinition is the public callable contract discovered for one tool.
// It contains no credentials, results, or model state.
type FrozenToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	ReadOnly    bool            `json:"read_only"`
}

func NormalizeToolDefinitions(tools []FrozenToolDefinition) ([]FrozenToolDefinition, error) {
	if tools == nil {
		return nil, nil
	}
	result := append([]FrozenToolDefinition{}, tools...)
	for index := range result {
		tool := &result[index]
		if tool.Name == "" || strings.TrimSpace(tool.Name) != tool.Name {
			return nil, errors.New("frozen tool name is invalid")
		}
		original := append([]byte(nil), tool.InputSchema...)
		var err error
		if tool.InputSchema, err = canonicalRequiredJSONObject(tool.InputSchema); err != nil {
			return nil, err
		}
		if !sameJSONNumbers(original, tool.InputSchema) {
			return nil, errors.New("frozen tool schema numbers cannot be represented without precision loss")
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	for index := 1; index < len(result); index++ {
		if result[index-1].Name == result[index].Name {
			return nil, errors.New("frozen tool name is repeated")
		}
	}
	return result, nil
}

// New input contracts use a stable empty-array representation without changing
// legacy DTO normalization or already-published v1 bytes.
func standardDeclarationStringSet(values []string) ([]string, error) {
	normalized, err := canonicalStringSet(values)
	if err != nil {
		return nil, err
	}
	if normalized == nil {
		normalized = []string{}
	}
	return normalized, nil
}

// JSON Schema numbers carry exact comparison semantics. JCS uses IEEE-754:
// reject contracts whose canonical spelling would change those semantics.
func sameJSONNumbers(before, after []byte) bool {
	decode := func(raw []byte) (any, error) {
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		var value any
		err := d.Decode(&value)
		return value, err
	}
	a, err := decode(before)
	if err != nil {
		return false
	}
	b, err := decode(after)
	if err != nil {
		return false
	}
	var equal func(any, any) bool
	equal = func(a, b any) bool {
		switch value := a.(type) {
		case json.Number:
			other, ok := b.(json.Number)
			if !ok {
				return false
			}
			x, ok := new(big.Rat).SetString(string(value))
			if !ok {
				return false
			}
			y, ok := new(big.Rat).SetString(string(other))
			return ok && x.Cmp(y) == 0
		case map[string]any:
			other, ok := b.(map[string]any)
			if !ok || len(value) != len(other) {
				return false
			}
			for key, child := range value {
				if !equal(child, other[key]) {
					return false
				}
			}
		case []any:
			other, ok := b.([]any)
			if !ok || len(value) != len(other) {
				return false
			}
			for i, child := range value {
				if !equal(child, other[i]) {
					return false
				}
			}
		}
		return true
	}
	return equal(a, b)
}

// Decode admission checks schemas before the envelope's JCS spelling can hide
// precision loss. Existing canonical artifact bytes remain unchanged.
func toolSchemaNumbersPreserved(before, after []byte) bool {
	before = bytes.TrimSpace(before)
	if len(before) == 0 {
		return true
	}
	switch before[0] {
	case '{':
		var a, b map[string]json.RawMessage
		if json.Unmarshal(before, &a) != nil || json.Unmarshal(after, &b) != nil {
			return false
		}
		for key, value := range a {
			if key == "input_schema" {
				if !sameJSONNumbers(value, b[key]) {
					return false
				}
			} else if !toolSchemaNumbersPreserved(value, b[key]) {
				return false
			}
		}
	case '[':
		var a, b []json.RawMessage
		if json.Unmarshal(before, &a) != nil || json.Unmarshal(after, &b) != nil || len(a) != len(b) {
			return false
		}
		for i, value := range a {
			if !toolSchemaNumbersPreserved(value, b[i]) {
				return false
			}
		}
	}
	return true
}
