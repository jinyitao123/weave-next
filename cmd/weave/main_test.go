package main

import (
	"bytes"
	"encoding/json"
	"os"
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

func TestMCPCommandDoesNotLoadServerConfig(t *testing.T) {
	t.Setenv(weaveclient.BaseURLEnv, "http://127.0.0.1:1")
	t.Setenv(weaveclient.APIKeyEnv, "wv_sk_early_test")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("JWT_SECRET", "")
	input, err := os.CreateTemp(t.TempDir(), "mcp-stdin")
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if _, err := input.WriteString(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	originalStdin := os.Stdin
	os.Stdin = input
	t.Cleanup(func() { os.Stdin = originalStdin })
	var stdout, stderr bytes.Buffer
	handled, code := dispatchEarlyCommand([]string{"mcp", "serve"}, &stdout, &stderr)
	if !handled || code != 0 || !strings.Contains(stdout.String(), `"serverInfo":{"name":"weave"`) {
		t.Fatalf("dispatch = %v, %d, stdout=%s stderr=%s", handled, code, stdout.String(), stderr.String())
	}
}

func TestBootstrapRejectsRetiredClientConfigurationBeforeDatabaseAccess(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	var stdout, stderr bytes.Buffer
	handled, code := dispatchEarlyCommand([]string{"bootstrap", "--command", "/usr/local/bin/weave"}, &stdout, &stderr)
	if !handled || code != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), `"invalid_arguments"`) {
		t.Fatalf("bootstrap = %v, %d, stdout=%s stderr=%s", handled, code, stdout.String(), stderr.String())
	}
}

func TestServeAndEmptyArgsRemainOnServerPath(t *testing.T) {
	for _, args := range [][]string{nil, {"serve"}} {
		if handled, code := dispatchEarlyCommand(args, &bytes.Buffer{}, &bytes.Buffer{}); handled || code != 0 {
			t.Fatalf("dispatch(%v) = %v, %d", args, handled, code)
		}
	}
}
