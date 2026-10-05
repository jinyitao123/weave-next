package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

func chatRequestContext(t *testing.T) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx := echo.New().NewContext(httptest.NewRequest(http.MethodPost, "/v1/teams/test/dispatch", nil), recorder)
	ctx.Request().Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	return ctx, recorder
}

// Both refusals must happen before any registry access: a Server without a
// registry would panic if the request got that far.
func TestChatRefusesPlatformInternalAgentsAndRetiredIntentsBeforeRegistryLookup(t *testing.T) {
	for _, agent := range []string{
		"__team_architect", "__config_engineer", "__graph_designer_tf", "__eval_debugger",
		"__semantic_judge", "__blueprint_patch_planner", "__graph_designer", "__any_future_platform_agent",
	} {
		t.Run(agent, func(t *testing.T) {
			ctx, recorder := chatRequestContext(t)
			if err := (&Server{}).handleChatRequest(ctx, ChatRequest{Agent: agent, Message: "run", Stream: true}); err != nil {
				t.Fatal(err)
			}
			if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), `"code":"platform_internal_agent"`) {
				t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
			}
		})
	}
	for _, intent := range []string{"create_team", "optimize_team", "anything"} {
		t.Run("intent "+intent, func(t *testing.T) {
			ctx, recorder := chatRequestContext(t)
			if err := (&Server{}).handleChatRequest(ctx, ChatRequest{Agent: "ordinary-agent", Message: "run", Intent: intent, Stream: true}); err != nil {
				t.Fatal(err)
			}
			if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "invalid_conversation_intent") {
				t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestPlatformInternalAgentGateMatchesOnlyTheReservedPrefix(t *testing.T) {
	for _, name := range []string{"__graph_designer", "__another_platform_agent", "__"} {
		if !isPlatformInternalAgent(name) {
			t.Errorf("isPlatformInternalAgent(%q) = false", name)
		}
	}
	for _, name := range []string{"ordinary-agent", "a__b", "_single", "", "team__architect"} {
		if isPlatformInternalAgent(name) {
			t.Errorf("isPlatformInternalAgent(%q) = true", name)
		}
	}
}
