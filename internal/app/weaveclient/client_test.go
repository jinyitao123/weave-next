package weaveclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testAPIKey = "wv_sk_test"

func TestConfigFromEnv(t *testing.T) {
	t.Setenv(BaseURLEnv, "")
	t.Setenv(APIKeyEnv, testAPIKey)
	config, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if config.BaseURL != "http://127.0.0.1:8080" || config.APIKey != testAPIKey {
		t.Fatalf("config = %#v", config)
	}
	t.Setenv(APIKeyEnv, "not-a-key")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("ConfigFromEnv() accepted invalid API key")
	}
}

func TestTeamCreateRequiresCallerIdempotencyKey(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("unexpected HTTP request")
	}))
	_, err := client.TeamCreate(context.Background(), TeamCreateRequest{Sample: "code-review"})
	assertClientError(t, err, "idempotency_key_required", 0)
}

func TestTeamDispatchUsesUnifiedFreeCollabAndPollsToYielded(t *testing.T) {
	var polls atomic.Int32
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+testAPIKey {
			t.Fatalf("authorization = %q", request.Header.Get("Authorization"))
		}
		switch request.Method + " " + request.URL.Path {
		case "POST /v1/teams/team-1/dispatch":
			var body map[string]any
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["task"] != "do work" || body["mode"] != "free_collab" || body["client_request_id"] != "5d60aa9f-31d7-4fdf-a40c-53a46094e9d0" {
				t.Fatalf("chat body = %#v", body)
			}
			writeJSON(response, http.StatusAccepted, `{"status":"queued"}`)
		case "GET /v1/chat-requests/5d60aa9f-31d7-4fdf-a40c-53a46094e9d0":
			if polls.Add(1) == 1 {
				writeJSON(response, http.StatusOK, `{"status":"queued"}`)
				return
			}
			writeJSON(response, http.StatusOK, `{"status":"yielded","run_id":"run-1"}`)
		default:
			t.Fatalf("unexpected request %s %s", request.Method, request.URL.String())
		}
	})
	client := newTestClient(t, handler)
	id, result, err := client.TeamDispatchAndWait(context.Background(), DispatchRequest{
		TeamID: "team-1", Task: "do work", Mode: "free_collab", ClientRequestID: "5d60aa9f-31d7-4fdf-a40c-53a46094e9d0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != "5d60aa9f-31d7-4fdf-a40c-53a46094e9d0" || !strings.Contains(string(result), `"status":"yielded"`) {
		t.Fatalf("result = %q, %s", id, result)
	}
}

func TestTeamDispatchHandlesInProgressConflict(t *testing.T) {
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.Method + " " + request.URL.Path {
		case "POST /v1/teams/team-1/dispatch":
			writeJSON(response, http.StatusConflict, `{"code":"client_request_in_progress"}`)
		case "GET /v1/chat-requests/5d60aa9f-31d7-4fdf-a40c-53a46094e9d0":
			writeJSON(response, http.StatusOK, `{"status":"failed","error_code":"provider_failed"}`)
		default:
			t.Fatalf("unexpected request %s %s", request.Method, request.URL.String())
		}
	})
	client := newTestClient(t, handler)
	_, result, err := client.TeamDispatch(context.Background(), DispatchRequest{
		TeamID: "team-1", Task: "do work", ClientRequestID: "5d60aa9f-31d7-4fdf-a40c-53a46094e9d0",
	})
	if err != nil || !strings.Contains(string(result), `"status":"failed"`) {
		t.Fatalf("result = %s, error = %v", result, err)
	}
}

func TestTeamDispatchWaitsForFixedWorkflowRun(t *testing.T) {
	var polls atomic.Int32
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.Method + " " + request.URL.Path {
		case "POST /v1/teams/team-1/dispatch":
			writeJSON(response, http.StatusCreated, `{"run_id":"run-1","workflow_id":"workflow-1","workflow_version":2,"task_id":"task-1"}`)
		case "GET /v1/runs":
			if request.URL.Query().Get("run_snapshot_id") != "run-1" {
				t.Fatalf("query = %s", request.URL.RawQuery)
			}
			if polls.Add(1) == 1 {
				writeJSON(response, http.StatusOK, `{"runs":[],"total":0}`)
				return
			}
			writeJSON(response, http.StatusOK, `{"runs":[{"status":"succeeded"}],"total":1}`)
		default:
			t.Fatalf("unexpected request %s %s", request.Method, request.URL.String())
		}
	})
	client := newTestClient(t, handler)
	_, result, err := client.TeamDispatchAndWait(context.Background(), DispatchRequest{
		TeamID: "team-1", Task: "do work", Mode: "workflow",
	})
	if err != nil || !strings.Contains(string(result), `"status":"succeeded"`) {
		t.Fatalf("result = %s, error = %v", result, err)
	}
}

func TestTeamDispatchRejectsMissingOrInactiveTeam(t *testing.T) {
	for _, test := range []struct {
		name       string
		status     int
		body       string
		wantCode   string
		wantStatus int
	}{
		{name: "missing", status: http.StatusNotFound, body: `{"error":"team not found"}`, wantCode: "team_not_found", wantStatus: http.StatusNotFound},
		{name: "inactive", status: http.StatusOK, body: `{"team":{"status":"building"},"lead":{"name":"lead"}}`, wantCode: "team_not_active", wantStatus: http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := newTestClient(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if request.URL.Path != "/v1/teams/team-1/dispatch" {
					t.Fatalf("unexpected request %s", request.URL.Path)
				}
				if test.name == "missing" {
					writeJSON(response, test.status, `{"code":"team_not_found"}`)
				} else {
					writeJSON(response, http.StatusConflict, `{"code":"team_not_active"}`)
				}
			}))
			_, _, err := client.TeamDispatch(context.Background(), DispatchRequest{TeamID: "team-1", Task: "work"})
			assertClientError(t, err, test.wantCode, test.wantStatus)
		})
	}
}

func TestReadEndpointsAndResumeAreThinHTTPMappings(t *testing.T) {
	seen := make(map[string]bool)
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		key := request.Method + " " + request.URL.RequestURI()
		seen[key] = true
		if request.URL.Path == "/v1/resume" {
			writeJSON(response, http.StatusOK, "event: done\ndata: {}\n\n")
			return
		}
		writeJSON(response, http.StatusOK, `{}`)
	})
	client := newTestClient(t, handler)
	ctx := context.Background()
	_, _ = client.TeamTemplateList(ctx)
	_, _ = client.TeamCreate(ctx, TeamCreateRequest{Sample: "code-review", IdempotencyKey: "id"})
	_, _ = client.ProviderList(ctx)
	_, _ = client.ProviderAdd(ctx, ProviderAddRequest{Name: "openai", BaseURL: "https://example.test", APIKey: "secret", Models: []string{"model"}})
	_, _ = client.APIKeyCreate(ctx, APIKeyCreateRequest{Name: "codex", Scopes: []string{"runs"}})
	_, _ = client.RuntimeCreate(ctx, "mac")
	_, _ = client.TeamList(ctx, "all", true)
	_, _ = client.TeamStatus(ctx, "team-1")
	_, _ = client.UsageSummary(ctx, "build-1")
	_, _ = client.BuildStatus(ctx, "build-1")
	_, _ = client.DispatchStatus(ctx, "dispatch-1")
	_, _ = client.TeamRunStatus(ctx, "snapshot-1")
	_, _ = client.HumanTaskList(ctx, 20, "next")
	_, _ = client.HumanTaskGet(ctx, "human-1", "/predecessor_outputs/chapter", 10, 50)
	_, _ = client.HumanTaskComplete(ctx, HumanTaskCompleteRequest{
		RunID: "human-1", Payload: json.RawMessage(`{"decision":"approve"}`), IdempotencyKey: "complete-1",
	})
	_, _ = client.DeliverableList(ctx, 20, 5)
	_, _ = client.DeliverableGet(ctx, "delivery-1")
	_, _ = client.DeliverableGetPath(ctx, "delivery-1", "/chapters/one", 0, 25)
	_, _ = client.Resume(ctx, ResumeRequest{RunID: "run-1", Agent: "lead", Input: map[string]any{"answer": "yes"}})
	want := []string{
		"GET /v1/team-templates/samples", "POST /v1/teams:from-template",
		"GET /v1/providers", "POST /v1/providers", "POST /v1/auth/api-keys", "POST /v1/runtimes",
		"GET /v1/teams?include=summary&status=all", "GET /v1/teams/team-1?include=summary",
		"GET /v1/usage", "GET /v1/internal/team-build-runs/build-1/usage",
		"GET /v1/internal/team-build-runs/build-1/progress", "GET /v1/chat-requests/dispatch-1",
		"GET /v1/runs?aggregation_mode=all-exclusive&run_snapshot_id=snapshot-1&view=run",
		"GET /v1/human-tasks?cursor=next&limit=20",
		"GET /v1/human-tasks/human-1?limit=50&offset=10&path=%2Fpredecessor_outputs%2Fchapter",
		"POST /v1/human-tasks/human-1/complete",
		"GET /v1/deliverables?limit=20&offset=5",
		"GET /v1/deliverables/delivery-1",
		"GET /v1/deliverables/delivery-1?limit=25&offset=0&path=%2Fchapters%2Fone", "POST /v1/resume",
	}
	for _, key := range want {
		if !seen[key] {
			t.Errorf("request not seen: %s", key)
		}
	}
}

func TestAPIErrorDoesNotExposeRawBody(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		writeJSON(response, http.StatusInternalServerError, `{"error":"database password is secret"}`)
	}))
	_, err := client.TeamTemplateList(context.Background())
	assertClientError(t, err, "http_500", http.StatusInternalServerError)
	if strings.Contains(err.Error(), "password") {
		t.Fatalf("error exposed response body: %v", err)
	}
}

func newTestClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := New(Config{BaseURL: server.URL, APIKey: testAPIKey, PollInterval: time.Millisecond}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func writeJSON(response http.ResponseWriter, status int, body string) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_, _ = response.Write([]byte(body))
}

func assertClientError(t *testing.T, err error, code string, status int) {
	t.Helper()
	apiErr, ok := err.(*Error)
	if !ok || apiErr.Code != code || apiErr.StatusCode != status {
		t.Fatalf("error = %#v, want code=%q status=%d", err, code, status)
	}
}
