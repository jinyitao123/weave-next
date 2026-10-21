package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/kernel/config"
	"github.com/labstack/echo/v4"
)

func TestTeamManagementRoutesAreRegistered(t *testing.T) {
	server := NewServer(&config.Config{}, nil, nil)
	routes := map[string]bool{}
	for _, route := range server.Echo.Routes() {
		routes[route.Method+" "+route.Path] = true
	}
	for _, want := range []string{
		http.MethodPut + " /v1/teams/:id/profile",
		http.MethodPost + " /v1/teams/:id/workers",
		http.MethodDelete + " /v1/teams/:id/workers/:worker",
	} {
		if !routes[want] {
			t.Fatalf("missing team management route %s", want)
		}
	}
}

// Agents and skills are shared development resources of the workspace.
func TestMemberCannotChangeSharedDevelopmentResources(t *testing.T) {
	server := NewServer(&config.Config{JWTSecret: "route-gate-test"}, nil, nil)
	for _, role := range []string{"member", roleDeveloper} {
		token, err := server.signJWTWithPermissionSets("ws", "user", []string{role}, "forge", nil, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		for _, route := range [][2]string{
			{http.MethodPost, "/v1/agents"}, {http.MethodPut, "/v1/agents/a"}, {http.MethodDelete, "/v1/agents/a"},
			{http.MethodPost, "/v1/agents/upload"}, {http.MethodPost, "/v1/agents/import/preview"},
			{http.MethodPost, "/v1/skills"}, {http.MethodPut, "/v1/skills/s"}, {http.MethodDelete, "/v1/skills/s"}, {http.MethodPost, "/v1/agents/a/memories"}, {http.MethodDelete, "/v1/agents/a/memories/m"},
		} {
			request := httptest.NewRequest(route[0], route[1], strings.NewReader("{}"))
			request.Header.Set(echo.HeaderAuthorization, "Bearer "+token)
			request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			recorder := httptest.NewRecorder()
			server.Echo.ServeHTTP(recorder, request)
			if refused := recorder.Code == http.StatusForbidden; refused != (role == "member") {
				t.Fatalf("%s %s %s: status %d %s", role, route[0], route[1], recorder.Code, recorder.Body.String())
			}
		}
	}
}
