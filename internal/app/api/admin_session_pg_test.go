package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jinyitao123/weave/internal/app/apikeys"
	"github.com/jinyitao123/weave/internal/app/users"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/testutil"
)

func TestAdminConsoleAPIKeySignInRealPG(t *testing.T) {
	ctx := context.Background()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	workspaceID := "admin-console-" + uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES($1,$1,$1)`, workspaceID); err != nil {
		t.Fatal(err)
	}
	userStore := users.NewStore(pool)
	owner, err := userStore.Create(ctx, workspaceID, "operator-"+uuid.NewString()[:8], "unused-password-1", "运维小王", "admin")
	if err != nil {
		t.Fatal(err)
	}
	keys := apikeys.NewStore(pool)
	_, adminKey, err := keys.Create(ctx, workspaceID, "weave-bootstrap", "admin", owner.ID, apikeys.BootstrapScopes(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, runsKey, err := keys.Create(ctx, workspaceID, "runs-only", "admin", owner.ID, []string{"runs"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	s := adminConsoleServer(nil)
	s.KeyStore, s.UserStore = keys, userStore
	trusted := func(r *http.Request) { r.Header.Set(adminRequestHeader, "1") }
	signIn := func(key string) (*http.Cookie, adminSessionView) {
		t.Helper()
		recorder := serveAdmin(s, adminRequest(http.MethodPost, "/v1/admin/session", `{"api_key":"`+key+`"}`, trusted))
		if recorder.Code != http.StatusOK {
			t.Fatalf("sign-in = %d %s", recorder.Code, recorder.Body.String())
		}
		var view adminSessionView
		if err := json.Unmarshal(recorder.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		for _, cookie := range recorder.Result().Cookies() {
			if cookie.Name == adminSessionCookie {
				return cookie, view
			}
		}
		t.Fatal("no session cookie")
		return nil, view
	}
	readSession := func(cookie *http.Cookie) adminSessionView {
		t.Helper()
		recorder := serveAdmin(s, adminRequest(http.MethodGet, "/v1/admin/session", "", func(r *http.Request) { r.AddCookie(cookie) }))
		if recorder.Code != http.StatusOK {
			t.Fatalf("session read = %d %s", recorder.Code, recorder.Body.String())
		}
		var view adminSessionView
		if err := json.Unmarshal(recorder.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		return view
	}

	// The bootstrap administrator key becomes a session of its owner, so the
	// console is not limited to the operator key's narrow scope list.
	adminCookie, signedIn := signIn(adminKey)
	if strings.HasPrefix(adminCookie.Value, "wv_sk_") || signedIn.Name != "运维小王" || signedIn.Source != "api_key" {
		t.Fatalf("admin key session = %q %#v", adminCookie.Value[:6], signedIn)
	}
	if view := readSession(adminCookie); view.Name != "运维小王" || view.Role != "admin" || view.Source != "api_key" {
		t.Fatalf("admin key session read = %#v", view)
	}
	agents := serveAdmin(s, adminRequest(http.MethodPost, "/v1/agents", `{}`, func(r *http.Request) { trusted(r); r.AddCookie(adminCookie) }))
	if agents.Code == http.StatusForbidden || agents.Code == http.StatusUnauthorized {
		t.Fatalf("admin console session refused by agent scope: %d %s", agents.Code, agents.Body.String())
	}

	// A narrower key keeps its own scopes.
	runsCookie, _ := signIn(runsKey)
	if runsCookie.Value != runsKey {
		t.Fatal("scoped key was widened into an owner session")
	}
	if view := readSession(runsCookie); view.Source != "api_key" || view.Name != "runs-only" {
		t.Fatalf("scoped key session read = %#v", view)
	}
	refused := serveAdmin(s, adminRequest(http.MethodPost, "/v1/agents", `{}`, func(r *http.Request) { trusted(r); r.AddCookie(runsCookie) }))
	if refused.Code != http.StatusForbidden {
		t.Fatalf("scoped key reached the agent route: %d", refused.Code)
	}
}
