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
	"github.com/jinyitao123/weave/internal/build/teamtemplate"
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
	if instructions, _ := initialize["instructions"].(string); !strings.Contains(instructions, "call team_list") ||
		!strings.Contains(instructions, "user-facing completion summary") ||
		!strings.Contains(instructions, "Keep internal run IDs") ||
		!strings.Contains(instructions, "Confirm team, task scope and deliverable before dispatch") ||
		!strings.Contains(instructions, "honor prior explicit approval without asking again") ||
		!strings.Contains(instructions, "If yielded or parked, inspect the waiting reason") ||
		!strings.Contains(instructions, "ask the user only for a human task") ||
		!strings.Contains(instructions, "Workbench shows progress, recovery and deliverables") ||
		!strings.Contains(instructions, "Workbench owns the work conversation") ||
		!strings.Contains(instructions, "Dispatch the original business task and materials") ||
		!strings.Contains(instructions, "Follow the same run after dispatch") ||
		strings.Contains(instructions, "Codex") || strings.Contains(instructions, "Claude") ||
		len(instructions) > 900 {
		t.Fatalf("initialize instructions = %q", instructions)
	}
	tools := responses[1]["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 21 {
		t.Fatalf("tool count = %d", len(tools))
	}
	wantNames := []string{
		"team_template_list", "team_create", "provider_list", "provider_add", "apikey_create",
		"runtime_create", "team_list", "team_status", "usage_summary",
		"team_dispatch", "build_status", "dispatch_status",
		"team_run_status", "team_run_activity", "team_run_stop", "human_task_list", "human_task_get", "human_task_complete",
		"resume", "deliverable_list", "deliverable_get",
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
		if tool["name"] == "team_create" {
			schema, _ := json.Marshal(tool["inputSchema"])
			if !bytes.Contains(schema, []byte(`"required":["definition"]`)) ||
				!bytes.Contains(schema, []byte(`"required":["yaml"]`)) ||
				bytes.Contains(schema, []byte(`"required":["yaml","declarative_spec"]`)) ||
				bytes.Contains(schema, []byte(`"runtime_ref"`)) || bytes.Contains(schema, []byte(`"model_ref"`)) ||
				!strings.Contains(description, "Prefer definition") {
				t.Fatalf("team_create contract does not expose the structured business path: %s / %s", schema, description)
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

func TestToolRoleScopeTableIsFrozen(t *testing.T) {
	want := map[string]toolAccessPolicy{
		"team_template_list":  {Role: "any", Scopes: []string{"org"}},
		"team_create":         {Role: "admin", Scopes: []string{"org"}},
		"provider_list":       {Role: "any", Scopes: []string{"admin"}},
		"provider_add":        {Role: "admin", Scopes: []string{"admin"}},
		"apikey_create":       {Role: "admin", Scopes: []string{"admin"}},
		"runtime_create":      {Role: "any", Scopes: []string{"org"}},
		"team_list":           {Role: "any", Scopes: []string{"org"}},
		"team_status":         {Role: "any", Scopes: []string{"org"}},
		"usage_summary":       {Role: "any", Scopes: []string{"runs", "org"}},
		"team_dispatch":       {Role: "any", Scopes: []string{"org", "chat"}},
		"build_status":        {Role: "any", Scopes: []string{"org"}},
		"dispatch_status":     {Role: "any", Scopes: []string{"chat"}},
		"team_run_status":     {Role: "any", Scopes: []string{"runs"}},
		"team_run_activity":   {Role: "any", Scopes: []string{"runs"}},
		"team_run_stop":       {Role: "any", Scopes: []string{"runs"}},
		"human_task_list":     {Role: "workspace_member", Scopes: []string{"runs"}},
		"human_task_get":      {Role: "workspace_member", Scopes: []string{"runs"}},
		"human_task_complete": {Role: "workspace_member", Scopes: []string{"runs"}},
		"resume":              {Role: "any", Scopes: []string{"chat"}},
		"deliverable_list":    {Role: "any", Scopes: []string{"chat"}},
		"deliverable_get":     {Role: "any", Scopes: []string{"chat"}},
	}
	if !reflect.DeepEqual(toolAccessPolicies, want) {
		t.Fatalf("tool access policies = %#v", toolAccessPolicies)
	}
	for _, tool := range toolDefinitions {
		if _, ok := toolAccessPolicies[tool.Name]; !ok {
			t.Errorf("tool %q has no access policy", tool.Name)
		}
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

func TestServeReturnsFieldProblemsForCorrectableValidationErrors(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = response.Write([]byte(`{"problems":[{"path":"/lead","code":"template_lead_unknown","message":"lead must reference a member"}]}`))
	}))
	defer api.Close()
	input := `{"jsonrpc":"2.0","id":"call","method":"tools/call","params":{"name":"team_create","arguments":{"sample":"market-research","idempotency_key":"018f5f5a-c73c-7e31-8f4a-9b36797553a1"}}}` + "\n"
	var output bytes.Buffer
	if err := Serve(context.Background(), strings.NewReader(input), &output, mcpClient(t, api.URL)); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`\"error\":\"http_422\"`, `\"path\":\"/lead\"`, `\"code\":\"template_lead_unknown\"`, "lead must reference a member"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("output missing %q: %s", want, output.String())
		}
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

func TestTeamCreatePassesDeclarativeSpec(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/teams:from-template" {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		spec, _ := body["declarative_spec"].(map[string]any)
		if spec["schema_version"] != float64(1) {
			t.Fatalf("body = %#v", body)
		}
		response.WriteHeader(http.StatusAccepted)
		_, _ = response.Write([]byte(`{"build_run_id":"build-1","status":"building"}`))
	}))
	defer api.Close()
	result, err := NewToolDispatcher(mcpClient(t, api.URL)).Dispatch(context.Background(), structToolCall(
		"team_create", `{"yaml":"schema: team-template/v1","declarative_spec":{"schema_version":1},"idempotency_key":"018f5f5a-c73c-7e31-8f4a-9b36797553a1"}`,
	))
	if err != nil || result.IsError || !strings.Contains(result.Content, "build-1") {
		t.Fatalf("result = %#v err = %v", result, err)
	}
}

func TestTeamCreatePassesBuiltInTemplateYAMLWithoutDeclarativeSpec(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/teams:from-template" {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["yaml"] != "schema: team-template/v1" {
			t.Fatalf("body = %#v", body)
		}
		if _, ok := body["declarative_spec"]; ok {
			t.Fatalf("unexpected declarative_spec in body = %#v", body)
		}
		response.WriteHeader(http.StatusAccepted)
		_, _ = response.Write([]byte(`{"build_run_id":"build-1","status":"building"}`))
	}))
	defer api.Close()
	result, err := NewToolDispatcher(mcpClient(t, api.URL)).Dispatch(context.Background(), structToolCall(
		"team_create", `{"yaml":"schema: team-template/v1","idempotency_key":"018f5f5a-c73c-7e31-8f4a-9b36797553a1"}`,
	))
	if err != nil || result.IsError || !strings.Contains(result.Content, "build-1") {
		t.Fatalf("result = %#v err = %v", result, err)
	}
}

func TestTeamCreateRendersStructuredBusinessDefinition(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		var body struct {
			YAML            string          `json:"yaml"`
			DeclarativeSpec json.RawMessage `json:"declarative_spec"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			"schema: team-template/v1", "display_name: 日冕研究团队", "template: research_synthesis",
			"parallel_worker_refs:", "- researcher", "max_cost_usd: 3",
		} {
			if !strings.Contains(body.YAML, want) {
				t.Fatalf("rendered YAML missing %q:\n%s", want, body.YAML)
			}
		}
		compiled, err := teamtemplate.CompileYAML([]byte(body.YAML))
		if err != nil {
			t.Fatalf("rendered structured definition does not compile: %v\n%s", err, body.YAML)
		}
		if compiled.Template.Name == "corona-research" || len(compiled.Template.Members) != 4 ||
			compiled.Template.TemplateParameters.FinalizerRef != "finalizer" {
			t.Fatalf("compiled template = %#v", compiled.Template)
		}
		if len(body.DeclarativeSpec) != 0 {
			t.Fatalf("unexpected declarative spec = %s", body.DeclarativeSpec)
		}
		response.WriteHeader(http.StatusAccepted)
		_, _ = response.Write([]byte(`{"build_run_id":"build-structured","status":"building"}`))
	}))
	defer api.Close()
	result, err := NewToolDispatcher(mcpClient(t, api.URL)).Dispatch(context.Background(), structToolCall(
		"team_create", `{"idempotency_key":"018f5f5a-c73c-7e31-8f4a-9b36797553a2","definition":{"display_name":"日冕研究团队","purpose":"形成决策研究","lead_instruction":"组织研究","lead":{"display_name":"负责人","responsibilities":["组织"],"capabilities":["delegation"],"result_requirement":"统筹交付"},"researchers":[{"display_name":"研究员","responsibilities":["研究"],"capabilities":["research"],"result_requirement":"提供资料"},{"display_name":"分析员","responsibilities":["分析"],"capabilities":["analysis"],"result_requirement":"交叉验证"}],"finalizer":{"display_name":"总装员","responsibilities":["交付"],"capabilities":["writing"],"result_requirement":"综合交付"},"success_criteria":["可追溯"],"max_cost_usd":3}}`,
	))
	if err != nil || result.IsError || !strings.Contains(result.Content, "build-structured") {
		t.Fatalf("result = %#v err = %v", result, err)
	}
}

func TestTeamListReturnsCompactMatchingFacts(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/teams" || request.URL.Query().Get("include") != "summary" {
			t.Fatalf("request = %s", request.URL.String())
		}
		_, _ = response.Write([]byte(`[{
			"team":{"id":"team-1","name":"writers","display_name":"内容创作组","status":"active","objective":"write",
			"primary_scenario":"articles","success_criteria":"publishable","default_workflow_id":"wf-1"},
			"lead":{"context_instruction":"large hidden prompt"},
			"workers":[{"duty":"draft","when_to_use":"writing","context_instruction":"secret prompt"}],
			"summary":{"published_workflow_count":1,"health":{"conclusion":"healthy"}}
		}]`))
	}))
	defer api.Close()
	result, err := NewToolDispatcher(mcpClient(t, api.URL)).Dispatch(context.Background(), structToolCall("team_list", `{}`))
	if err != nil || result.IsError {
		t.Fatalf("result = %#v err = %v", result, err)
	}
	if !strings.Contains(result.Content, `"workflow_available":true`) || !strings.Contains(result.Content, `"display_name":"内容创作组"`) || !strings.Contains(result.Content, `"draft"`) ||
		strings.Contains(result.Content, "context_instruction") || strings.Contains(result.Content, "hidden prompt") {
		t.Fatalf("compact list = %s", result.Content)
	}
}

func TestNormalizeDispatchResultUsesRootAndProductStatus(t *testing.T) {
	result, err := normalizeDispatchResult("request-1", json.RawMessage(`{
		"runs":[
			{"run_id":"child","parent_run_id":"root","status":"running"},
			{"run_id":"root","status":"succeeded"}
		]
	}`))
	if err != nil || !strings.Contains(string(result), `"run_id":"root"`) || !strings.Contains(string(result), `"status":"completed"`) {
		t.Fatalf("result = %s err = %v", result, err)
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
