package frozen

import (
	"encoding/json"
	"errors"
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
	input, err := strictDecode[StandardFactoryInputV2](raw)
	if err != nil {
		return StandardFactoryInputV2{}, err
	}
	if input.SchemaVersion != 2 || input.MCPServers == nil {
		return StandardFactoryInputV2{}, errors.New("standard v2 input requires schema_version 2 and mcp_servers")
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
		var err error
		if tool.InputSchema, err = canonicalRequiredJSONObject(tool.InputSchema); err != nil {
			return nil, err
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
