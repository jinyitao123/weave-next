package api

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/admissionfence"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/app/apikeys"
	"github.com/jinyitao123/weave/internal/app/users"
	"github.com/jinyitao123/weave/internal/kernel/config"
	"github.com/labstack/echo/v4"
)

func TestAccessChangeAuthenticatedUserMembershipAndWorkflowAdmissionRealPG(t *testing.T) {
	s, pool := newTeamDispatchTestServer(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO weave_members(workspace_id,user_id,role)VALUES('ws','user','owner'),('ws','user-other','member')`); err != nil {
		t.Fatal(err)
	}
	s.UserStore = users.NewStore(pool)
	s.KeyStore = apikeys.NewStore(pool)
	s.Config = &config.Config{JWTSecret: "isolated-access-test"}
	e := echo.New()
	e.Use(AuthMiddleware(s.Config.JWTSecret, func() *apikeys.Store { return s.KeyStore }, func() *users.Store { return s.UserStore }))
	admin := RequireAnyRole("admin", "owner")
	e.DELETE("/members/:userID", s.handleRemoveMember, admin)
	e.POST("/members", s.handleAddMember, admin)
	e.PUT("/users/:id", s.handleUpdateUser, admin)
	e.PUT("/flows/:id/versions/:version/admission", s.handlePutWorkflowAdmission, admin)
	e.POST("/teams/:id/dispatch", s.handleDispatchTeam)
	token, err := s.signJWT("ws", "user", []string{"admin"})
	if err != nil {
		t.Fatal(err)
	}
	member, err := s.signJWT("ws", "user-other", []string{"member"})
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, key, authorization string, body any, want int) *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+authorization)
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("%s %s = %d want %d body=%s", method, path, rec.Code, want, rec.Body.String())
		}
		return rec
	}
	request(http.MethodDelete, "/members/user-other", "", token, nil, http.StatusBadRequest)
	request(http.MethodDelete, "/members/user-other", "forged", member, map[string]string{"user_id": "user"}, http.StatusForbidden)
	var operations int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM weave_access_change_operations`).Scan(&operations); err != nil || operations != 0 {
		t.Fatalf("rejected request wrote intent: %d %v", operations, err)
	}
	request(http.MethodDelete, "/members/user-other", "remove-member", token, nil, http.StatusNoContent)
	request(http.MethodDelete, "/members/user-other", "remove-member", token, nil, http.StatusNoContent)
	request(http.MethodPost, "/teams/team/dispatch", "", member, teamDispatchRequest{Task: "member task", ClientRequestID: "00000000-0000-0000-0000-000000000701"}, http.StatusConflict)
	request(http.MethodPut, "/users/user-other", "enable-account", token, UpdateUserRequest{DisplayName: "Other", Role: "member", Disabled: false}, http.StatusNoContent)
	request(http.MethodPost, "/teams/team/dispatch", "", member, teamDispatchRequest{Task: "member task", ClientRequestID: "00000000-0000-0000-0000-000000000702"}, http.StatusConflict)
	request(http.MethodPost, "/members", "new-member-authorization", token, memberRequest{UserID: "user-other", Role: "member"}, http.StatusNoContent)
	request(http.MethodPost, "/teams/team/dispatch", "", member, teamDispatchRequest{Task: "member task", ClientRequestID: "00000000-0000-0000-0000-000000000703"}, http.StatusCreated)
	yes, no := true, false
	request(http.MethodPut, "/flows/flow/versions/1/admission", "block-version", token, admissionChangeRequest{DesiredBlocked: &yes, Reason: "withdraw version"}, http.StatusOK)
	request(http.MethodPost, "/teams/team/dispatch", "", member, teamDispatchRequest{Task: "member task", ClientRequestID: "00000000-0000-0000-0000-000000000704"}, http.StatusConflict)
	request(http.MethodPut, "/flows/flow/versions/1/admission", "new-version-authorization", token, admissionChangeRequest{DesiredBlocked: &no, Reason: "new approval"}, http.StatusOK)
	request(http.MethodPost, "/teams/team/dispatch", "", member, teamDispatchRequest{Task: "member task", ClientRequestID: "00000000-0000-0000-0000-000000000705"}, http.StatusCreated)
	var users int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM weave_task_queue WHERE actor_subject->>'user_id'='user-other'`).Scan(&users); err != nil || users != 2 {
		t.Fatalf("execution subject or revoked task leaked: %d %v", users, err)
	}
}

func TestAccessChangeRosterScopesKindsAndRejectsInvalidMutationBeforeFencingRealPG(t *testing.T) {
	s, pool := newTeamDispatchTestServer(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO weave_agents(id,workspace_id,name,role,spec)VALUES('worker','ws','worker','worker','{}');INSERT INTO weave_team_workers(workspace_id,team_id,worker_agent_id,allowed_kinds,default_kind)VALUES('ws','team','worker',ARRAY['consult','dispatch','handoff'],'consult')`); err != nil {
		t.Fatal(err)
	}
	var updated time.Time
	if err := pool.QueryRow(ctx, `SELECT updated_at FROM weave_teams WHERE id='team'`).Scan(&updated); err != nil {
		t.Fatal(err)
	}
	request := teamRosterRequest{ExpectedUpdatedAt: updated, DesiredTeamStatus: "active", LeadAgentID: "lead", Reason: "limit this relationship", Workers: []registry.TeamRosterWorkerInput{{WorkerAgentID: "worker", AllowedKinds: []string{"consult"}, DefaultKind: "consult", Enabled: true}}}
	invoke := func(key string, r teamRosterRequest, want int) {
		t.Helper()
		raw, _ := json.Marshal(r)
		httpRequest := httptest.NewRequest(http.MethodPut, "/teams/team/roster", bytes.NewReader(raw))
		httpRequest.Header.Set("Content-Type", "application/json")
		httpRequest.Header.Set("Idempotency-Key", key)
		httpRequest = httpRequest.WithContext(execution.WithSubject(ctx, execution.Subject{WorkspaceID: "ws", UserID: "user"}))
		rec := httptest.NewRecorder()
		c := echo.New().NewContext(httpRequest, rec)
		c.SetParamNames("id")
		c.SetParamValues("team")
		c.Set("tenant", "ws")
		c.Set("user_id", "user")
		err := s.handleUpdateTeamRoster(c)
		if err != nil || rec.Code != want {
			t.Fatalf("roster %s = %d want %d %s error=%v", key, rec.Code, want, rec.Body.String(), err)
		}
	}
	invoke("restrict-worker-kinds", request, http.StatusOK)
	base := admissionfence.Worker("team", "worker")
	for _, kind := range []string{"consult", "dispatch", "handoff"} {
		var blocked bool
		if err := pool.QueryRow(ctx, `SELECT blocked FROM weave_resource_admission_fences WHERE workspace_id='ws' AND resource_kind='team_worker' AND resource_id=$1 AND resource_version=$2`, base.ID, kind).Scan(&blocked); err != nil || blocked != (kind != "consult") {
			t.Fatalf("kind %s blocked=%v err=%v", kind, blocked, err)
		}
	}
	if err := pool.QueryRow(ctx, `SELECT updated_at FROM weave_teams WHERE id='team'`).Scan(&request.ExpectedUpdatedAt); err != nil {
		t.Fatal(err)
	}
	invalid := request
	invalid.Workers = append([]registry.TeamRosterWorkerInput{}, request.Workers...)
	invalid.Workers[0].Enabled = false
	invoke("invalid-last-worker", invalid, http.StatusConflict)
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM weave_access_change_operations WHERE operation_id='invalid-last-worker'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("invalid roster retained pending intent %d %v", n, err)
	}
	request.DesiredTeamStatus = "archived"
	request.Workers[0].Enabled = false
	request.Reason = "archive team"
	invoke("archive-team", request, http.StatusOK)
	var blocked bool
	if err := pool.QueryRow(ctx, `SELECT blocked FROM weave_resource_admission_fences WHERE workspace_id='ws' AND resource_kind='team' AND resource_id='team' AND resource_version='*'`).Scan(&blocked); err != nil || !blocked {
		t.Fatalf("archived team not fenced %v %v", blocked, err)
	}
}
