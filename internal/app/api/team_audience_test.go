package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

func TestTeamAudienceRules(t *testing.T) {
	if !teamAvailableTo(nil, nil) || !teamAvailableTo([]string{}, []string{"sales"}) {
		t.Fatal("an empty audience is open to the organization")
	}
	if !teamAvailableTo([]string{"sales", "delivery"}, []string{"member_default", "delivery"}) {
		t.Fatal("any shared permission set grants use")
	}
	if teamAvailableTo([]string{"sales"}, []string{"delivery"}) || teamAvailableTo([]string{"sales"}, nil) {
		t.Fatal("a session without the team's permission sets must not use it")
	}
	if got, err := normalizeTeamAudience([]string{" sales ", "delivery"}); err != nil || len(got) != 2 || got[0] != "sales" {
		t.Fatalf("normalize = %v, %v", got, err)
	}
	for _, invalid := range [][]string{{""}, {"sales", "sales"}, {"a\nb"}} {
		if _, err := normalizeTeamAudience(invalid); err == nil {
			t.Fatalf("audience %q must be rejected", invalid)
		}
	}
}

func TestOnlyForgeEmployeeSessionsAreLimitedByAudience(t *testing.T) {
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

func TestDispatchInputRefusesTeamOutsideAudienceRealPG(t *testing.T) {
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
	if refused := register([]string{"delivery"}); refused.Code != http.StatusForbidden || !strings.Contains(refused.Body.String(), "team_not_available") {
		t.Fatalf("registration outside audience status=%d body=%s", refused.Code, refused.Body.String())
	}
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM weave_dispatch_input_revisions WHERE workspace_id='ws' AND workbench_session_id='audience-session'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("refused registration wrote %d inputs (err %v)", count, err)
	}
	if accepted := register([]string{"sales"}); accepted.Code != http.StatusCreated {
		t.Fatalf("registration inside audience status=%d body=%s", accepted.Code, accepted.Body.String())
	}
}
