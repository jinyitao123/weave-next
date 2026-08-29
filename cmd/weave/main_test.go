package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jinyitao123/weave/internal/app/weaveclient"
	"github.com/jinyitao123/weave/internal/base/testutil"
)

func TestBootstrapEarlyCommandRunsMigrationsWithoutServerConfigRealPG(t *testing.T) {
	pool := testutil.PostgresPool(t)
	t.Setenv("DATABASE_URL", pool.Config().ConnString())
	t.Setenv("JWT_SECRET", "")
	workspaceID := "bootstrap-command-" + uuid.NewString()
	args := []string{"bootstrap", "--workspace", workspaceID, "--username", "admin", "--password", "password"}
	var stdout, stderr bytes.Buffer
	handled, code := dispatchEarlyCommand(args, &stdout, &stderr)
	if !handled || code != 0 {
		t.Fatalf("first bootstrap = %v, %d, stdout=%s stderr=%s", handled, code, stdout.String(), stderr.String())
	}
	var first struct {
		APIKeyCreated bool   `json:"api_key_created"`
		APIKey        string `json:"api_key"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &first); err != nil || !first.APIKeyCreated || !strings.HasPrefix(first.APIKey, "wv_sk_") {
		t.Fatalf("first result = %#v err=%v body=%s", first, err, stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	handled, code = dispatchEarlyCommand(args, &stdout, &stderr)
	if !handled || code != 0 || !strings.Contains(stdout.String(), `"api_key_created": false`) || strings.Contains(stdout.String(), first.APIKey) {
		t.Fatalf("second bootstrap = %v, %d, stdout=%s stderr=%s", handled, code, stdout.String(), stderr.String())
	}
}

func TestClientCommandDispatchDoesNotLoadServerConfig(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		_, _ = response.Write([]byte(`{"samples":[]}`))
	}))
	defer server.Close()
	t.Setenv(weaveclient.BaseURLEnv, server.URL)
	t.Setenv(weaveclient.APIKeyEnv, "wv_sk_early_test")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("JWT_SECRET", "")
	var stdout, stderr bytes.Buffer
	handled, code := dispatchEarlyCommand([]string{"team", "samples"}, &stdout, &stderr)
	if !handled || code != 0 || !strings.Contains(stdout.String(), `"samples"`) {
		t.Fatalf("dispatch = %v, %d, stdout=%s stderr=%s", handled, code, stdout.String(), stderr.String())
	}
}

func TestServeAndEmptyArgsRemainOnServerPath(t *testing.T) {
	for _, args := range [][]string{nil, {"serve"}} {
		if handled, code := dispatchEarlyCommand(args, &bytes.Buffer{}, &bytes.Buffer{}); handled || code != 0 {
			t.Fatalf("dispatch(%v) = %v, %d", args, handled, code)
		}
	}
}
