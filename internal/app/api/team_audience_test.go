package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jinyitao123/weave/internal/app/apikeys"
	"github.com/jinyitao123/weave/internal/app/users"
	"github.com/labstack/echo/v4"
)

func TestTeamAudienceRules(t *testing.T) {
	if got, err := normalizeTeamAudience([]string{" sales ", "delivery"}); err != nil || len(got) != 2 || got[0] != "sales" {
		t.Fatalf("normalize = %v, %v", got, err)
	}
	for _, invalid := range [][]string{{""}, {"sales", "sales"}, {"a\nb"}} {
		if _, err := normalizeTeamAudience(invalid); err == nil {
			t.Fatalf("audience %q must be rejected", invalid)
		}
	}
}

func TestForgeEmployeeSessionCarriesPermissionSets(t *testing.T) {
	c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), httptest.NewRecorder())
	if _, employee := forgeEmployeeSession(c); employee {
		t.Fatal("operator and API-key callers are not employees")
	}
	c.Set(identitySourceContextKey, "forge")
	c.Set(forgePermissionSetsContextKey, []string{"sales"})
	sets, employee := forgeEmployeeSession(c)
	if !employee || len(sets) != 1 || sets[0] != "sales" {
		t.Fatalf("forge session = %v, %v", sets, employee)
	}
}

func TestStoredAudienceDoesNotLimitTeamUseRealPG(t *testing.T) {
	server, pool := newTeamDispatchTestServer(t)
	if _, err := pool.Exec(t.Context(), `UPDATE weave_teams SET audience='["sales"]'::jsonb WHERE workspace_id='ws' AND id='team'`); err != nil {
		t.Fatal(err)
	}
	keyStore, userStore := apikeys.NewStore(pool), users.NewStore(pool)
	_, rawKey, err := keyStore.Create(t.Context(), "ws", "delegation-host", "admin", "user", []string{"org"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	claims := Claims{TenantID: "ws", UserID: "user-other", Roles: []string{"member"},
		IdentitySource: "forge", PermissionSets: []string{"delivery"},
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}}
	proof, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte("delegation-test-secret"))
	if err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	e.GET("/v1/teams/:id/audience", func(c echo.Context) error {
		if ok, err := server.ensureTeamAvailable(c, getTenant(c), c.Param("id")); !ok {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	}, AuthMiddleware("delegation-test-secret", func() *apikeys.Store { return keyStore }, func() *users.Store { return userStore }), RequireScope("org"))
	for _, delegated := range []bool{true, false} {
		request := httptest.NewRequest(http.MethodGet, "/v1/teams/team/audience", nil)
		request.Header.Set("Authorization", "Bearer "+rawKey)
		if delegated {
			request.Header.Set("X-Weave-User-Authorization", "Bearer "+proof)
		}
		response := httptest.NewRecorder()
		e.ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			t.Fatalf("stored audience still limited the team (delegated=%v): %d %s", delegated, response.Code, response.Body.String())
		}
	}
}

func TestDispatchInputIgnoresStoredAudienceRealPG(t *testing.T) {
	server, pool := newTeamDispatchTestServer(t)
	if _, err := pool.Exec(t.Context(), `INSERT INTO weave_external_identities(issuer,subject,workspace_id,user_id,native_organization)
		VALUES('forge:audience-test','native-user','ws','user','native-org')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE weave_teams SET audience='["sales"]'::jsonb WHERE workspace_id='ws' AND id='team'`); err != nil {
		t.Fatal(err)
	}
	register := func(permissionSets []string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(dispatchInputRegistrationFixture("audience-session", "检查材料", ""))
		c, recorder := dispatchInputTestContext(body, "/v1/workbench/dispatch-inputs", "ws", "user")
		c.Set(identitySourceContextKey, "forge")
		c.Set(forgePermissionSetsContextKey, permissionSets)
		if err := server.handleRegisterDispatchInput(c); err != nil {
			t.Fatal(err)
		}
		return recorder
	}
	if accepted := register([]string{"delivery"}); accepted.Code != http.StatusCreated {
		t.Fatalf("registration with a stored audience status=%d body=%s", accepted.Code, accepted.Body.String())
	}
}
