package frozen

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

func TestStandardV2PreservesLegacyCanonicalBytes(t *testing.T) {
	raw, err := os.ReadFile("testdata/standard-v1-published.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Input     FrozenExecutionBundle `json:"input"`
		Canonical string                `json:"canonical"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	canonical, err := Canonicalize(fixture.Input, PreorderFrozenExecutionBundle)
	if err != nil {
		t.Fatal(err)
	}
	if string(canonical) != fixture.Canonical {
		t.Fatal("legacy frozen bundle canonical bytes changed")
	}
}

func TestStandardV2InputRejectsUnknownFieldsAndNormalizesDeclarations(t *testing.T) {
	for _, raw := range []string{
		`{"schema_version":2,"mcp_servers":[],"headers":{"secret":"x"}}`,
		`{"schema_version":3,"mcp_servers":[]}`,
		`{"schema_version":2,"mcp_servers":null}`,
		`{"schema_version":2,"mcp_servers":[{"server_id":"s"},{"server_id":"s"}]}`,
	} {
		if _, err := DecodeStandardFactoryInputV2([]byte(raw)); err == nil {
			t.Fatalf("accepted invalid input %s", raw)
		}
	}
	input, err := DecodeStandardFactoryInputV2([]byte(`{"schema_version":2,"mcp_servers":[{"server_id":"z","filter":["b","a"]},{"server_id":"a"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if input.MCPServers[0].ServerID != "a" || len(input.MCPServers[1].Filter) != 2 || input.MCPServers[1].Filter[0] != "a" {
		t.Fatalf("not canonical: %#v", input)
	}
}

func TestFrozenToolContractRejectsAmbiguityAndNormalizesSchema(t *testing.T) {
	input := []FrozenToolDefinition{{Name: "calculate", InputSchema: json.RawMessage(`{"type":"object", "properties":{}}`)}}
	got, err := NormalizeToolDefinitions(input)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got[0].InputSchema, []byte(`{"properties":{},"type":"object"}`)) {
		t.Fatalf("schema=%s", got[0].InputSchema)
	}
	if _, err := NormalizeToolDefinitions(append(input, input[0])); err == nil {
		t.Fatal("duplicate tool accepted")
	}
	input[0].InputSchema = json.RawMessage(`[]`)
	if _, err := NormalizeToolDefinitions(input); err == nil {
		t.Fatal("non-object schema accepted")
	}
}

func TestFrozenMCPHashBindsToolDefinition(t *testing.T) {
	binding := FrozenMCPBinding{
		SchemaVersion: 1, WorkspaceID: "w", ServerID: "s", ServerRevision: 1,
		Transport: "http", URL: "http://localhost:1234/mcp",
		AccessRef: CredentialReference{SchemaVersion: 1, Kind: CredentialMCPServerAccess, WorkspaceID: "w", ResourceID: "s", Slot: "access"},
		Tools:     []FrozenToolDefinition{{Name: "calculate", InputSchema: json.RawMessage(`{"type":"object","properties":{}}`)}},
	}
	original, err := HashDTO(binding, PreorderFrozenMCPBinding)
	if err != nil {
		t.Fatal(err)
	}
	binding.Tools[0].InputSchema = json.RawMessage(`{"type":"object","properties":{"value":{"type":"number"}}}`)
	changed, err := HashDTO(binding, PreorderFrozenMCPBinding)
	if err != nil {
		t.Fatal(err)
	}
	if changed == original {
		t.Fatal("tool schema omitted from dependency hash")
	}
	binding.Tools[0].InputSchema = json.RawMessage(`{"type":"object","properties":{}}`)
	binding.Tools[0].ReadOnly = true
	changed, err = HashDTO(binding, PreorderFrozenMCPBinding)
	if err != nil {
		t.Fatal(err)
	}
	if changed == original {
		t.Fatal("tool read policy omitted from dependency hash")
	}
}

func TestStandardV2InputNormalizationIsStable(t *testing.T) {
	raw := []byte(`{"schema_version":2,"mcp_servers":[{"server_id":"mcp"}]}`)
	var prior []byte
	for i := 0; i < 5; i++ {
		input, err := DecodeStandardFactoryInputV2(raw)
		if err != nil {
			t.Fatal(err)
		}
		raw, err = json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		if i > 0 && !bytes.Equal(prior, raw) {
			t.Fatalf("normalization changed on pass %d: %s -> %s", i, prior, raw)
		}
		prior = append([]byte(nil), raw...)
	}
}

func TestFrozenToolSchemaRejectsCanonicalNumericLoss(t *testing.T) {
	for _, schema := range []string{`{"type":"object","properties":{"n":{"const":9007199254740993}}}`, `{"type":"object","properties":{"n":{"minimum":0.123456789012345678901}}}`} {
		if _, err := NormalizeToolDefinitions([]FrozenToolDefinition{{Name: "read", InputSchema: json.RawMessage(schema)}}); err == nil {
			t.Fatalf("accepted changed numeric meaning: %s", schema)
		}
	}
	for _, schema := range []string{`{"type":"object","properties":{"n":{"const":1e3}}}`, `{"type":"object","properties":{"n":{"minimum":0.1}}}`} {
		if _, err := NormalizeToolDefinitions([]FrozenToolDefinition{{Name: "read", InputSchema: json.RawMessage(schema)}}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFrozenDecodeCannotHideToolSchemaPrecisionLoss(t *testing.T) {
	raw := []byte(`{"nested":{"tools":[{"name":"calculate","input_schema":{"type":"object","properties":{"n":{"const":9007199254740993}}}}]}}`)
	if _, err := strictDecode[map[string]any](raw); err == nil {
		t.Fatal("envelope canonicalization hid schema precision loss")
	}
	raw = []byte(`{"nested":{"tools":[{"name":"calculate","input_schema":{"type":"object","properties":{"n":{"const":1e3}}}}]}}`)
	if _, err := strictDecode[map[string]any](raw); err != nil {
		t.Fatal(err)
	}
}
