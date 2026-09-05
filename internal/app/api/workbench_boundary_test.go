package api

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/jinyitao123/weave/internal/kernel/config"
	"github.com/labstack/echo/v4"
)

func workbenchBoundaryServer() *Server {
	s := &Server{Echo: echo.New(), Config: &config.Config{JWTSecret: "workbench-boundary-test"}}
	s.registerRoutes()
	return s
}

func TestWorkbenchBoundaryHasNoRetiredBusinessRoutes(t *testing.T) {
	s := workbenchBoundaryServer()
	retiredFamilies := regexp.MustCompile(`^/v1/(projects|conversations|flags|inbox|sessions|task-groups|sources|schedules|agent-schedules|features|team-creation-options)(/|$)`)
	retiredActions := regexp.MustCompile(`^/v1/(agents/[^/]+/channels|messages/[^/]+/(flag|promote|deliverable)|runs/[^/]+/fork|mcp/tools)(/|$)`)
	standaloneWorkflow := regexp.MustCompile(`^/v1/(internal/)?workflows/[^/]+/run$`)
	for _, route := range s.Echo.Routes() {
		retired := retiredFamilies.MatchString(route.Path) || retiredActions.MatchString(route.Path) ||
			(route.Method == http.MethodPost && (route.Path == "/v1/chat" || standaloneWorkflow.MatchString(route.Path))) ||
			(route.Method == http.MethodGet && route.Path == "/v1/events")
		if retired {
			t.Errorf("retired business route registered: %s %s", route.Method, route.Path)
		}
	}
}

func TestWeaveServiceDoesNotServeAStandaloneBrowserUI(t *testing.T) {
	s := workbenchBoundaryServer()
	for _, path := range []string{"/", "/assets/retired.js", "/runtimes"} {
		recorder := httptest.NewRecorder()
		s.Echo.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("GET %s returned %d: %s", path, recorder.Code, recorder.Body.String())
		}
	}
}

func TestWorkbenchBoundaryRetiredRequestsStayUnavailable(t *testing.T) {
	s := workbenchBoundaryServer()
	// Authenticate these probes so a still-registered route cannot pass merely
	// because its middleware rejects an anonymous request. No database is used.
	token, err := s.signJWT("boundary-workspace", "boundary-user", []string{"admin"})
	if err != nil {
		t.Fatal(err)
	}
	requests := []struct{ method, path string }{
		{http.MethodGet, "/projects"},
		{http.MethodPost, "/projects"},
		{http.MethodPut, "/projects/project-1"},
		{http.MethodGet, "/projects/project-1/resources"},
		{http.MethodGet, "/conversations"},
		{http.MethodPatch, "/conversations/conversation-1"},
		{http.MethodGet, "/conversations/conversation-1/messages"},
		{http.MethodGet, "/conversations/conversation-1/deliverable"},
		{http.MethodGet, "/flags/count"},
		{http.MethodGet, "/inbox/unread"},
		{http.MethodPost, "/messages/message-1/flag"},
		{http.MethodDelete, "/messages/message-1/flag"},
		{http.MethodPost, "/messages/message-1/deliverable"},
		{http.MethodGet, "/sessions"},
		{http.MethodDelete, "/sessions/session-1"},
		{http.MethodPost, "/chat"},
		{http.MethodGet, "/events"},
		{http.MethodGet, "/agents/agent-1/channels"},
		{http.MethodPost, "/agents/agent-1/channels"},
		{http.MethodPost, "/mcp/tools"},
		{http.MethodPost, "/runs/run-1/fork"},
		{http.MethodGet, "/task-groups"},
		{http.MethodGet, "/sources"},
		{http.MethodGet, "/schedules"},
		{http.MethodPut, "/schedules"},
		{http.MethodGet, "/agent-schedules"},
		{http.MethodPut, "/agent-schedules"},
		{http.MethodGet, "/features"},
		{http.MethodGet, "/team-creation-options"},
		{http.MethodPost, "/workflows/workflow-1/run"},
		{http.MethodPost, "/internal/workflows/workflow-1/run"},
	}
	for _, request := range requests {
		t.Run(request.method+request.path, func(t *testing.T) {
			req := httptest.NewRequest(request.method, "/v1"+request.path, strings.NewReader(`{}`))
			req.Header.Set(echo.HeaderAuthorization, "Bearer "+token)
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			recorder := httptest.NewRecorder()
			s.Echo.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusNotFound && recorder.Code != http.StatusMethodNotAllowed {
				t.Fatalf("retired request returned %d: %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestWorkbenchBoundaryRequiredRoutesRemainAuthenticated(t *testing.T) {
	s := workbenchBoundaryServer()
	registered := make(map[string]bool)
	for _, route := range s.Echo.Routes() {
		registered[route.Method+" "+route.Path] = true
	}
	// These cover the Workbench MCP client, activity and recovery HTTP calls,
	// saved content, and the runtime transport they depend on.
	requests := []struct{ method, path string }{
		{http.MethodGet, "/team-templates/samples"},
		{http.MethodGet, "/teams"},
		{http.MethodGet, "/teams/:id"},
		{http.MethodPost, "/teams:from-template"},
		{http.MethodPost, "/teams/:id/dispatch"},
		{http.MethodGet, "/providers"},
		{http.MethodPost, "/providers"},
		{http.MethodPost, "/auth/api-keys"},
		{http.MethodGet, "/runtimes"},
		{http.MethodPost, "/runtimes"},
		{http.MethodGet, "/usage"},
		{http.MethodGet, "/internal/team-build-runs/:id/progress"},
		{http.MethodGet, "/internal/team-build-runs/:id/usage"},
		{http.MethodGet, "/chat-requests/:id"},
		{http.MethodGet, "/runs"},
		{http.MethodGet, "/runs/:id"},
		{http.MethodGet, "/runs/:id/activity"},
		{http.MethodPost, "/runs/:id/stop"},
		{http.MethodPost, "/runs/:id/stages/:node_id/retry"},
		{http.MethodGet, "/runs/:id/corrections"},
		{http.MethodPost, "/runs/:id/corrections"},
		{http.MethodPost, "/runs/:id/corrections/:correction_id/confirm"},
		{http.MethodGet, "/human-tasks"},
		{http.MethodGet, "/human-tasks/:run_id"},
		{http.MethodPost, "/human-tasks/:run_id/complete"},
		{http.MethodPost, "/resume"},
		{http.MethodGet, "/deliverables"},
		{http.MethodGet, "/deliverables/:id"},
		{http.MethodGet, "/deliverables/:id/content"},
		{http.MethodPost, "/runtime/claim"},
		{http.MethodPost, "/runtime/tasks/:id/events"},
		{http.MethodPost, "/runtime/tasks/:id/complete"},
		{http.MethodPost, "/runtime/tasks/:id/stopped"},
		{http.MethodPost, "/runtime/tasks/:id/mcp/:idx"},
	}
	parameter := regexp.MustCompile(`:[a-z_]+`)
	for _, request := range requests {
		t.Run(request.method+request.path, func(t *testing.T) {
			if !registered[request.method+" /v1"+request.path] {
				t.Fatalf("required Workbench route is missing")
			}
			path := "/v1" + parameter.ReplaceAllString(request.path, "boundary-id")
			for _, authorization := range []string{"", "Bearer invalid-token"} {
				req := httptest.NewRequest(request.method, path, strings.NewReader(`{}`))
				req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
				if authorization != "" {
					req.Header.Set(echo.HeaderAuthorization, authorization)
				}
				recorder := httptest.NewRecorder()
				s.Echo.ServeHTTP(recorder, req)
				if recorder.Code != http.StatusUnauthorized {
					t.Fatalf("unauthorized request returned %d: %s", recorder.Code, recorder.Body.String())
				}
			}
		})
	}
}
