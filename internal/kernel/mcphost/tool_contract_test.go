package mcphost

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
)

const argumentSchema = `{"type":"object","description":"MCP definition","properties":{"count":{"type":"integer","const":9007199254740993},"scale":{"type":"number","const":1e3},"tag":{"enum":["a","b"]},"optional":{"type":"string","default":"must not be inserted"}},"required":["count","scale","tag"],"additionalProperties":false}`

func boundTool(t *testing.T, schema string) *ToolContract {
	t.Helper()
	bound, err := NewToolContract([]contract.ToolDef{{Name: "calculate", InputSchema: json.RawMessage(schema)}})
	if err != nil {
		t.Fatal(err)
	}
	return bound
}

func TestToolContractArgumentsAndPrecision(t *testing.T) {
	bound := boundTool(t, argumentSchema)
	for name, args := range map[string]string{
		"number rounded":    `{"count":9007199254740992,"scale":1000,"tag":"a"}`,
		"coercion":          `{"count":"9007199254740993","scale":1000,"tag":"a"}`,
		"missing":           `{"count":9007199254740993,"tag":"a"}`,
		"extra":             `{"count":9007199254740993,"scale":1000,"tag":"a","extra":"PRIVATE_VALUE"}`,
		"enum":              `{"count":9007199254740993,"scale":1000,"tag":"PRIVATE_VALUE"}`,
		"duplicate":         `{"count":0,"count":9007199254740993,"scale":1000,"tag":"a"}`,
		"escaped duplicate": `{"count":0,"co\u0075nt":9007199254740993,"scale":1000,"tag":"a"}`,
		"trailing":          `{"count":9007199254740993,"scale":1000,"tag":"a"} {}`,
		"array":             `[]`, "null": `null`, "invalid": `{`, "empty": "",
		"surrogate": `{"count":9007199254740993,"scale":1000,"tag":"\ud800"}`,
	} {
		t.Run(name, func(t *testing.T) {
			call := contract.ToolCall{ID: "call-1", Name: "calculate", Args: args}
			result := bound.Validate(call)
			if result == nil || !result.IsError || result.CallID != call.ID || strings.Contains(result.Content, "PRIVATE_VALUE") {
				t.Fatalf("invalid arguments accepted or leaked: %#v", result)
			}
		})
	}
	call := contract.ToolCall{ID: "valid", Name: "calculate", Args: `{"count":9007199254740993,"scale":1e3,"tag":"a"}`}
	if rejected := bound.Validate(call); rejected != nil {
		t.Fatalf("precise arguments rejected: %#v", rejected)
	}
	if rejected := bound.Validate(contract.ToolCall{Name: "other", Args: `{}`}); rejected == nil {
		t.Fatal("unbound tool accepted")
	}
	other := boundTool(t, `{"type":"object","required":["other"]}`)
	if other.Validate(call) == nil || bound.Validate(call) != nil {
		t.Fatal("same-name server contracts were shared")
	}
}

func TestToolContractReferencesAndComposition(t *testing.T) {
	var fetched atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetched.Add(1)
		_, _ = w.Write([]byte(`{"type":"object"}`))
	}))
	defer server.Close()
	file := filepath.Join(t.TempDir(), "schema.json")
	if err := os.WriteFile(file, []byte(`{"type":"object"}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, schema := range []string{
		`{"$ref":"` + server.URL + `"}`, `{"$ref":"file://` + file + `"}`,
		`{"$schema":"` + server.URL + `"}`, `{"type":"not-a-json-type"}`, `[]`, ``,
	} {
		if _, err := NewToolContract([]contract.ToolDef{{Name: "calculate", InputSchema: json.RawMessage(schema)}}); !errors.Is(err, ErrFailClosed) {
			t.Fatalf("unavailable schema was accepted: %q, %v", schema, err)
		}
	}
	if fetched.Load() != 0 {
		t.Fatalf("external schemas were fetched: %d", fetched.Load())
	}
	bound := boundTool(t, `{"$defs":{"item":{"type":"integer","minimum":2}},"type":"object","properties":{"n":{"$ref":"#/$defs/item"}},"anyOf":[{"required":["n"]},{"required":["alternate"]}]}`)
	if bound.Validate(contract.ToolCall{Name: "calculate", Args: `{"n":2}`}) != nil || bound.Validate(contract.ToolCall{Name: "calculate", Args: `{"n":1}`}) == nil || bound.Validate(contract.ToolCall{Name: "calculate", Args: `{}`}) == nil {
		t.Fatal("local references or anyOf were not evaluated")
	}
	date := boundTool(t, `{"type":"object","properties":{"date":{"type":"string","format":"date-time"}},"required":["date"]}`)
	if date.Validate(contract.ToolCall{Name: "calculate", Args: `{"date":"invalid"}`}) == nil {
		t.Fatal("configured format assertion was ignored")
	}
}

func TestHTTPHostValidatesFinalArgumentsBeforeUpstreamCall(t *testing.T) {
	for _, published := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "bound"}[published], func(t *testing.T) {
			calls := 0
			var received json.RawMessage
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					ID     int64  `json:"id"`
					Method string `json:"method"`
					Params struct {
						Arguments json.RawMessage `json:"arguments"`
					} `json:"params"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				var result any
				switch req.Method {
				case "initialize":
					result = map[string]any{"protocolVersion": protocolVersion, "capabilities": map[string]any{}, "serverInfo": map[string]string{"name": "test", "version": "1"}}
				case "notifications/initialized":
					w.WriteHeader(202)
					return
				case "tools/list":
					result = map[string]any{"tools": []map[string]any{{"name": "calculate", "inputSchema": json.RawMessage(argumentSchema)}}}
				case "tools/call":
					calls++
					received = append([]byte(nil), req.Params.Arguments...)
					result = map[string]any{"content": []map[string]string{{"type": "text", "text": "ok"}}}
				default:
					t.Errorf("unexpected method %s", req.Method)
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
			}))
			defer server.Close()
			var opts []HostOption
			if published {
				opts = append(opts, WithToolContract(boundTool(t, argumentSchema)))
			}
			host := NewHTTPHost(server.URL, opts...)
			bad, err := host.Dispatch(t.Context(), contract.ToolCall{ID: "bad", Name: "calculate", Args: `{"count":"secret"}`})
			if err != nil || bad == nil || !bad.IsError || bad.CallID != "bad" || calls != 0 {
				t.Fatalf("invalid call reached upstream: %#v %v calls=%d", bad, err, calls)
			}
			args := `{"count":9007199254740993,"scale":1e3,"tag":"a"}`
			good, err := host.Dispatch(t.Context(), contract.ToolCall{ID: "good", Name: "calculate", Args: args})
			if err != nil || good == nil || good.IsError || calls != 1 || string(received) != args {
				t.Fatalf("valid arguments changed or failed: %#v %v received=%s", good, err, received)
			}
		})
	}
}

func TestToolContractValidatesFinalLoomHookArguments(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected request", 500)
	}))
	defer server.Close()
	host := NewHTTPHost(server.URL, WithToolContract(boundTool(t, argumentSchema)))
	original := contract.ToolCall{ID: "hook-call", Name: "calculate", Args: `{"count":9007199254740993,"scale":1e3,"tag":"a"}`}
	results, err := stdlib.DispatchWithHooks(t.Context(), host, []contract.ToolCall{original}, []contract.ToolDef{{Name: "calculate", ReadOnly: true}}, []contract.ToolHook{{Pre: func(_ context.Context, call contract.ToolCall) (contract.ToolCall, error) {
		call.Args = `{"count":"PRIVATE_VALUE","scale":1e3,"tag":"a"}`
		return call, nil
	}}})
	if err != nil || len(results) != 1 || !results[0].IsError || results[0].CallID != original.ID || requests.Load() != 0 {
		t.Fatalf("hook arguments escaped final check: results=%+v err=%v requests=%d", results, err, requests.Load())
	}
}

func TestToolContractRejectsResourceLimitInputs(t *testing.T) {
	bound := boundTool(t, `{"type":"object"}`)
	for _, args := range []string{`{"large":"` + strings.Repeat("x", maxToolArgsBytes) + `"}`, strings.Repeat(`{"n":`, 65) + `{}` + strings.Repeat(`}`, 65), `{"n":1e9999}`} {
		if rejected := bound.Validate(contract.ToolCall{Name: "calculate", Args: args}); rejected == nil {
			t.Fatal("unbounded arguments accepted")
		}
	}
	if _, err := NewToolContract([]contract.ToolDef{{Name: "calculate", InputSchema: json.RawMessage(`{"description":"` + strings.Repeat("x", maxToolSchemaBytes) + `"}`)}}); !errors.Is(err, ErrFailClosed) {
		t.Fatal("unbounded schema accepted")
	}
	if _, err := NewToolContract(make([]contract.ToolDef, maxToolSchemas+1)); !errors.Is(err, ErrFailClosed) {
		t.Fatal("unbounded catalog accepted")
	}
}
