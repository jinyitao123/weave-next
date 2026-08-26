package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jinyitao123/weave/internal/app/weaveclient"
)

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
