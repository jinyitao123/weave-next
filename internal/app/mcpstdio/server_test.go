package mcpstdio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/app/weaveclient"
)

func TestServeUsesSharedProtocolForInitializeListAndCall(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/v1/team-templates/samples" {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer wv_sk_mcp_test" {
			t.Fatalf("authorization = %q", request.Header.Get("Authorization"))
		}
		_, _ = response.Write([]byte(`{"samples":[{"name":"code-review"}]}`))
	}))
	defer api.Close()
	client := mcpClient(t, api.URL)
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"team_template_list","arguments":{}}}`,
		`not-json`,
	}, "\n") + "\n"
	var output bytes.Buffer
	if err := Serve(context.Background(), strings.NewReader(input), &output, client); err != nil {
		t.Fatal(err)
	}
	responses := decodeResponses(t, output.String())
	if len(responses) != 4 {
		t.Fatalf("response count = %d, output=%s", len(responses), output.String())
	}
	initialize := responses[0]["result"].(map[string]any)
	serverInfo := initialize["serverInfo"].(map[string]any)
	if initialize["protocolVersion"] != "2025-03-26" || serverInfo["name"] != "weave" {
		t.Fatalf("initialize = %#v", initialize)
	}
	tools := responses[1]["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 9 {
		t.Fatalf("tool count = %d", len(tools))
	}
	wantNames := []string{
		"team_template_list", "team_create", "team_dispatch", "build_status", "dispatch_status",
		"team_run_status", "resume", "deliverable_list", "deliverable_get",
	}
	gotNames := make([]string, 0, len(tools))
	for _, raw := range tools {
		tool := raw.(map[string]any)
		gotNames = append(gotNames, tool["name"].(string))
		description := tool["description"].(string)
		for _, forbidden := range []string{"internal/", "/v1/", "state machine", "HTTP response body"} {
			if strings.Contains(description, forbidden) {
				t.Errorf("description for %s contains %q: %s", tool["name"], forbidden, description)
			}
		}
	}
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Fatalf("tool names = %#v", gotNames)
	}
	callText := responses[2]["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(callText, "code-review") {
		t.Fatalf("call result = %s", callText)
	}
	parseError := responses[3]["error"].(map[string]any)
	if parseError["code"] != float64(-32700) {
		t.Fatalf("parse error = %#v", parseError)
	}
}

func TestServeReturnsStableToolErrorWithoutRawHTTPBody(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.WriteHeader(http.StatusInternalServerError)
		_, _ = response.Write([]byte(`{"error":"private database password"}`))
	}))
	defer api.Close()
	input := `{"jsonrpc":"2.0","id":"call","method":"tools/call","params":{"name":"team_template_list","arguments":{}}}` + "\n"
	var output bytes.Buffer
	if err := Serve(context.Background(), strings.NewReader(input), &output, mcpClient(t, api.URL)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `\"error\":\"http_500\"`) ||
		!strings.Contains(output.String(), `"isError":true`) || strings.Contains(output.String(), "password") {
		t.Fatalf("output = %s", output.String())
	}
}

func TestToolArgumentsRejectUnknownFields(t *testing.T) {
	client := mcpClient(t, "http://127.0.0.1:1")
	result, err := NewToolDispatcher(client).Dispatch(context.Background(), structToolCall(
		"deliverable_get", `{"id":"x","extra":true}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || result.Content != `{"error":"invalid_arguments"}` {
		t.Fatalf("result = %#v", result)
	}
}

func structToolCall(name, args string) contract.ToolCall {
	return contract.ToolCall{ID: "call", Name: name, Args: args}
}

func mcpClient(t *testing.T, baseURL string) *weaveclient.Client {
	t.Helper()
	client, err := weaveclient.New(weaveclient.Config{BaseURL: baseURL, APIKey: "wv_sk_mcp_test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func decodeResponses(t *testing.T, output string) []map[string]any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(output))
	var responses []map[string]any
	for {
		var response map[string]any
		if err := decoder.Decode(&response); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		responses = append(responses, response)
	}
	return responses
}
