package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/app/agentcatalog"
	"github.com/jinyitao123/weave/internal/app/chatrequest"
	orgstore "github.com/jinyitao123/weave/internal/app/org"
	"github.com/jinyitao123/weave/internal/app/teamconstruction"
	"github.com/jinyitao123/weave/internal/app/workflowcatalog"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/publication"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

type ambiguousPublishedKernel struct {
	publication.Service
	published  publication.PublishedService
	reconciler publication.PublishedReconciler
	lose       atomic.Bool
	before     bool
}

func (k *ambiguousPublishedKernel) AdmitPublished(ctx context.Context, request publication.PublishedRunRequest) (publication.AdmissionReceipt, error) {
	if k.before && k.lose.Swap(false) {
		return publication.AdmissionReceipt{}, errors.New("connection lost before kernel accepted")
	}
	receipt, err := k.published.AdmitPublished(ctx, request)
	if err == nil && k.lose.Swap(false) {
		return publication.AdmissionReceipt{}, errors.New("connection lost after kernel committed")
	}
	return receipt, err
}
func (k *ambiguousPublishedKernel) ClosePublished(ctx context.Context, r publication.PublishedRunRequest) (*publication.AdmissionReceipt, error) {
	return k.reconciler.ClosePublished(ctx, r)
}

func TestPublishedProductSingleConnectionUnknownCommitAndRevocationRealPG(t *testing.T) {
	server, pool := newTeamDispatchTestServer(t)
	cfg := pool.Config()
	cfg.MaxConns = 1
	single, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(single.Close)
	server.Store = teamDispatchPoolStore{pool: single}
	server.ScheduleTransactions = single
	server.Workflow = workflowcatalog.New(single, nil, workflow.NewArtifactStore(single, nil))
	server.OrgStore = orgstore.NewStore(single)
	server.Registry = agentcatalog.New(single)
	authority := teamconstruction.NewPublicationAuthority(single, nil)
	real := openAPIKernelPublication(t, context.Background(), pool, authority)
	kernel := &ambiguousPublishedKernel{Service: real, published: real, reconciler: real}
	kernel.lose.Store(true)
	server.KernelPublication = kernel
	registration := dispatchInputRegistrationFixture("unknown-commit", "保留原始交付范围", "")
	registered, err := registerInputForTest(server, registration)
	if err != nil || registered.Code != http.StatusCreated {
		t.Fatalf("register: %s %v", registered.Body.String(), err)
	}
	var input dispatchInputReceipt
	if err = json.Unmarshal(registered.Body.Bytes(), &input); err != nil {
		t.Fatal(err)
	}
	call := func() int {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"input_revision_id": input.InputRevisionID})
		c, out := dispatchInputTestContext(body, "/v1/teams/team/dispatch", "ws", "user")
		ctx, cancel := context.WithTimeout(c.Request().Context(), 3*time.Second)
		defer cancel()
		c.SetRequest(c.Request().WithContext(ctx))
		if err := server.handleDispatchTeam(c); err != nil {
			t.Fatal(err)
		}
		return out.Code
	}
	if status := call(); status != http.StatusInternalServerError {
		t.Fatalf("lost response status %d", status)
	}
	var tasks, associated int
	if err = pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM weave_task_queue),(SELECT count(*) FROM weave_workflow_admission_requests WHERE associated)`).Scan(&tasks, &associated); err != nil || tasks != 1 || associated != 0 {
		t.Fatalf("unknown state tasks=%d associated=%d %v", tasks, associated, err)
	}
	if _, err = pool.Exec(context.Background(), `UPDATE weave_users SET disabled=true WHERE id='user'`); err != nil {
		t.Fatal(err)
	}
	if status := call(); status < 400 {
		t.Fatal("revoked actor reused an unassociated receipt")
	}
	if _, err = pool.Exec(context.Background(), `UPDATE weave_users SET disabled=false WHERE id='user'`); err != nil {
		t.Fatal(err)
	}
	if status := call(); status != http.StatusOK {
		t.Fatalf("same request recovery: %d", status)
	}
	if err = pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM weave_task_queue),(SELECT count(*) FROM weave_workflow_admission_requests WHERE associated)`).Scan(&tasks, &associated); err != nil || tasks != 1 || associated != 1 {
		t.Fatalf("replay duplicated or lost association %d/%d %v", tasks, associated, err)
	}
	foreign := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "ws", UserID: "user-other"})
	runID, _ := deterministicWorkflowDispatchIDs("ws", "user", input.ClientRequestID)
	if _, err = server.workflowAdmissions().Get(foreign, "ws", runID); !errors.Is(err, execution.ErrSubjectMismatch) {
		t.Fatalf("foreign pending intent visible: %v", err)
	}
}

func TestPublishedInputReconcileOrdersUnknownAcceptanceRealPG(t *testing.T) {
	for _, before := range []bool{true, false} {
		t.Run(map[bool]string{true: "not-accepted", false: "accepted"}[before], func(t *testing.T) {
			server, pool := newTeamDispatchTestServer(t)
			real := server.KernelPublication
			kernel := &ambiguousPublishedKernel{Service: real, published: real.(publication.PublishedService), reconciler: real.(publication.PublishedReconciler), before: before}
			kernel.lose.Store(true)
			server.KernelPublication = kernel
			registered, err := registerInputForTest(server, dispatchInputRegistrationFixture("reconcile", "只派发一次", ""))
			if err != nil || registered.Code != http.StatusCreated {
				t.Fatalf("register %s %v", registered.Body.String(), err)
			}
			var input dispatchInputReceipt
			if err = json.Unmarshal(registered.Body.Bytes(), &input); err != nil {
				t.Fatal(err)
			}
			out, err := boundDispatchForTest(server, map[string]any{"input_revision_id": input.InputRevisionID}, "user")
			if err != nil || out.Code != http.StatusInternalServerError {
				t.Fatalf("ambiguous dispatch %s %v", out.Body.String(), err)
			}
			reconciled, err := reconcileInputForTest(server, input.InputRevisionID)
			if err != nil || reconciled.Code != http.StatusOK {
				t.Fatalf("reconcile %s %v", reconciled.Body.String(), err)
			}
			out, err = boundDispatchForTest(server, map[string]any{"input_revision_id": input.InputRevisionID}, "user")
			want := http.StatusOK
			if before {
				want = http.StatusConflict
			}
			if err != nil || out.Code != want {
				t.Fatalf("delayed dispatch %s %v", out.Body.String(), err)
			}
			var n int
			if err = pool.QueryRow(context.Background(), `SELECT count(*) FROM weave_task_queue`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			wantTasks := 1
			if before {
				wantTasks = 0
			}
			if n != wantTasks {
				t.Fatalf("reconciliation created/duplicated execution: %d", n)
			}
		})
	}
}

func TestPublishedChatRecoversUnboundKernelReceiptRealPG(t *testing.T) {
	server, pool := newTeamDispatchTestServer(t)
	ctx := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "ws", UserID: "user"})
	if _, err := pool.Exec(ctx, `INSERT INTO weave_projects(id,workspace_id,avatar_id,name,team_id)VALUES('project','ws','lead','Project','team');
 INSERT INTO weave_conversations(id,workspace_id,agent_id,user_id,project_id)VALUES('conversation','ws','lead','user','project');
 INSERT INTO weave_messages(id,conversation_id,workspace_id,role,content)VALUES('user-message','conversation','ws','user','Do this once')`); err != nil {
		t.Fatal(err)
	}
	server.ChatRequests = chatrequest.New(pool, nil)
	requestID := uuid.NewString()
	if _, _, err := server.ChatRequests.Begin(ctx, chatrequest.BeginRequest{WorkspaceID: "ws", UserID: "user", ClientRequestID: requestID, RequestFingerprint: "chat-fingerprint", ProjectID: "project", AgentID: "lead"}); err != nil {
		t.Fatal(err)
	}
	if _, err := server.ChatRequests.MarkAdmitted(ctx, "ws", "user", requestID, "session", "conversation", "user-message"); err != nil {
		t.Fatal(err)
	}
	real := server.KernelPublication
	kernel := &ambiguousPublishedKernel{Service: real, published: real.(publication.PublishedService), reconciler: real.(publication.PublishedReconciler)}
	kernel.lose.Store(true)
	server.KernelPublication = kernel
	if _, _, err := server.admitPublishedWorkflowChat(ctx, "ws", "flow", "project", "conversation", "user-message", requestID, "Do this once"); err == nil {
		t.Fatal("response loss did not reach caller")
	}
	original, err := server.ChatRequests.Get(ctx, "ws", "user", requestID)
	if err != nil || original.RunID != "" {
		t.Fatalf("unverified receipt associated %+v %v", original, err)
	}
	// Recreate the adapter, as a restart would, without the original HTTP handler.
	server.ChatRequests = chatrequest.New(pool, nil)
	recovered, err := server.recoverPublishedChatAdmission(ctx, original)
	if err != nil || recovered.RunID == "" || recovered.TaskID == "" || recovered.Status != "running" {
		t.Fatalf("recovery %+v %v", recovered, err)
	}
	repeated, err := server.recoverPublishedChatAdmission(ctx, recovered)
	if err != nil || repeated.RunID != recovered.RunID {
		t.Fatalf("repeated recovery %+v %v", repeated, err)
	}
	var n int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM weave_task_queue`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("chat recovery queued %d %v", n, err)
	}
}
