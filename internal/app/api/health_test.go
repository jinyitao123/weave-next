package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jinyitao123/weave/internal/app/apikeys"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/config"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/delivery"
	"github.com/jinyitao123/weave/internal/kernel/mcpregistry"
	"github.com/jinyitao123/weave/internal/kernel/org"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/labstack/echo/v4"
)

func TestHandleReadyReportsMissingDependencies(t *testing.T) {
	recorder := httptest.NewRecorder()
	ctx := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/v1/ready", nil), recorder)
	if err := (&Server{}).handleReady(ctx); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusServiceUnavailable ||
		!strings.Contains(recorder.Body.String(), `"status":"not_ready"`) ||
		!strings.Contains(recorder.Body.String(), `"credential_store"`) ||
		!strings.Contains(recorder.Body.String(), `"team_template_service"`) {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestHandleReadyReportsReady(t *testing.T) {
	server := &Server{
		Pool:            testutil.PostgresPool(t),
		KeyStore:        &apikeys.Store{},
		OrgStore:        &org.Store{},
		Workflow:        &workflow.Store{},
		Credentials:     &credentials.Store{},
		DeliveryTargets: &delivery.Store{},
		MCPRegistry:     &mcpregistry.Store{},
		TeamTemplates:   &fakeTeamTemplateService{},
	}
	recorder := httptest.NewRecorder()
	ctx := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/v1/ready", nil), recorder)
	if err := server.handleReady(ctx); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusOK || recorder.Body.String() != "{\"status\":\"ready\"}\n" {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestReadinessRouteIsPublic(t *testing.T) {
	e := echo.New()
	server := &Server{Echo: e, Config: &config.Config{JWTSecret: "test-secret"}}
	server.registerRoutes()
	for _, route := range e.Routes() {
		if route.Method == http.MethodGet && route.Path == "/v1/ready" {
			return
		}
	}
	t.Fatal("GET /v1/ready is not registered")
}
