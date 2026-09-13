package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func TestRetiredBusinessAndUnknownCommandsCannotStartServerOrCallAPI(t *testing.T) {
	t.Setenv(weaveclient.APIKeyEnv, "")
	for _, args := range [][]string{
		{"unknown"}, {"team", "samples"}, {"team", "up", "-f", "missing.yaml"},
		{"team", "dispatch", "--team", "team-1", "--task", "work"},
		{"status", "build", "build-1"}, {"status", "dispatch", "dispatch-1"},
		{"status", "team-run", "run-1"}, {"deliverable", "list"}, {"deliverable", "get", "file-1"},
	} {
		var stdout, stderr bytes.Buffer
		handled, code := Dispatch(args, &stdout, &stderr)
		if !handled || code != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), `"unknown_command"`) {
			t.Fatalf("Dispatch(%v) = %v, %d, stdout=%s stderr=%s", args, handled, code, stdout.String(), stderr.String())
		}
	}
}

func TestMCPCommandRequiresServeAndConfiguration(t *testing.T) {
	t.Setenv(weaveclient.APIKeyEnv, "")
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"mcp"}, "mcp_serve_required"},
		{[]string{"mcp", "other"}, "mcp_serve_required"},
		{[]string{"mcp", "serve", "extra"}, "mcp_serve_required"},
		{[]string{"mcp", "serve"}, "configuration_invalid"},
	} {
		var stderr bytes.Buffer
		handled, code := Dispatch(test.args, &bytes.Buffer{}, &stderr)
		if !handled || code != 2 || !strings.Contains(stderr.String(), test.want) {
			t.Fatalf("Dispatch(%v) = %v, %d, %s", test.args, handled, code, stderr.String())
		}
	}
}

func TestDoctorReportsServiceReadinessAndRuntimes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer wv_sk_cli_test" {
			t.Errorf("authorization = %q", request.Header.Get("Authorization"))
		}
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v1/health":
			_, _ = response.Write([]byte(`{"status":"ok","version":"test"}`))
		case "/v1/ready":
			_, _ = response.Write([]byte(`{"status":"ready"}`))
		case "/v1/runtimes":
			_, _ = response.Write([]byte(`{"runtimes":[{"name":"local","online":true}]}`))
		default:
			response.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	t.Setenv(weaveclient.BaseURLEnv, server.URL)
	t.Setenv(weaveclient.APIKeyEnv, "wv_sk_cli_test")
	var stdout, stderr bytes.Buffer
	handled, code := Dispatch([]string{"doctor"}, &stdout, &stderr)
	if !handled || code != 0 || stderr.Len() != 0 ||
		!strings.Contains(stdout.String(), `"readiness"`) ||
		!strings.Contains(stdout.String(), `"runtime_registry"`) ||
		!strings.Contains(stdout.String(), `"online": true`) {
		t.Fatalf("doctor = %v, %d, stdout=%s stderr=%s", handled, code, stdout.String(), stderr.String())
	}
}

func TestDoctorRejectsArgumentsAndMissingConfiguration(t *testing.T) {
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"doctor", "extra"}, "doctor_arguments_invalid"},
		{[]string{"doctor"}, "configuration_invalid"},
	} {
		t.Setenv(weaveclient.APIKeyEnv, "")
		var stderr bytes.Buffer
		handled, code := Dispatch(test.args, &bytes.Buffer{}, &stderr)
		if !handled || code != 2 || !strings.Contains(stderr.String(), test.want) {
			t.Fatalf("Dispatch(%v) = %v, %d, %s", test.args, handled, code, stderr.String())
		}
	}
}

func TestMCPTransportKeepsAPICallsAndRedactsFailures(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodGet || request.URL.Path != "/v1/team-templates/samples" ||
					request.Header.Get("Authorization") != "Bearer wv_sk_cli_test" ||
					request.Header.Get("X-Weave-User-Authorization") != "Bearer user-jwt" {
					t.Errorf("unexpected MCP API request")
					response.WriteHeader(http.StatusBadRequest)
					return
				}
				response.WriteHeader(status)
				if status == http.StatusOK {
					_, _ = response.Write([]byte(`{"samples":[{"name":"code-review"}]}`))
				} else {
					_, _ = response.Write([]byte(`{"error":"private database failure"}`))
				}
			}))
			defer server.Close()
			t.Setenv(weaveclient.BaseURLEnv, server.URL)
			t.Setenv(weaveclient.APIKeyEnv, "wv_sk_cli_test")
			input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"team_template_list","arguments":{},"_meta":{"weave_user_authorization":"Bearer user-jwt"}}}` + "\n"
			var output bytes.Buffer
			if err := runMCP(context.Background(), []string{"mcp", "serve"}, strings.NewReader(input), &output); err != nil {
				t.Fatal(err)
			}
			var response map[string]any
			if err := json.Unmarshal(output.Bytes(), &response); err != nil || response["id"] != float64(1) {
				t.Fatalf("MCP response = %s, err=%v", output.String(), err)
			}
			want := "code-review"
			if status != http.StatusOK {
				want = "http_500"
			}
			if !strings.Contains(output.String(), want) || strings.Contains(output.String(), "private database") ||
				strings.Contains(output.String(), "wv_sk_cli_test") {
				t.Fatalf("MCP result = %s", output.String())
			}
		})
	}
}
