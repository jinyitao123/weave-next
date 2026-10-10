package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jinyitao123/weave/internal/app/kernelbindings"
	"github.com/jinyitao123/weave/internal/kernel/config"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/labstack/echo/v4"
)

func TestFeishuPrivateDispatchRevisionAndIsolationRealPG(t *testing.T) {
	s, pool := newTeamDispatchTestServer(t)
	s.Echo = echo.New()
	s.OrgStore = kernelbindings.NewOrganization(pool)
	s.Config = &config.Config{JWTSecret: "feishu-test"}
	f, tokens, sent := testFeishuClient(t)
	s.Feishu = f
	s.teamRunActivities = &teamrun.PGActivityStore{Transactions: pool}
	_, err := pool.Exec(t.Context(), `INSERT INTO weave_members(workspace_id,user_id,role) VALUES('ws','user','member'),('ws','user-other','member');
 INSERT INTO weave_external_identities(issuer,subject,workspace_id,user_id,native_organization) VALUES('forge:feishu-test','native-user','ws','user','native-org');
 UPDATE weave_teams SET display_name='资料团队' WHERE workspace_id='ws' AND id='team';
 INSERT INTO weave_feishu_team_access(workspace_id,team_id,enabled,notify,updated_by) VALUES('ws','team',true,'{"result":true,"revisionRequired":true,"humanReview":true,"failure":true,"cancelled":true}','user');`)
	if err != nil {
		t.Fatal(err)
	}
	// Pair via the actual employee route, then deliver a verified provider callback.
	claims := &Claims{TenantID: "ws", UserID: "user", IdentitySource: "forge", RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(s.Config.JWTSecret))
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/integrations/feishu/link-code", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	c := s.Echo.NewContext(req, recorder)
	c.Set("tenant", "ws")
	c.Set("user_id", "user")
	c.Set(identitySourceContextKey, "forge")
	if err := s.handleFeishuLinkCode(c); err != nil || recorder.Code != 200 {
		t.Fatalf("link status=%d err=%v", recorder.Code, err)
	}
	var pairing struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(recorder.Body.Bytes(), &pairing)
	if pairing.Code == "" {
		t.Fatal("link code missing")
	}
	receive := func(id, openID, text string, want int) {
		t.Helper()
		rec := httptest.NewRecorder()
		ctx := s.Echo.NewContext(signedFeishuRequest(f, testFeishuMessage(f, id, openID, text), true), rec)
		if err := s.handleFeishuEvent(ctx); err != nil || rec.Code != want {
			t.Fatalf("event %s status=%d want=%d err=%v", id, rec.Code, want, err)
		}
	}
	drain := func() {
		t.Helper()
		for i := 0; i < 10; i++ {
			n, err := s.sweepFeishuCommand(t.Context(), s.Feishu)
			if err != nil {
				t.Fatal(err)
			}
			if n == 0 {
				return
			}
		}
		t.Fatal("message journal did not settle")
	}
	receive("bind", "open-user", "绑定 "+pairing.Code, 200)
	drain()
	var stored string
	if err := pool.QueryRow(t.Context(), `SELECT command::text FROM weave_feishu_messages WHERE message_id='bind'`).Scan(&stored); err != nil || strings.Contains(stored, pairing.Code) {
		t.Fatal("binding code persisted")
	}
	receive("start", "open-user", "开始 资料团队 汇总两份材料", 200)
	drain()
	receive("start", "open-user", "开始 资料团队 汇总两份材料", 200)
	drain()
	receive("start", "open-user", "开始 资料团队 更换任务", 409)
	var inputs, runs int
	_ = pool.QueryRow(t.Context(), `SELECT count(*) FROM weave_dispatch_input_revisions`).Scan(&inputs)
	_ = pool.QueryRow(t.Context(), `SELECT count(*) FROM weave_task_queue WHERE kind='team_workflow'`).Scan(&runs)
	if inputs != 1 || runs != 1 {
		t.Fatalf("duplicate dispatch: inputs=%d tasks=%d", inputs, runs)
	}
	// Execute the real published deterministic workflow on the existing queue.
	checkpoints := teamrun.NewPGCheckpointStore()
	runStore := teamrun.NewPGStore()
	runtime := &teamrun.WorkflowSerialRuntime{Artifacts: s.WorkflowArtifacts, Loader: &workflow.RuntimeLoader{}, HostFactory: rejectingRuntimeHostFactory{}, CredentialResolvers: func(string) (workflow.RuntimeCredentialResolver, error) { return nil, nil }, Transactions: pool, Runs: runStore, Checkpoints: checkpoints, Tasks: s.Tasks, Snapshots: s.Snapshots, OutputRecorder: &developmentTrialWorkflowOutputRecorder{pool: pool, fallback: s.Deliverables}, Activities: s.teamRunActivities}
	executor := &teamrun.Executor{Tasks: s.Tasks, Consumer: &teamrun.Consumer{Transactions: pool, Snapshots: s.Snapshots, Runs: runStore, Tasks: s.Tasks}, Transactions: pool, Runs: runStore, Checkpoints: checkpoints, Runtime: runtime, ResumeTokenHash: func() ([]byte, error) { return []byte("test-feishu-32-byte-resume-digest"), nil }, HeartbeatInterval: time.Second}
	processed, err := executor.ProcessNext(t.Context(), "feishu-test-worker")
	if err != nil || !processed {
		t.Fatalf("actual queue execution: processed=%v err=%v", processed, err)
	}
	receive("view", "open-user", "查看", 200)
	drain()
	if !strings.Contains((*sent)[len(*sent)-1], "汇总两份材料") {
		t.Fatalf("result not read from original work: %s", (*sent)[len(*sent)-1])
	}
	receive("revision", "open-user", "继续 补充第三份材料", 200)
	drain()
	var revisionKind, task, parent string
	err = pool.QueryRow(t.Context(), `SELECT i.revision_kind,i.execution_task,i.parent_input_revision_id FROM weave_dispatch_input_revisions i JOIN weave_feishu_messages m ON m.input_revision_id=i.input_revision_id WHERE m.message_id='revision'`).Scan(&revisionKind, &task, &parent)
	if err != nil || revisionKind != "revision" || parent == "" || !strings.Contains(task, "汇总两份材料") || !strings.Contains(task, "补充第三份材料") {
		t.Fatalf("revision lost original: kind=%s err=%v", revisionKind, err)
	}
	// Unbound or disabled people cannot trigger tasks, even with another person's names.
	receive("other", "open-other", "开始 资料团队 越权", 200)
	drain()
	var after int
	_ = pool.QueryRow(t.Context(), `SELECT count(*) FROM weave_dispatch_input_revisions`).Scan(&after)
	if after != 2 {
		t.Fatal("unbound user dispatched")
	}
	_, _ = pool.Exec(t.Context(), `UPDATE weave_users SET disabled=true WHERE id='user'`)
	receive("disabled", "open-user", "开始 资料团队 越权", 200)
	drain()
	_ = pool.QueryRow(t.Context(), `SELECT count(*) FROM weave_dispatch_input_revisions`).Scan(&after)
	if after != 2 {
		t.Fatal("disabled user dispatched")
	}
	if tokens.Load() != 1 {
		t.Fatal("token cache was not reused across replies")
	}
}

func TestFeishuHumanContinuationAndDurableNotificationsRealPG(t *testing.T) {
	graph, err := os.ReadFile("testdata/human_final_review/graph_definition.json")
	if err != nil {
		t.Fatal(err)
	}
	s, pool := newTeamDispatchTestServerWithGraph(t, graph)
	s.Echo = echo.New()
	s.OrgStore = kernelbindings.NewOrganization(pool)
	f, _, sent := testFeishuClient(t)
	s.Feishu = f
	_, err = pool.Exec(t.Context(), `INSERT INTO weave_members(workspace_id,user_id,role) VALUES('ws','user','member');
 INSERT INTO weave_external_identities(issuer,subject,workspace_id,user_id,native_organization) VALUES('forge:feishu-human','native-user','ws','user','native-org');
 UPDATE weave_teams SET display_name='复核团队' WHERE workspace_id='ws' AND id='team';
 INSERT INTO weave_feishu_team_access(workspace_id,team_id,enabled,notify,updated_by) VALUES('ws','team',true,'{"result":true,"revisionRequired":true,"humanReview":true,"failure":true,"cancelled":true}','user');
 INSERT INTO weave_feishu_links(app_id,workspace_id,user_id,open_id,chat_id,expires_at) VALUES('app','ws','user','human-user','chat',now()+interval '1 hour')`)
	if err != nil {
		t.Fatal(err)
	}
	activities := &teamrun.PGActivityStore{Transactions: pool}
	s.teamRunActivities = activities
	checkpoints := teamrun.NewPGCheckpointStore()
	runs := teamrun.NewPGStore()
	s.teamRunHumanTasks = &teamrun.HumanTaskReader{Pool: pool}
	s.teamRunHumanResume = &teamrun.HumanResumeService{Transactions: pool, Runs: runs, Checkpoints: checkpoints, Tasks: s.Tasks}
	runtime := &teamrun.WorkflowSerialRuntime{Artifacts: s.WorkflowArtifacts, Loader: &workflow.RuntimeLoader{}, HostFactory: rejectingRuntimeHostFactory{}, CredentialResolvers: func(string) (workflow.RuntimeCredentialResolver, error) { return nil, nil }, Transactions: pool, Runs: runs, Checkpoints: checkpoints, Tasks: s.Tasks, Snapshots: s.Snapshots, OutputRecorder: &developmentTrialWorkflowOutputRecorder{pool: pool, fallback: s.Deliverables}, Activities: activities}
	executor := &teamrun.Executor{Tasks: s.Tasks, Consumer: &teamrun.Consumer{Transactions: pool, Snapshots: s.Snapshots, Runs: runs, Tasks: s.Tasks}, Transactions: pool, Runs: runs, Checkpoints: checkpoints, Runtime: runtime, ResumeTokenHash: func() ([]byte, error) { return []byte("test-feishu-human-resume-digest"), nil }, HeartbeatInterval: time.Second}
	receive := func(id, text string) {
		t.Helper()
		response := httptest.NewRecorder()
		if err := s.handleFeishuEvent(s.Echo.NewContext(signedFeishuRequest(f, testFeishuMessage(f, id, "human-user", text), true), response)); err != nil || response.Code != 200 {
			t.Fatalf("event rejected: %v status=%d", err, response.Code)
		}
		for i := 0; i < 4; i++ {
			n, err := s.sweepFeishuCommand(t.Context(), s.Feishu)
			if err != nil {
				t.Fatal(err)
			}
			if n == 0 {
				return
			}
		}
	}
	execute := func() {
		t.Helper()
		processed, err := executor.ProcessNext(t.Context(), "human-worker")
		if err != nil || !processed {
			t.Fatalf("execute processed=%v err=%v", processed, err)
		}
	}
	receive("human-start", "开始 复核团队 请确认这份成果")
	execute()
	worker := &employeeRunEventWorker{Pool: pool, FeishuServer: s}
	if _, err := worker.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains((*sent)[len(*sent)-1], "终审交付稿") {
		t.Fatal("human question not delivered")
	}
	var forgeState, feishuState string
	if err := pool.QueryRow(t.Context(), `SELECT delivery_state,feishu_state FROM weave_employee_run_event_outbox WHERE event_scope<>'terminal'`).Scan(&forgeState, &feishuState); err != nil || forgeState != "pending" || feishuState != "delivered" {
		t.Fatalf("transport states coupled: forge=%s feishu=%s err=%v", forgeState, feishuState, err)
	}
	receive("invalid-answer", `确认 {"decision":"unsafe","comments":"wrong"}`)
	var status string
	_ = pool.QueryRow(t.Context(), `SELECT status FROM weave_team_runs`).Scan(&status)
	if status != "parked" {
		t.Fatal("invalid schema resumed run")
	}
	receive("answer", `确认 {"decision":"approve","comments":"已核对"}`)
	execute()
	receive("answer", `确认 {"decision":"approve","comments":"已核对"}`)
	if _, err := worker.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	receive("human-view", "查看")
	if !strings.Contains((*sent)[len(*sent)-1], "已核对") {
		t.Fatal("human continuation lost the actual result")
	}
	var count int
	_ = pool.QueryRow(t.Context(), `SELECT count(*) FROM weave_employee_run_event_outbox`).Scan(&count)
	if count != 2 {
		t.Fatalf("unexpected duplicate lifecycle events: %d", count)
	}
}

func TestFeishuCachedReplyRechecksOriginalEmployeeRealPG(t *testing.T) {
	s, pool := newTeamDispatchTestServer(t)
	for _, scenario := range []struct {
		name, mutation, command, cached string
		allowed                         bool
	}{
		{"current binding", "", `{"text":"查看"}`, "private old result", true},
		{"unlinked", `DELETE FROM weave_feishu_links`, `{"text":"查看"}`, "private old result", false},
		{"disabled", `UPDATE weave_users SET disabled=true WHERE id='user'`, `{"text":"查看"}`, "private old result", false},
		{"expired", `UPDATE weave_feishu_links SET expires_at=now()-interval '1 second'`, `{"text":"查看"}`, "private old result", false},
		{"rebound", `UPDATE weave_feishu_links SET user_id='user-other'`, `{"text":"查看"}`, "private old result", false},
		{"membership removed", `DELETE FROM weave_members WHERE user_id='user'`, `{"text":"查看"}`, "private old result", false},
		{"binding acknowledgement", `DELETE FROM weave_feishu_links`, `{"text":"绑定","binding_code_hash":"used-code"}`, "已绑定。", true},
		{"unlink acknowledgement", `DELETE FROM weave_feishu_links`, `{"text":"解绑"}`, "已解绑。已接单的工作继续运行。", true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f, _, sent := testFeishuClient(t)
			s.Feishu = f
			_, err := pool.Exec(t.Context(), `DELETE FROM weave_feishu_messages; DELETE FROM weave_feishu_links;
 UPDATE weave_users SET disabled=false WHERE id='user';
 INSERT INTO weave_members(workspace_id,user_id,role) VALUES('ws','user','member'),('ws','user-other','member') ON CONFLICT DO NOTHING;
 INSERT INTO weave_feishu_links(app_id,workspace_id,user_id,open_id,expires_at) VALUES('app','ws','user','open-user',now()+interval '1 hour')`)
			if err != nil {
				t.Fatal(err)
			}
			// The prior sweep already read the result and persisted its reply.
			// Change authorization before the next sweep sends that cached body.
			_, err = pool.Exec(t.Context(), `INSERT INTO weave_feishu_messages(app_id,message_id,open_id,workspace_id,user_id,content_hash,command,response)
 VALUES('app','cached','open-user','ws','user','hash',$1::jsonb,$2)`, scenario.command, scenario.cached)
			if err != nil {
				t.Fatal(err)
			}
			if scenario.mutation != "" {
				if _, err = pool.Exec(t.Context(), scenario.mutation); err != nil {
					t.Fatal(err)
				}
			}
			if n, err := s.sweepFeishuCommand(t.Context(), s.Feishu); err != nil || n != 1 {
				t.Fatalf("delivery n=%d err=%v", n, err)
			}
			if len(*sent) != 1 {
				t.Fatalf("messages=%d", len(*sent))
			}
			var payload struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal([]byte((*sent)[0]), &payload); err != nil {
				t.Fatal(err)
			}
			if scenario.allowed {
				if payload.Text != scenario.cached {
					t.Fatalf("authorized reply changed: %q", payload.Text)
				}
			} else if strings.Contains(payload.Text, scenario.cached) || !strings.Contains(payload.Text, "绑定已失效") {
				t.Fatalf("revoked employee received cached result: %q", payload.Text)
			}
			var stored, receipt string
			if err := pool.QueryRow(t.Context(), `SELECT response,reply_message_id FROM weave_feishu_messages WHERE message_id='cached'`).Scan(&stored, &receipt); err != nil || stored != payload.Text || receipt == "" {
				t.Fatalf("reply replacement/receipt not durable: err=%v", err)
			}
			if n, err := s.sweepFeishuCommand(t.Context(), s.Feishu); err != nil || n != 0 || len(*sent) != 1 {
				t.Fatalf("reply repeated: n=%d err=%v", n, err)
			}
		})
	}
}
