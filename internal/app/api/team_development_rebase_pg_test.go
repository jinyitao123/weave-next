package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/app/agentcatalog"
	orgstore "github.com/jinyitao123/weave/internal/app/org"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/testutil"
	org "github.com/jinyitao123/weave/internal/kernel/orgspec"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/labstack/echo/v4"
)

// A live team change made after the draft was opened (#103: a profile edit)
// stops trials and publishing until the draft takes it in; rebasing takes it
// in without losing the draft's own edits, and refuses to guess when both
// changed the same part.
func TestTeamDevelopmentRebaseTakesInLiveChangesRealPG(t *testing.T) {
	ctx := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "ws", UserID: "developer"})
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES('ws','ws','Workspace'); INSERT INTO weave_users(id,tenant_id,username,password,role) VALUES('developer','ws','developer','unused','admin')`); err != nil {
		t.Fatal(err)
	}
	agents := agentcatalog.New(pool)
	lead := registry.AgentRecord{Name: "lead", DisplayName: "负责人", Role: "avatar", Engine: "loom", Model: "fixture-model", GraphType: "standard", Spec: stdlib.AgentSpec{SystemPrompt: "Coordinate"}}
	worker := registry.AgentRecord{Name: "worker", DisplayName: "审核员", Role: "worker", Engine: "loom", Model: "fixture-model", GraphType: "standard", Spec: stdlib.AgentSpec{SystemPrompt: "Review"}}
	for _, rec := range []*registry.AgentRecord{&lead, &worker} {
		if err := agents.Put(ctx, "ws", rec); err != nil {
			t.Fatal(err)
		}
	}
	teams := orgstore.NewStore(pool)
	created, err := teams.CreateActiveTeam(ctx, "ws", org.CreateActiveTeamInput{Name: "review", Objective: "审核合同", LeadAvatarID: lead.ID,
		Workers: []org.InitialTeamWorker{{WorkerAgentID: worker.ID, Duty: "审核合同", AllowedKinds: []string{"consult"}, DefaultKind: "consult"}}})
	if err != nil {
		t.Fatal(err)
	}
	teamID := created.Team.ID
	server := &Server{Pool: pool, Registry: agents}
	call := func(body any, handler echo.HandlerFunc) (*httptest.ResponseRecorder, error) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest("POST", "/", bytes.NewReader(raw)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		c := echo.New().NewContext(req, rr)
		c.SetParamNames("id")
		c.SetParamValues(teamID)
		c.Set("tenant", "ws")
		c.Set("user_id", "developer")
		return rr, handler(c)
	}
	read := func(rr *httptest.ResponseRecorder) developmentDraft {
		t.Helper()
		var d developmentDraft
		if err := json.Unmarshal(rr.Body.Bytes(), &d); err != nil {
			t.Fatalf("decode draft %s: %v", rr.Body.String(), err)
		}
		return d
	}
	get := func() developmentDraft {
		t.Helper()
		rr, err := call(nil, server.handleGetTeamDevelopment)
		if err != nil {
			t.Fatal(err)
		}
		return read(rr)
	}
	// gate is the check trials, preparation and publishing all run first.
	gate := func() error {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		d, err := readDevelopment(ctx, tx, "ws", teamID, true)
		if err != nil {
			t.Fatal(err)
		}
		return checkDevelopmentBaseline(ctx, tx, "ws", teamID, d.Baseline)
	}
	editProfile := func(name, objective string) {
		t.Helper()
		var updatedAt time.Time
		if err := pool.QueryRow(ctx, `SELECT updated_at FROM weave_teams WHERE workspace_id='ws' AND id=$1`, teamID).Scan(&updatedAt); err != nil {
			t.Fatal(err)
		}
		if _, err := teams.UpdateTeamProfile(ctx, "ws", teamID, org.UpdateTeamProfileInput{DisplayName: name, Objective: objective, ExpectedUpdatedAt: updatedAt}); err != nil {
			t.Fatal(err)
		}
	}
	rebase := func(revision int64, keepDraft bool) (*httptest.ResponseRecorder, error) {
		t.Helper()
		return call(map[string]any{"expected_revision": revision, "keep_draft": keepDraft}, server.handleRebaseTeamDevelopment)
	}
	baselineCode := func(err error) string {
		var httpErr *echo.HTTPError
		if !errors.As(err, &httpErr) {
			return ""
		}
		fields, _ := httpErr.Message.(map[string]string)
		return fields["error"]
	}

	// The draft edits a member; then the profile changes live.
	d := get()
	if d.BaselineChanged {
		t.Fatal("a fresh draft reports a changed baseline")
	}
	for i := range d.Document.Members {
		if d.Document.Members[i].Configuration.Role == "worker" {
			d.Document.Members[i].Configuration.SystemPrompt = "逐条核对原文"
		}
	}
	if _, err := call(map[string]any{"expected_revision": d.Revision, "document": d.Document}, server.handleSaveTeamDevelopment); err != nil {
		t.Fatal(err)
	}
	editProfile("合同审核组", "审核合同并标注风险")
	if code := baselineCode(gate()); code != "development_baseline_changed" {
		t.Fatalf("live profile edit left the gate open or failed differently: %q", code)
	}
	d = get()
	if !d.BaselineChanged {
		t.Fatal("draft does not report the live change")
	}

	// A stale revision is refused; the current one takes the change in.
	if _, err := rebase(d.Revision-1, false); err == nil {
		t.Fatal("rebase accepted a stale revision")
	}
	rr, err := rebase(d.Revision, false)
	if err != nil || rr.Code != 200 {
		t.Fatalf("rebase: %v %s", err, rr.Body.String())
	}
	rebased := read(rr)
	if rebased.Revision != d.Revision+1 || rebased.BaselineChanged || rebased.Document.Name != "合同审核组" || rebased.Document.Objective != "审核合同并标注风险" {
		t.Fatalf("rebased draft = revision %d changed=%v name=%q objective=%q", rebased.Revision, rebased.BaselineChanged, rebased.Document.Name, rebased.Document.Objective)
	}
	kept := false
	for _, member := range rebased.Document.Members {
		kept = kept || member.Configuration.SystemPrompt == "逐条核对原文"
	}
	if !kept {
		t.Fatal("rebase dropped the draft's own member edit")
	}
	taken := map[string]bool{}
	for _, change := range rebased.RebaseChanges {
		if change.Kind == "team" && change.Action == "taken" {
			taken[change.Field] = true
		}
	}
	if !taken["name"] || !taken["objective"] || len(rebased.RebaseChanges) != 2 {
		t.Fatalf("rebase changes = %+v", rebased.RebaseChanges)
	}
	if err := gate(); err != nil {
		t.Fatalf("rebased draft is still blocked: %v", err)
	}
	// Rebasing again with nothing new live changes nothing.
	if rr, err := rebase(rebased.Revision, false); err != nil || read(rr).Revision != rebased.Revision {
		t.Fatalf("no-op rebase moved the draft: %v", err)
	}

	// Both sides change the objective: no guessing, unless the draft's edit is
	// kept on request.
	rebased.Document.Objective = "草稿里的目标"
	if _, err := call(map[string]any{"expected_revision": rebased.Revision, "document": rebased.Document}, server.handleSaveTeamDevelopment); err != nil {
		t.Fatal(err)
	}
	editProfile("合同审核组", "线上的目标")
	d = get()
	rr, err = rebase(d.Revision, false)
	if err != nil || rr.Code != 409 {
		t.Fatalf("conflicting rebase: %v %d %s", err, rr.Code, rr.Body.String())
	}
	var conflict struct {
		Error     string                      `json:"error"`
		Conflicts []developmentRebaseConflict `json:"conflicts"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &conflict); err != nil || conflict.Error != "development_rebase_conflict" ||
		len(conflict.Conflicts) != 1 || conflict.Conflicts[0].Field != "objective" {
		t.Fatalf("conflict reply = %s", rr.Body.String())
	}
	if after := get(); after.Revision != d.Revision || after.Document.Objective != "草稿里的目标" || !after.BaselineChanged {
		t.Fatal("a refused rebase changed the draft")
	}
	rr, err = rebase(d.Revision, true)
	if err != nil || rr.Code != 200 {
		t.Fatalf("keep-draft rebase: %v %s", err, rr.Body.String())
	}
	if resolved := read(rr); resolved.Document.Objective != "草稿里的目标" || resolved.BaselineChanged ||
		len(resolved.RebaseChanges) != 1 || resolved.RebaseChanges[0].Action != "kept" {
		t.Fatalf("keep-draft rebase = objective %q changes %+v", resolved.Document.Objective, resolved.RebaseChanges)
	}
	if err := gate(); err != nil {
		t.Fatalf("resolved draft is still blocked: %v", err)
	}
	// Rebasing never writes the live team.
	var liveObjective string
	if err := pool.QueryRow(ctx, `SELECT objective FROM weave_teams WHERE workspace_id='ws' AND id=$1`, teamID).Scan(&liveObjective); err != nil || liveObjective != "线上的目标" {
		t.Fatalf("live objective = %q (%v)", liveObjective, err)
	}
}

func TestMergeDevelopmentDocumentsHandlesMembersAndFlows(t *testing.T) {
	member := func(id, prompt string) developmentMember {
		m := developmentMember{ID: id}
		m.Configuration.DisplayName = id
		m.Configuration.SystemPrompt = prompt
		return m
	}
	flow := func(id, name string) developmentWorkflow {
		return developmentWorkflow{ID: id, Name: name, Graph: json.RawMessage(`{"a": 1,"b":2}`), Trigger: json.RawMessage(`{}`)}
	}
	base := developmentDocument{Name: "t", Members: []developmentMember{member("lead", "x"), member("kept", "x"), member("gone", "x")}, Workflows: []developmentWorkflow{flow("f", "flow")}}
	draft := base
	draft.Members = []developmentMember{member("lead", "x"), member("kept", "draft edit"), member("gone", "x"), member("draft-new", "x")}
	// The same graph spelled differently is not a change.
	draft.Workflows = []developmentWorkflow{{ID: "f", Name: "flow", Graph: json.RawMessage(`{"b":2,"a":1}`), Trigger: json.RawMessage(`{}`)}}
	live := base
	live.Members = []developmentMember{member("lead", "live edit"), member("kept", "x"), member("live-new", "x")}
	live.Workflows = []developmentWorkflow{flow("f", "flow")}
	merged, changes, conflicts := mergeDevelopmentDocuments(base, draft, live, false)
	if len(conflicts) != 0 {
		t.Fatalf("conflicts = %+v", conflicts)
	}
	got := map[string]string{}
	for _, m := range merged.Members {
		got[m.ID] = m.Configuration.SystemPrompt
	}
	if len(got) != 4 || got["lead"] != "live edit" || got["kept"] != "draft edit" || got["draft-new"] != "x" || got["live-new"] != "x" {
		t.Fatalf("merged members = %v", got)
	}
	if len(changes) != 3 { // lead taken, gone removed, live-new added
		t.Fatalf("changes = %+v", changes)
	}
	// A member deleted live that the draft still edits cannot be kept.
	draft.Members[2] = member("gone", "draft edit")
	_, _, conflicts = mergeDevelopmentDocuments(base, draft, live, true)
	if len(conflicts) != 1 || !conflicts[0].LiveRemoved || conflicts[0].ID != "gone" {
		t.Fatalf("live-removed conflict = %+v", conflicts)
	}
}
