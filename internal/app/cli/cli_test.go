package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jinyitao123/weave/internal/app/weaveclient"
)

func TestServerCommandsAreNotIntercepted(t *testing.T) {
	for _, args := range [][]string{nil, {"serve"}, {"runtime"}, {"daemon"}} {
		handled, code := Dispatch(args, &bytes.Buffer{}, &bytes.Buffer{})
		if handled || code != 0 {
			t.Fatalf("Dispatch(%v) = %v, %d", args, handled, code)
		}
	}
}

func TestUnknownCommandReturnsNonzero(t *testing.T) {
	var stderr bytes.Buffer
	handled, code := Dispatch([]string{"unknown"}, &bytes.Buffer{}, &stderr)
	if !handled || code == 0 || !strings.Contains(stderr.String(), `"unknown_command"`) {
		t.Fatalf("Dispatch() = %v, %d, %s", handled, code, stderr.String())
	}
}

func TestTeamSamplesAndStatusFormatAPIJSON(t *testing.T) {
	server := cliServer(t, func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.RequestURI() {
		case "/v1/team-templates/samples":
			_, _ = response.Write([]byte(`{"samples":[{"name":"code-review"}]}`))
		case "/v1/internal/team-build-runs/build-1/progress":
			_, _ = response.Write([]byte(`{"run_status":"ready"}`))
		default:
			t.Fatalf("unexpected request %s", request.URL.RequestURI())
		}
	})
	defer server.Close()
	setCLIEnv(t, server.URL)

	for _, test := range []struct {
		args []string
		want string
	}{
		{args: []string{"team", "samples"}, want: `"code-review"`},
		{args: []string{"status", "build", "build-1"}, want: `"ready"`},
	} {
		var stdout, stderr bytes.Buffer
		handled, code := Dispatch(test.args, &stdout, &stderr)
		if !handled || code != 0 || !strings.Contains(stdout.String(), test.want) {
			t.Fatalf("Dispatch(%v) = %v, %d, stdout=%s stderr=%s", test.args, handled, code, stdout.String(), stderr.String())
		}
		var decoded any
		if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
			t.Fatalf("output is not JSON: %v", err)
		}
	}
}

func TestTeamUpRequiresExplicitIdempotencyKey(t *testing.T) {
	setCLIEnv(t, "http://127.0.0.1:1")
	file := filepath.Join(t.TempDir(), "team.yaml")
	if err := os.WriteFile(file, []byte("name: test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	_, code := Dispatch([]string{"team", "up", "-f", file}, &bytes.Buffer{}, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), `"idempotency_key_required"`) {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
}

func TestTeamUpReadsFileAndCallsTemplateEndpoint(t *testing.T) {
	server := cliServer(t, func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/teams:from-template" {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["yaml"] != "name: test\n" || body["idempotency_key"] != "018f5f5a-c73c-7e31-8f4a-9b36797553a1" {
			t.Fatalf("body = %#v", body)
		}
		response.WriteHeader(http.StatusCreated)
		_, _ = response.Write([]byte(`{"team_id":"team-1","status":"ready"}`))
	})
	defer server.Close()
	setCLIEnv(t, server.URL)
	file := filepath.Join(t.TempDir(), "team.yaml")
	if err := os.WriteFile(file, []byte("name: test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	_, code := Dispatch([]string{
		"team", "up", "-f", file, "--idempotency-key", "018f5f5a-c73c-7e31-8f4a-9b36797553a1",
	}, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), `"team_id": "team-1"`) {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
}

func TestTeamDispatchOnlyResolvesTeamToLead(t *testing.T) {
	server := cliServer(t, func(response http.ResponseWriter, request *http.Request) {
		switch request.Method + " " + request.URL.Path {
		case "GET /v1/teams/team-1":
			_, _ = response.Write([]byte(`{"team":{"status":"active"},"lead":{"name":"lead"}}`))
		case "POST /v1/chat":
			response.WriteHeader(http.StatusAccepted)
			_, _ = response.Write([]byte(`{"status":"queued"}`))
		default:
			t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
		}
	})
	defer server.Close()
	setCLIEnv(t, server.URL)
	var stdout, stderr bytes.Buffer
	_, code := Dispatch([]string{"team", "dispatch", "--team", "team-1", "--task", "work"}, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), `"client_request_id"`) || !strings.Contains(stdout.String(), `"queued"`) {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
}

func TestConfigurationAndAPIErrorsAreStable(t *testing.T) {
	t.Setenv(weaveclient.APIKeyEnv, "")
	var stderr bytes.Buffer
	_, code := Dispatch([]string{"team", "samples"}, &bytes.Buffer{}, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), `"configuration_invalid"`) {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}

	server := cliServer(t, func(response http.ResponseWriter, request *http.Request) {
		response.WriteHeader(http.StatusInternalServerError)
		_, _ = response.Write([]byte(`{"error":"private database failure"}`))
	})
	defer server.Close()
	setCLIEnv(t, server.URL)
	stderr.Reset()
	_, code = Dispatch([]string{"team", "samples"}, &bytes.Buffer{}, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), `"http_500"`) || strings.Contains(stderr.String(), "database") {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
}

func cliServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer wv_sk_cli_test" {
			t.Fatalf("authorization = %q", request.Header.Get("Authorization"))
		}
		response.Header().Set("Content-Type", "application/json")
		handler(response, request)
	}))
}

func setCLIEnv(t *testing.T, baseURL string) {
	t.Helper()
	t.Setenv(weaveclient.BaseURLEnv, baseURL)
	t.Setenv(weaveclient.APIKeyEnv, "wv_sk_cli_test")
}
