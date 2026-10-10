package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jinyitao123/weave/internal/app/kernelbindings"
	"github.com/jinyitao123/weave/internal/kernel/config"
	"github.com/labstack/echo/v4"
)

func feishuAdminCall(t *testing.T, s *Server, handler echo.HandlerFunc, method, workspace string, param []string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, "/", reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	c := s.Echo.NewContext(req, rec)
	c.Set("tenant", workspace)
	c.Set("user_id", "user")
	if len(param) == 2 {
		c.SetParamNames(param[0])
		c.SetParamValues(param[1])
	}
	if err := handler(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	return rec
}

func decodeFeishuApp(t *testing.T, rec *httptest.ResponseRecorder) feishuAppView {
	t.Helper()
	var view feishuAppView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return view
}

func TestFeishuWorkspaceAppAdminRealPG(t *testing.T) {
	s, pool := newTeamDispatchTestServer(t)
	s.Echo = echo.New()
	s.OrgStore = kernelbindings.NewOrganization(pool)
	s.Config = &config.Config{JWTSecret: "feishu-admin"}
	t.Setenv("WEAVE_SECRET_KEY", strings.Repeat("31", 32))
	t.Setenv("WEAVE_SECRET_KEY_FILE", "")
	provider, _, sent := testFeishuClient(t)
	previousBase := feishuAPIBase
	feishuAPIBase = provider.base
	t.Cleanup(func() { feishuAPIBase = previousBase })
	if _, err := pool.Exec(t.Context(), `INSERT INTO weave_members(workspace_id,user_id,role) VALUES('ws','user','member')`); err != nil {
		t.Fatal(err)
	}

	if view := decodeFeishuApp(t, feishuAdminCall(t, s, s.handleGetFeishuApp, http.MethodGet, "ws", nil, nil)); view.Source != "none" {
		t.Fatalf("unconfigured source = %q", view.Source)
	}
	save := map[string]any{"expectedRevision": 0, "appId": "ws-app", "tenantKey": "tenant", "appSecret": "plain-secret", "verificationToken": "plain-verify", "encryptKey": "plain-encrypt"}
	rec := feishuAdminCall(t, s, s.handlePutFeishuApp, http.MethodPut, "ws", nil, save)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "plain-") {
		t.Fatalf("first save status=%d body=%s", rec.Code, rec.Body.String())
	}
	view := decodeFeishuApp(t, rec)
	if view.Source != "workspace" || view.Revision != 1 || !view.Secrets.AppSecret || !strings.HasPrefix(view.CallbackPath, feishuDeploymentCallback+"/") {
		t.Fatalf("saved view = %+v", view)
	}
	var sealed string
	if err := pool.QueryRow(t.Context(), `SELECT app_secret_sealed||verification_token_sealed||encrypt_key_sealed FROM weave_feishu_apps WHERE workspace_id='ws'`).Scan(&sealed); err != nil || strings.Contains(sealed, "plain-") {
		t.Fatalf("secrets were not sealed: err=%v", err)
	}

	if rec := feishuAdminCall(t, s, s.handlePutFeishuApp, http.MethodPut, "ws", nil, save); rec.Code != http.StatusConflict {
		t.Fatalf("stale revision status = %d", rec.Code)
	}
	if rec := feishuAdminCall(t, s, s.handlePutFeishuApp, http.MethodPut, "ws", nil, map[string]any{"expectedRevision": 1, "appId": "other-app", "tenantKey": "tenant"}); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("app change without secrets status = %d", rec.Code)
	}
	if rec := feishuAdminCall(t, s, s.handlePutFeishuApp, http.MethodPut, "ws", nil, map[string]any{"expectedRevision": 1, "appId": "ws-app", "tenantKey": "tenant", "appSecret": ""}); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty secret status = %d", rec.Code)
	}
	if view := decodeFeishuApp(t, feishuAdminCall(t, s, s.handlePutFeishuApp, http.MethodPut, "ws", nil, map[string]any{"expectedRevision": 1, "appId": "ws-app", "tenantKey": "tenant"})); view.Revision != 2 {
		t.Fatalf("metadata save revision = %d", view.Revision)
	}
	other := map[string]any{"expectedRevision": 0, "appId": "ws-app", "tenantKey": "tenant", "appSecret": "a", "verificationToken": "b", "encryptKey": "c"}
	if rec := feishuAdminCall(t, s, s.handlePutFeishuApp, http.MethodPut, "ws2", nil, other); rec.Code != http.StatusConflict {
		t.Fatalf("second workspace reusing the app status = %d", rec.Code)
	}

	rec = feishuAdminCall(t, s, s.handleCheckFeishuApp, http.MethodPost, "ws", nil, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("check status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Events reach the workspace app only through its own callback key.
	stored := newFeishuClient("ws", "ws-app", "plain-secret", "plain-verify", "plain-encrypt", "tenant")
	callback := strings.TrimPrefix(decodeFeishuApp(t, feishuAdminCall(t, s, s.handleGetFeishuApp, http.MethodGet, "ws", nil, nil)).CallbackPath, feishuDeploymentCallback+"/")
	deliver := func(key string, client *feishuClient, id string) int {
		rec := httptest.NewRecorder()
		c := s.Echo.NewContext(signedFeishuRequest(client, testFeishuMessage(client, id, "open-ws", "团队"), true), rec)
		c.SetParamNames("callback")
		c.SetParamValues(key)
		if err := s.handleFeishuWorkspaceEvent(c); err != nil {
			t.Fatal(err)
		}
		return rec.Code
	}
	if code := deliver(strings.Repeat("0", 32), stored, "unknown-key"); code != http.StatusNotFound {
		t.Fatalf("unknown callback status = %d", code)
	}
	if code := deliver(callback, stored, "ws-message"); code != http.StatusOK {
		t.Fatalf("workspace callback status = %d", code)
	}
	if app, err := s.feishuForWorkspace(t.Context(), "ws"); err != nil || app.appID != "ws-app" {
		t.Fatalf("workspace app = %v, %v", app, err)
	}

	// The deployment app stops serving a workspace that saved its own app.
	s.Feishu = provider
	if _, err := pool.Exec(t.Context(), `INSERT INTO weave_feishu_links(app_id,workspace_id,user_id,open_id,expires_at) VALUES('app','ws','user','open-old',now()+interval '1 hour')`); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	if err := s.handleFeishuEvent(s.Echo.NewContext(signedFeishuRequest(provider, testFeishuMessage(provider, "old-message", "open-old", "团队"), true), rec)); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("deployment callback status=%d err=%v", rec.Code, err)
	}
	for range 2 {
		if _, err := s.sweepFeishuCommand(t.Context(), s.Feishu); err != nil {
			t.Fatal(err)
		}
	}
	if len(*sent) == 0 || !strings.Contains((*sent)[len(*sent)-1], "已改用自己的飞书应用") {
		t.Fatalf("deployment reply = %v", *sent)
	}

	rec = feishuAdminCall(t, s, s.handleDeleteFeishuApp, http.MethodDelete, "ws", nil, map[string]any{"expectedRevision": 2})
	if view := decodeFeishuApp(t, rec); rec.Code != http.StatusOK || view.Source != "deployment" {
		t.Fatalf("delete status=%d view=%+v", rec.Code, view)
	}
	var audits int
	var leaked bool
	if err := pool.QueryRow(t.Context(), `SELECT count(*),bool_or(fields::text LIKE '%plain-%') FROM weave_feishu_app_audit WHERE workspace_id='ws'`).Scan(&audits, &leaked); err != nil || audits != 4 || leaked {
		t.Fatalf("audit rows=%d leaked=%v err=%v", audits, leaked, err)
	}
}

func TestFeishuTeamAccessGatesTeamListRealPG(t *testing.T) {
	s, pool := newTeamDispatchTestServer(t)
	s.Echo = echo.New()
	s.OrgStore = kernelbindings.NewOrganization(pool)
	s.Config = &config.Config{JWTSecret: "feishu-access"}
	f, _, sent := testFeishuClient(t)
	s.Feishu = f
	if _, err := pool.Exec(t.Context(), `INSERT INTO weave_members(workspace_id,user_id,role) VALUES('ws','user','member');
 UPDATE weave_teams SET display_name='资料团队',default_workflow_id='flow' WHERE workspace_id='ws' AND id='team';
 INSERT INTO weave_feishu_links(app_id,workspace_id,user_id,open_id,expires_at) VALUES('app','ws','user','open-user',now()+interval '1 hour')`); err != nil {
		t.Fatal(err)
	}
	ask := func(id string) string {
		t.Helper()
		rec := httptest.NewRecorder()
		if err := s.handleFeishuEvent(s.Echo.NewContext(signedFeishuRequest(f, testFeishuMessage(f, id, "open-user", "团队"), true), rec)); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("callback status=%d err=%v", rec.Code, err)
		}
		for range 2 {
			if _, err := s.sweepFeishuCommand(t.Context(), s.Feishu); err != nil {
				t.Fatal(err)
			}
		}
		return (*sent)[len(*sent)-1]
	}
	if reply := ask("before"); !strings.Contains(reply, "暂无开启飞书接入的可用团队") {
		t.Fatalf("team listed before access was enabled: %s", reply)
	}

	get := func() feishuTeamAccess {
		var access feishuTeamAccess
		_ = json.Unmarshal(feishuAdminCall(t, s, s.handleGetFeishuTeamAccess, http.MethodGet, "ws", []string{"id", "team"}, nil).Body.Bytes(), &access)
		return access
	}
	if access := get(); access.Enabled || access.Revision != 0 || !access.Notify.Result || access.WorkflowID != nil {
		t.Fatalf("default access = %+v", access)
	}
	notify := map[string]bool{"result": true, "revisionRequired": true, "humanReview": false, "failure": true, "cancelled": true}
	if rec := feishuAdminCall(t, s, s.handlePutFeishuTeamAccess, http.MethodPut, "ws", []string{"id", "team"}, map[string]any{"expectedRevision": 0, "enabled": true, "workflowId": "missing", "notify": notify}); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("foreign workflow status = %d", rec.Code)
	}
	if rec := feishuAdminCall(t, s, s.handlePutFeishuTeamAccess, http.MethodPut, "ws", []string{"id", "nobody"}, map[string]any{"expectedRevision": 0, "enabled": true, "notify": notify}); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown team status = %d", rec.Code)
	}
	if rec := feishuAdminCall(t, s, s.handlePutFeishuTeamAccess, http.MethodPut, "ws", []string{"id", "team"}, map[string]any{"expectedRevision": 0, "enabled": true, "notify": notify}); rec.Code != http.StatusOK {
		t.Fatalf("enable status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := feishuAdminCall(t, s, s.handlePutFeishuTeamAccess, http.MethodPut, "ws", []string{"id", "team"}, map[string]any{"expectedRevision": 0, "enabled": false, "notify": notify}); rec.Code != http.StatusConflict {
		t.Fatalf("stale access status = %d", rec.Code)
	}
	if access := get(); !access.Enabled || access.Revision != 1 || access.Notify.HumanReview {
		t.Fatalf("saved access = %+v", access)
	}
	if reply := ask("after"); !strings.Contains(reply, "资料团队") {
		t.Fatalf("enabled team missing from list: %s", reply)
	}
	var audits int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM weave_feishu_team_access_audit WHERE workspace_id='ws' AND team_id='team'`).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("access audits=%d err=%v", audits, err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE weave_employee_run_event_outbox SET feishu_state='suppressed' WHERE false`); err != nil {
		t.Fatalf("suppressed state rejected: %v", err)
	}
}

var errFeishuAPI = errors.New("feishu API code 99991663")

func TestFeishuNotifyKinds(t *testing.T) {
	notify := feishuNotify{Result: true, RevisionRequired: false, HumanReview: true, Failure: false, Cancelled: true}
	for kind, want := range map[string]bool{"result": true, "revision_required": false, "human_review": true, "failure": false, "cancelled": true, "future_kind": true} {
		if notify.allows(kind) != want {
			t.Fatalf("%s allowed = %v", kind, !want)
		}
	}
	if feishuCheckReason(nil) != "" || feishuCheckReason(errFeishuAPI) != "credentials_rejected" || feishuCheckReason(io.EOF) != "unreachable" {
		t.Fatal("check reasons misclassified")
	}
}
