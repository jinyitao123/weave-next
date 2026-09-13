package api

import (
	"net/http/httptest"
	"testing"

	"github.com/jinyitao123/weave/internal/app/apikeys"
	"github.com/labstack/echo/v4"
)

func TestWorkbenchHostActorIsOpaqueAndSeparatesApplicationIdentity(t *testing.T) {
	e := echo.New()
	first := e.NewContext(httptest.NewRequest("GET", "/", nil), httptest.NewRecorder())
	first.Request().Header.Set("X-Weave-Actor-ID", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	setAPIKeyContext(first, &apikeys.APIKey{ID: "host", TenantID: "ws", Role: "admin", OwnerUserID: "owner"})
	second := e.NewContext(httptest.NewRequest("GET", "/", nil), httptest.NewRecorder())
	second.Request().Header.Set("X-Weave-Actor-ID", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	setAPIKeyContext(second, &apikeys.APIKey{ID: "host", TenantID: "ws", Role: "admin", OwnerUserID: "owner"})
	if getUserID(first) == getUserID(second) || capabilityApplicationID(first) == capabilityApplicationID(second) {
		t.Fatal("two Workbench browser actors shared capability identity")
	}
	if getUserID(first) != "workbench:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("actor was not propagated: %q", getUserID(first))
	}
}

func TestWorkbenchActorDelegationRequiresAdminHostKey(t *testing.T) {
	e := echo.New()
	c := e.NewContext(httptest.NewRequest("GET", "/", nil), httptest.NewRecorder())
	c.Request().Header.Set("X-Weave-Actor-ID", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	setAPIKeyContext(c, &apikeys.APIKey{ID: "key", TenantID: "ws", Role: "user", OwnerUserID: "owner"})
	if getUserID(c) != "owner" || capabilityApplicationID(c) != "key" {
		t.Fatal("ordinary API key delegated a Workbench actor")
	}
}
