package mcpstdio

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestTeamDispatchRequiresRegisteredInputWithoutRawFallback(t *testing.T) {
	var requests atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		t.Error("invalid bound dispatch reached HTTP")
		response.WriteHeader(http.StatusInternalServerError)
	}))
	defer api.Close()
	dispatcher := NewToolDispatcher(mcpClient(t, api.URL))
	for _, body := range []string{`{"team_id":"team","task":"old task"}`, `{"team_id":"team","input_revision_id":"","task":"old task"}`} {
		result, err := dispatcher.Dispatch(context.Background(), structToolCall("team_dispatch", body))
		if err != nil || !result.IsError || result.Content != `{"error":"dispatch_input_required"}` {
			t.Fatalf("unbound call result=%+v err=%v", result, err)
		}
	}
	if requests.Load() != 0 {
		t.Fatal("unbound model input was dispatched")
	}
}

func TestTeamDispatchHiddenCatalogStillAllowsBoundReceiptReplay(t *testing.T) {
	t.Setenv("WEAVE_WORKBENCH_BOUND_DISPATCH", "1")
	const revision = "00000000-0000-4000-8000-000000000011"
	const fixedRequest = "00000000-0000-4000-8000-000000000012"
	var requests atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.Method != http.MethodPost || request.URL.Path != "/v1/teams/team/dispatch" {
			t.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if len(body) != 1 || body["input_revision_id"] != revision {
			t.Errorf("MCP injected text or a fresh nonce into a bound dispatch: %#v", body)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"input_revision_id":"` + revision + `","client_request_id":"` + fixedRequest + `","run_id":"run-existing","workflow_id":"flow","workflow_version":1,"task_id":"task-existing"}`))
	}))
	defer api.Close()
	dispatcher := NewToolDispatcher(mcpClient(t, api.URL))
	definitions, err := dispatcher.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(definitions) != len(toolDefinitions)-1 {
		t.Fatalf("catalog has %d tools", len(definitions))
	}
	for _, definition := range definitions {
		if definition.Name == "team_dispatch" || strings.Contains(definition.Name, "dispatch_input") {
			t.Fatalf("Host-owned dispatch/registration leaked into catalog: %s", definition.Name)
		}
	}
	result, err := dispatcher.Dispatch(context.Background(), structToolCall("team_dispatch", `{"team_id":"team","input_revision_id":"`+revision+`"}`))
	if err != nil || result.IsError || !strings.Contains(result.Content, "run-existing") || requests.Load() != 1 {
		t.Fatalf("bound replay unavailable: result=%+v requests=%d err=%v", result, requests.Load(), err)
	}
}
