package mcpprotocol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/jinyitao123/loom/contract"
)

type fakeDispatcher struct {
	tools       []contract.ToolDef
	listErr     error
	result      *contract.ToolResult
	dispatchErr error
	call        contract.ToolCall
}

func (d *fakeDispatcher) ListTools(context.Context) ([]contract.ToolDef, error) {
	return d.tools, d.listErr
}

func (d *fakeDispatcher) Dispatch(_ context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	d.call = call
	return d.result, d.dispatchErr
}

func TestDecodeRequiresExactlyOneJSONValue(t *testing.T) {
	for _, input := range []string{"", "{", `{"method":"tools/list"} {}`} {
		if _, err := Decode(bytes.NewBufferString(input)); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("Decode(%q) error = %v", input, err)
		}
	}
	request, err := Decode(bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil || request.Method != "tools/list" {
		t.Fatalf("Decode() = %#v, %v", request, err)
	}
}

func TestAdapterPreservesBoundaryAndGatewayResponses(t *testing.T) {
	for _, test := range []struct {
		name        string
		server      string
		unsupported string
	}{
		{name: "boundary", server: "weave-mcp-boundary", unsupported: "method not supported by boundary"},
		{name: "gateway", server: "weave-mcp-gateway", unsupported: "method not supported by gateway"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dispatcher := &fakeDispatcher{tools: []contract.ToolDef{{
				Name: "lookup", Description: "Lookup", InputSchema: json.RawMessage(`{"type":"object"}`),
			}}}
			adapter := Adapter{
				Dispatcher: dispatcher, ServerName: test.server,
				UnsupportedMethodMessage: test.unsupported, RedactToolErrors: true,
			}

			initialized, err := adapter.Handle(context.Background(), Request{ID: json.RawMessage(`1`), Method: "initialize"})
			if err != nil {
				t.Fatal(err)
			}
			assertJSON(t, initialized.Response, `{"jsonrpc":"2.0","id":1,"result":{"capabilities":{"tools":{}},"protocolVersion":"2025-03-26","serverInfo":{"name":"`+test.server+`","version":"1"}}}`)

			listed, err := adapter.Handle(context.Background(), Request{ID: json.RawMessage(`"list-1"`), Method: "tools/list"})
			if err != nil {
				t.Fatal(err)
			}
			assertJSON(t, listed.Response, `{"jsonrpc":"2.0","id":"list-1","result":{"tools":[{"name":"lookup","description":"Lookup","inputSchema":{"type":"object"}}]}}`)

			unknown, err := adapter.Handle(context.Background(), Request{ID: json.RawMessage(`2`), Method: "unknown"})
			if err != nil {
				t.Fatal(err)
			}
			assertJSON(t, unknown.Response, `{"jsonrpc":"2.0","id":2,"error":{"code":-32601,"message":"`+test.unsupported+`"}}`)
		})
	}
}

func TestAdapterToolCallBehavior(t *testing.T) {
	dispatcher := &fakeDispatcher{result: &contract.ToolResult{Content: "done"}}
	adapter := Adapter{Dispatcher: dispatcher, RedactToolErrors: true}
	result, err := adapter.Handle(context.Background(), Request{
		ID: json.RawMessage(`"call-1"`), Method: "tools/call",
		Params: json.RawMessage(`{"name":"lookup"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if dispatcher.call.ID != "call-1" || dispatcher.call.Name != "lookup" || dispatcher.call.Args != "{}" {
		t.Fatalf("call = %#v", dispatcher.call)
	}
	assertJSON(t, result.Response, `{"jsonrpc":"2.0","id":"call-1","result":{"content":[{"text":"done","type":"text"}],"isError":false}}`)

	dispatcher.result = &contract.ToolResult{Content: "private detail", IsError: true}
	result, err = adapter.Handle(context.Background(), Request{
		ID: json.RawMessage(`3`), Method: "tools/call",
		Params: json.RawMessage(`{"name":"lookup","arguments":{"q":"x"}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	assertJSON(t, result.Response, `{"jsonrpc":"2.0","id":3,"result":{"content":[{"text":"upstream MCP error","type":"text"}],"isError":true}}`)
}

func TestAdapterErrorClassification(t *testing.T) {
	dispatcher := &fakeDispatcher{listErr: errors.New("private")}
	adapter := Adapter{Dispatcher: dispatcher}
	if _, err := adapter.Handle(context.Background(), Request{Method: "tools/list"}); !errors.Is(err, ErrUpstreamUnavailable) {
		t.Fatalf("list error = %v", err)
	}
	if _, err := adapter.Handle(context.Background(), Request{Method: "tools/call", Params: json.RawMessage(`{`)}); !errors.Is(err, ErrInvalidCallParams) {
		t.Fatalf("params error = %v", err)
	}
	dispatcher.listErr = nil
	dispatcher.result = nil
	if _, err := adapter.Handle(context.Background(), Request{Method: "tools/call", Params: json.RawMessage(`{"name":"x"}`)}); !errors.Is(err, ErrEmptyToolResult) {
		t.Fatalf("empty result error = %v", err)
	}
	dispatcher.result = &contract.ToolResult{}
	dispatcher.dispatchErr = errors.New("private")
	if _, err := adapter.Handle(context.Background(), Request{Method: "tools/call", Params: json.RawMessage(`{"name":"x"}`)}); !errors.Is(err, ErrUpstreamUnavailable) {
		t.Fatalf("dispatch error = %v", err)
	}
	notification, err := adapter.Handle(context.Background(), Request{Method: "notifications/initialized"})
	if err != nil || !notification.Notification || notification.Response != nil {
		t.Fatalf("notification = %#v, %v", notification, err)
	}
}

func TestErrorResponseUsesJSONRPCWithoutPrivateDetails(t *testing.T) {
	assertJSON(t, ErrorResponse(nil, ErrInvalidRequest), `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"invalid JSON-RPC request"}}`)
	assertJSON(t, ErrorResponse(json.RawMessage(`7`), ErrInvalidCallParams), `{"jsonrpc":"2.0","id":7,"error":{"code":-32602,"message":"invalid tools/call params"}}`)
	assertJSON(t, ErrorResponse(json.RawMessage(`"x"`), errors.New("database password")), `{"jsonrpc":"2.0","id":"x","error":{"code":-32603,"message":"tool execution failed"}}`)
}

func assertJSON(t *testing.T, got any, want string) {
	t.Helper()
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var gotValue, wantValue any
	if err := json.Unmarshal(encoded, &gotValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("JSON = %s, want %s", encoded, want)
	}
}
