package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

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

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
