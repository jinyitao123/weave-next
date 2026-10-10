package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jinyitao123/weave/internal/app/users"
	"github.com/labstack/echo/v4"
)

func catalogBody(version string, capabilities ...map[string]any) string {
	raw, _ := json.Marshal(map[string]any{"version": version, "provider": map[string]string{"id": "forge"}, "refreshedAt": "2026-10-09T00:00:00Z", "capabilities": capabilities})
	return string(raw)
}

func catalogAction(id, mode, status string) map[string]any {
	return map[string]any{"id": id, "name": "动作 " + id, "description": "说明", "effect": "write", "executionMode": mode, "status": status,
		"objectName": "forge_sales_lead", "actionName": "convert", "requiresRecord": true, "unexpected": "dropped",
		"params": []map[string]any{{"name": "material_file_ids", "label": "全部材料", "type": "file", "multiple": true, "required": true}}}
}

func TestBusinessCatalogSnapshotRealPG(t *testing.T) {
	s, pool := newTeamDispatchTestServer(t)
	s.Echo = echo.New()
	var seenAuth atomic.Value
	var next atomic.Value
	next.Store(catalogBody("v1", catalogAction("forge:action:a.one", "team_delegable", "available"), catalogAction("forge:action:a.two", "employee_only", "available")))
	status := atomic.Int32{}
	status.Store(200)
	forge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != businessCatalogPath {
			t.Errorf("catalog path = %s", r.URL.Path)
		}
		seenAuth.Store(r.Header.Get("Authorization"))
		w.WriteHeader(int(status.Load()))
		_, _ = w.Write([]byte(next.Load().(string)))
	}))
	t.Cleanup(forge.Close)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE weave_users SET display_name='开发者小周' WHERE id='user' AND tenant_id='ws'`); err != nil {
		t.Fatal(err)
	}
	read := func() businessCapabilitiesView {
		t.Helper()
		rec := httptest.NewRecorder()
		c := s.Echo.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
		c.Set("tenant", "ws")
		if err := s.handleGetBusinessCapabilities(c); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("read status=%d err=%v", rec.Code, err)
		}
		var view businessCapabilitiesView
		if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		return view
	}

	if view := read(); view.Available || len(view.Capabilities) != 0 || string(mustJSON(t, view.Capabilities)) != "[]" {
		t.Fatalf("empty view = %+v", view)
	}
	if err := s.refreshBusinessCatalog(ctx, forge.URL, "forge-session-secret", "ws", "user"); err != nil {
		t.Fatal(err)
	}
	if got, _ := seenAuth.Load().(string); got != "Bearer forge-session-secret" {
		t.Fatalf("catalog request auth = %q", got)
	}
	view := read()
	if !view.Available || view.Version != "v1" || view.FetchedBy != "开发者小周" || len(view.Capabilities) != 2 {
		t.Fatalf("snapshot = %+v", view)
	}
	if view.Capabilities[1].ExecutionMode != "employee_only" || len(view.Capabilities[0].Params) != 1 || !view.Capabilities[0].Params[0].Multiple {
		t.Fatalf("capability content = %+v", view.Capabilities)
	}
	var stored string
	if err := pool.QueryRow(ctx, `SELECT row_to_json(snapshot)::text FROM weave_business_capability_catalog snapshot WHERE workspace_id='ws'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, "forge-session-secret") || strings.Contains(stored, "unexpected") {
		t.Fatalf("snapshot kept the token or an unknown field: %s", stored)
	}

	// A refused or invalid read keeps the previous snapshot.
	status.Store(401)
	if err := s.refreshBusinessCatalog(ctx, forge.URL, "forge-session-secret", "ws", "user"); err == nil {
		t.Fatal("refused catalog was accepted")
	}
	status.Store(200)
	next.Store(catalogBody("v2", catalogAction("forge:action:a.one", "sometimes", "available")))
	if err := s.refreshBusinessCatalog(ctx, forge.URL, "forge-session-secret", "ws", "user"); err == nil {
		t.Fatal("invalid execution mode was accepted")
	}
	next.Store(catalogBody("v2", catalogAction("forge:action:a.one", "team_delegable", "available"), catalogAction("forge:action:a.one", "team_delegable", "available")))
	if err := s.refreshBusinessCatalog(ctx, forge.URL, "forge-session-secret", "ws", "user"); err == nil {
		t.Fatal("duplicate ids were accepted")
	}
	if view := read(); view.Version != "v1" || len(view.Capabilities) != 2 {
		t.Fatalf("snapshot changed after failed reads: %+v", view)
	}

	// A legacy catalog without executionMode is team-delegable; success replaces the snapshot.
	legacy := catalogAction("forge:action:b.legacy", "", "available")
	delete(legacy, "executionMode")
	next.Store(catalogBody("v3", legacy))
	if err := s.refreshBusinessCatalog(ctx, forge.URL, "forge-session-secret", "ws", "user"); err != nil {
		t.Fatal(err)
	}
	if view := read(); view.Version != "v3" || len(view.Capabilities) != 1 || view.Capabilities[0].ExecutionMode != "team_delegable" {
		t.Fatalf("replaced snapshot = %+v", view)
	}

	// Only developer-capable roles trigger a read; others never reach the source.
	hits := atomic.Int32{}
	counting := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	t.Cleanup(counting.Close)
	s.captureBusinessCatalog(counting.URL, "token", "ws", "user", "member")
	if hits.Load() != 0 {
		t.Fatal("a member sign-in read the catalog")
	}
	if err := s.refreshBusinessCatalog(ctx, "ftp://forge", "token", "ws", "user"); err == nil {
		t.Fatal("unsupported source scheme was accepted")
	}
}

type catalogIdentityStub struct {
	identity ExternalIdentity
	err      error
}

func (v catalogIdentityStub) Verify(context.Context, string) (ExternalIdentity, error) {
	return v.identity, v.err
}

type catalogBinderStub struct{ user *users.User }

func (b catalogBinderStub) BindExternal(context.Context, string, string, string, string, string) (*users.User, error) {
	return b.user, nil
}

func (b catalogBinderStub) BindExternalInOrganization(context.Context, string, string, string, string, string, string) (*users.User, error) {
	return b.user, nil
}

func TestBusinessCatalogRefreshOnRequestRealPG(t *testing.T) {
	s, _ := newTeamDispatchTestServer(t)
	s.Echo = echo.New()
	status := atomic.Int32{}
	status.Store(200)
	forge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fresh-forge-session" {
			t.Errorf("catalog request auth = %q", r.Header.Get("Authorization"))
		}
		w.WriteHeader(int(status.Load()))
		_, _ = w.Write([]byte(catalogBody("v9", catalogAction("forge:action:a.one", "team_delegable", "available"))))
	}))
	t.Cleanup(forge.Close)
	identity := ExternalIdentity{Issuer: "forge:test", BaseURL: forge.URL, Subject: "native-user", Organization: "ws", NativeOrganization: "native-org", AccessRole: "developer"}
	s.ExternalIdentity = catalogIdentityStub{identity: identity}
	s.ExternalIdentityBinder = catalogBinderStub{user: &users.User{ID: "user", TenantID: "ws"}}
	refresh := func(caller, body string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		c := s.Echo.NewContext(request, rec)
		c.Set("tenant", "ws")
		c.Set("user_id", caller)
		if err := s.handleRefreshBusinessCapabilities(c); err != nil {
			t.Fatal(err)
		}
		return rec
	}
	if rec := refresh("user", `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("refresh without a Forge session status=%d", rec.Code)
	}
	// Someone else's Forge session cannot refresh on this console session.
	if rec := refresh("user-other", `{"forge_token":"fresh-forge-session"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("refresh with another account status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec := refresh("user", `{"forge_token":"fresh-forge-session"}`)
	var view businessCapabilitiesView
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &view) != nil || !view.Available || view.Version != "v9" || len(view.Capabilities) != 1 {
		t.Fatalf("refresh status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "fresh-forge-session") {
		t.Fatal("refresh returned the Forge session")
	}
	// A failed read reports it and keeps the snapshot.
	status.Store(503)
	if rec := refresh("user", `{"forge_token":"fresh-forge-session"}`); rec.Code != http.StatusBadGateway {
		t.Fatalf("failed refresh status=%d", rec.Code)
	}
	// A member-level Forge account cannot read the developer catalog.
	identity.AccessRole = "member"
	s.ExternalIdentity = catalogIdentityStub{identity: identity}
	if rec := refresh("user", `{"forge_token":"fresh-forge-session"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("member refresh status=%d", rec.Code)
	}
	s.ExternalIdentity = catalogIdentityStub{err: context.Canceled}
	if rec := refresh("user", `{"forge_token":"fresh-forge-session"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unverified refresh status=%d", rec.Code)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
