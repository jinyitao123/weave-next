package mcphost

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jinyitao123/loom/contract"
	"github.com/lattice-substrate/json-canon/jcstoken"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	maxToolSchemaBytes  = 256 << 10
	maxToolArgsBytes    = 1 << 20
	maxToolSchemas      = 1024
	maxToolCatalogBytes = 8 << 20
)

// ToolContract compiles one immutable, server-scoped tool catalog. It validates
// arguments without modifying them. Callers own publication and access identity.
type ToolContract struct {
	schemas map[string]*jsonschema.Schema
}

func NewToolContract(tools []contract.ToolDef) (*ToolContract, error) {
	if len(tools) > maxToolSchemas {
		return nil, fmt.Errorf("%w: mcp_tool_catalog_limit", ErrFailClosed)
	}
	result := &ToolContract{schemas: make(map[string]*jsonschema.Schema, len(tools))}
	total := 0
	for _, tool := range tools {
		if tool.Name == "" || strings.TrimSpace(tool.Name) != tool.Name {
			return nil, fmt.Errorf("%w: mcp_tool_name_invalid", ErrFailClosed)
		}
		if _, exists := result.schemas[tool.Name]; exists {
			return nil, fmt.Errorf("%w: mcp_tool_name_repeated", ErrFailClosed)
		}
		total += len(tool.InputSchema)
		if total > maxToolCatalogBytes {
			return nil, fmt.Errorf("%w: mcp_tool_catalog_limit", ErrFailClosed)
		}
		value, err := parseToolJSON(tool.InputSchema, maxToolSchemaBytes)
		if err != nil {
			return nil, fmt.Errorf("%w: mcp_tool_schema_invalid", ErrFailClosed)
		}
		if _, ok := value.(map[string]any); !ok {
			return nil, fmt.Errorf("%w: mcp_tool_schema_not_object", ErrFailClosed)
		}
		compiler := jsonschema.NewCompiler()
		compiler.DefaultDraft(jsonschema.Draft2020)
		compiler.AssertFormat()
		compiler.UseLoader(nil)
		// Each compiler has exactly one supplied resource; neither file nor HTTP
		// references can read ambient resources. Local references still resolve.
		const resource = "https://weave.invalid/mcp-tool-schema"
		if err := compiler.AddResource(resource, value); err != nil {
			return nil, fmt.Errorf("%w: mcp_tool_schema_invalid", ErrFailClosed)
		}
		schema, err := compiler.Compile(resource)
		if err != nil {
			return nil, fmt.Errorf("%w: mcp_tool_schema_unavailable", ErrFailClosed)
		}
		result.schemas[tool.Name] = schema
	}
	return result, nil
}

// Validate returns a bounded tool error for a correctable argument failure.
// It never returns schema error text or argument values supplied by the caller.
func (c *ToolContract) Validate(call contract.ToolCall) *contract.ToolResult {
	if c == nil || c.schemas[call.Name] == nil {
		return toolArgumentError(call.ID, "mcp_tool_not_bound", "")
	}
	value, err := parseToolJSON([]byte(call.Args), maxToolArgsBytes)
	if err != nil {
		return toolArgumentError(call.ID, "mcp_arguments_invalid_json", "")
	}
	if _, ok := value.(map[string]any); !ok {
		return toolArgumentError(call.ID, "mcp_arguments_not_object", "")
	}
	if err := c.schemas[call.Name].Validate(value); err != nil {
		path := ""
		var failure *jsonschema.ValidationError
		if errors.As(err, &failure) {
			for len(failure.Causes) > 0 {
				failure = failure.Causes[0]
			}
			for _, part := range failure.InstanceLocation {
				path += "/" + strings.ReplaceAll(strings.ReplaceAll(part, "~", "~0"), "/", "~1")
			}
			if len(path) > 160 {
				path = ""
			}
		}
		return toolArgumentError(call.ID, "mcp_arguments_schema_mismatch", path)
	}
	return nil
}

func toolArgumentError(id, code, path string) *contract.ToolResult {
	body, _ := json.Marshal(struct {
		Code string `json:"code"`
		Path string `json:"path,omitempty"`
	}{code, path})
	return &contract.ToolResult{CallID: id, IsError: true, Content: string(body)}
}

func parseToolJSON(raw []byte, maxBytes int) (any, error) {
	// Reuse the strict tokenizer for duplicate keys, Unicode and resource
	// bounds. Its numeric tree is discarded: schema semantics use json.Number
	// decoded from the original bytes, never the tokenizer's float64 values.
	// This admission policy retains the existing finite I-JSON domain, including
	// rejection of negative zero and under/overflow; it does not canonicalize.
	if _, err := jcstoken.ParseWithOptions(raw, &jcstoken.Options{
		MaxInputSize: maxBytes, MaxDepth: 64, MaxValues: 16384,
		MaxObjectMembers: 4096, MaxArrayElements: 4096,
		MaxStringBytes: maxBytes, MaxNumberChars: 128,
	}); err != nil {
		return nil, errors.New("tool JSON is invalid or exceeds resource limits")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, errors.New("tool JSON is invalid")
	}
	return value, nil
}

// RedactedToolError preserves only fixed, public argument error codes. Paths,
// extra fields and upstream error text cannot cross a redacting gateway.
func RedactedToolError(content string) string {
	if len(content) <= 512 {
		var failure struct {
			Code string `json:"code"`
		}
		if json.Unmarshal([]byte(content), &failure) == nil {
			switch failure.Code {
			case "mcp_tool_not_bound", "mcp_arguments_invalid_json", "mcp_arguments_not_object", "mcp_arguments_schema_mismatch":
				safe, _ := json.Marshal(failure)
				return string(safe)
			}
		}
	}
	return "upstream MCP error"
}
