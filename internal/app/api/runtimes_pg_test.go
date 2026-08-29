package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/labstack/echo/v4"
)

func TestCreateRuntimeReturnsThreeReadyCommandsRealPG(t *testing.T) {
	ctx := context.Background()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	workspaceID := "runtime-command-" + uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES($1,$1,$1)`, workspaceID); err != nil {
		t.Fatal(err)
	}
	server := &Server{Runtimes: runtimes.NewStore(pool)}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/runtimes", strings.NewReader(`{"name":"local-mac"}`))
	request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	echoContext := echo.New().NewContext(request, recorder)
	echoContext.Set("tenant", workspaceID)
	if err := server.handleCreateRuntime(echoContext); err != nil {
		t.Fatal(err)
	}
	var response struct {
		Token        string            `json:"token"`
		NextCommands map[string]string `json:"next_commands"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusCreated || len(response.NextCommands) != 3 {
		t.Fatalf("runtime response status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	for _, shape := range []string{"direct", "install", "docker"} {
		command := response.NextCommands[shape]
		if !strings.Contains(command, response.Token) {
			t.Errorf("%s command does not contain one-time token: %q", shape, command)
		}
	}
	if !strings.Contains(response.NextCommands["direct"], "weave runtime") ||
		!strings.Contains(response.NextCommands["install"], "install.sh") ||
		!strings.Contains(response.NextCommands["docker"], "docker compose") {
		t.Fatalf("runtime commands = %#v", response.NextCommands)
	}
}
