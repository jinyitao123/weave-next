package capabilities

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/app/users"
	"github.com/jinyitao123/weave/internal/app/workflowdispatch"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
	"github.com/jinyitao123/weave/internal/base/teamrun"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

type mutableCapabilityClock struct{ now time.Time }

func (clock *mutableCapabilityClock) Now() time.Time { return clock.now }

type invocationHarness struct {
	pool          *pgxpool.Pool
	clock         *mutableCapabilityClock
	store         *Store
	service       *InvocationService
	adminID       string
	app           ServiceApp
	credential    Credential
	rawCredential string
	principal     Principal
	capability    Capability
	release       Release
}

func newInvocationHarness(t *testing.T, appLimit, grantLimit int) *invocationHarness {
	t.Helper()
	ctx := context.Background()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	workspaceID := "capability-invocation-" + uuid.NewString()
	admin, err := users.NewStore(pool).Create(ctx, workspaceID, "admin", "password", "Admin", "admin")
	if err != nil {
		t.Fatal(err)
	}
	clock := &mutableCapabilityClock{now: time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC)}
	store := New(pool, clock)
	app, err := store.CreateApp(ctx, CreateAppRequest{
		WorkspaceID: workspaceID, Name: "order-service-" + uuid.NewString(),
		MaxConcurrentInvocations: appLimit, CreatedBy: admin.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	credential, raw, err := store.CreateCredential(ctx, CreateCredentialRequest{
		WorkspaceID: workspaceID, AppID: app.ID, Name: "primary",
		Scopes: []string{"invoke", "read", "cancel"}, CreatedBy: admin.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	principal, err := store.ValidateCredential(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	capability, err := store.CreateCapability(ctx, CreateCapabilityRequest{
		WorkspaceID: workspaceID, Key: "order_check_" + uuid.NewString()[:8],
		Name: "订单材料检查", CreatedBy: admin.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	workflowID := "flow-" + uuid.NewString()
	seedNoToolArtifact(t, pool, workspaceID, "team-"+uuid.NewString(), workflowID, 1)
	limits := ExecutionLimits{
		MaxInputBytes: 65536, MaxOutputBytes: 32768, MaxNestingDepth: 20,
		MaxObjectFields: 256, MaxArrayItems: 1000, MaxStringBytes: 8192,
		QueueTimeoutSeconds: 60, ExecutionTimeoutSeconds: 300, MaxOutputTokens: 1200,
	}
	release, err := store.PublishRelease(ctx, PublishReleaseRequest{
		WorkspaceID: workspaceID, CapabilityID: capability.ID, Version: 1,
		WorkflowID: workflowID, WorkflowVersion: 1, ExecutionLimits: limits,
		ResultPolicy: ResultPolicy{ExposedFields: []string{"summary"}, IncludeContractEvidence: true},
		CreatedBy:    admin.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.GrantRelease(ctx, GrantReleaseRequest{
		WorkspaceID: workspaceID, AppID: app.ID, CapabilityID: capability.ID,
		ReleaseVersion: 1, MaxConcurrent: grantLimit, GrantedBy: admin.ID,
	}); err != nil {
		t.Fatal(err)
	}
	tasks := taskqueue.New(pool, clock, time.Minute)
	runs := teamrun.NewPGStore()
	runs.Transactions = pool
	cancel := &teamrun.CancelService{Transactions: pool, Runs: runs, Tasks: tasks, Now: clock.Now}
	service := &InvocationService{
		Store: store,
		Dispatch: &workflowdispatch.Service{
			Workflow: workflow.New(pool, clock), Snapshots: snapshot.NewStore(pool),
			Tasks: tasks, Deliverables: deliverable.New(pool),
		},
		Tasks: tasks, Runs: runs, Cancel: cancel,
	}
	return &invocationHarness{
		pool: pool, clock: clock, store: store, service: service, adminID: admin.ID,
		app: app, credential: credential, rawCredential: raw, principal: principal,
		capability: capability, release: release,
	}
}

func (h *invocationHarness) submit(t *testing.T, requestID string, input string, key *string) InvocationReceipt {
	t.Helper()
	receipt, err := h.service.Submit(context.Background(), SubmitInvocationRequest{
		Principal: h.principal, CapabilityID: h.capability.ID, ReleaseVersion: 1,
		RequestID: requestID, Input: json.RawMessage(input), ConcurrencyKey: key,
	})
	if err != nil {
		t.Fatalf("submit %s: %v", requestID, err)
	}
	return receipt
}

func validOrderInput(orderID string) string {
	return `{"order_id":"` + orderID + `","materials":["合同","清单"]}`
}

func TestInvocationAtomicReplayIsolationAndEarlyCancellationRealPG(t *testing.T) {
	h := newInvocationHarness(t, 2, 2)
	ctx := context.Background()

	if _, err := h.service.Submit(ctx, SubmitInvocationRequest{
		Principal: h.principal, CapabilityID: h.capability.ID, ReleaseVersion: 1,
		RequestID: "invalid-input", Input: json.RawMessage(`{"materials":[]}`),
	}); err == nil {
		t.Fatalf("invalid input error = %v", err)
	} else {
		var schema *SchemaViolationError
		if !errors.As(err, &schema) {
			t.Fatalf("invalid input error type = %v", err)
		}
	}
	assertInvocationCounts(t, h.pool, h.app.WorkspaceID, 0, 0, 0)

	key := "order-42"
	first := h.submit(t, "request-1", validOrderInput("42"), &key)
	if first.Replay || first.Invocation.ReleaseID != h.release.ID ||
		first.Invocation.ArtifactContentHash != h.release.ArtifactContentHash {
		t.Fatalf("new receipt = %+v", first)
	}
	assertInvocationCounts(t, h.pool, h.app.WorkspaceID, 1, 1, 1)
	replayed := h.submit(t, "request-1", `{"materials":["合同","清单"],"order_id":"42"}`, &key)
	if !replayed.Replay || replayed.Invocation.InvocationID != first.Invocation.InvocationID ||
		replayed.Invocation.RunID != first.Invocation.RunID || replayed.Invocation.TaskID != first.Invocation.TaskID {
		t.Fatalf("replay drifted: first=%+v replay=%+v", first, replayed)
	}
	assertInvocationCounts(t, h.pool, h.app.WorkspaceID, 1, 1, 1)
	if _, err := h.service.Submit(ctx, SubmitInvocationRequest{
		Principal: h.principal, CapabilityID: h.capability.ID, ReleaseVersion: 1,
		RequestID: "request-1", Input: json.RawMessage(validOrderInput("changed")), ConcurrencyKey: &key,
	}); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("changed replay error = %v", err)
	}

	rotated, raw, err := h.store.CreateCredential(ctx, CreateCredentialRequest{
		WorkspaceID: h.app.WorkspaceID, AppID: h.app.ID, Name: "rotated",
		Scopes: []string{"read", "cancel", "invoke"}, CreatedBy: h.adminID,
	})
	if err != nil {
		t.Fatal(err)
	}
	rotatedPrincipal, err := h.store.ValidateCredential(ctx, raw)
	if err != nil || rotated.ID == h.credential.ID {
		t.Fatalf("rotated credential = %+v error=%v", rotated, err)
	}
	if status, err := h.service.Get(ctx, rotatedPrincipal, first.Invocation.InvocationID); err != nil ||
		status.ExecutionStatus != "accepted" {
		t.Fatalf("rotated read status=%+v error=%v", status, err)
	}

	otherApp, err := h.store.CreateApp(ctx, CreateAppRequest{
		WorkspaceID: h.app.WorkspaceID, Name: "other-service-" + uuid.NewString(),
		MaxConcurrentInvocations: 1, CreatedBy: h.adminID,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, otherRaw, err := h.store.CreateCredential(ctx, CreateCredentialRequest{
		WorkspaceID: h.app.WorkspaceID, AppID: otherApp.ID, Name: "other",
		Scopes: []string{"read", "cancel"}, CreatedBy: h.adminID,
	})
	if err != nil {
		t.Fatal(err)
	}
	otherPrincipal, _ := h.store.ValidateCredential(ctx, otherRaw)
	if _, err := h.service.Get(ctx, otherPrincipal, first.Invocation.InvocationID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-app read error = %v", err)
	}
	if _, err := h.service.CancelInvocation(ctx, CancelInvocationRequest{
		Principal: otherPrincipal, InvocationID: first.Invocation.InvocationID, Reason: "stop",
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-app cancel error = %v", err)
	}

	early, err := h.service.CancelInvocation(ctx, CancelInvocationRequest{
		Principal: rotatedPrincipal, RequestID: "early-request", Reason: "caller no longer needs it",
	})
	if err != nil || early.Status != "cancelled_before_submit" {
		t.Fatalf("early cancellation = %+v error=%v", early, err)
	}
	if _, err := h.service.Submit(ctx, SubmitInvocationRequest{
		Principal: rotatedPrincipal, CapabilityID: h.capability.ID, ReleaseVersion: 1,
		RequestID: "early-request", Input: json.RawMessage(validOrderInput("early")),
	}); !errors.Is(err, ErrCancelledBeforeSubmit) {
		t.Fatalf("late submit after early cancellation error = %v", err)
	}
}

func TestInvocationCapacityClaimFenceResultPolicyAndDeadlineRealPG(t *testing.T) {
	h := newInvocationHarness(t, 2, 2)
	ctx := context.Background()
	serial := "customer-7"
	first := h.submit(t, "serial-1", validOrderInput("serial-1"), &serial)
	if _, err := h.service.Submit(ctx, SubmitInvocationRequest{
		Principal: h.principal, CapabilityID: h.capability.ID, ReleaseVersion: 1,
		RequestID: "serial-2", Input: json.RawMessage(validOrderInput("serial-2")),
		ConcurrencyKey: &serial,
	}); !errors.Is(err, ErrConcurrencyBusy) {
		t.Fatalf("same concurrency key error = %v", err)
	}
	otherKey := "customer-8"
	second := h.submit(t, "capacity-2", validOrderInput("capacity-2"), &otherKey)
	if _, err := h.service.Submit(ctx, SubmitInvocationRequest{
		Principal: h.principal, CapabilityID: h.capability.ID, ReleaseVersion: 1,
		RequestID: "capacity-3", Input: json.RawMessage(validOrderInput("capacity-3")),
	}); !errors.Is(err, ErrCapacityExceeded) {
		t.Fatalf("app capacity error = %v", err)
	}

	if _, err := h.service.CancelInvocation(ctx, CancelInvocationRequest{
		Principal: h.principal, RequestID: first.Invocation.RequestID, Reason: "release capacity",
	}); err != nil {
		t.Fatal(err)
	}
	claimed, err := h.service.Tasks.Claim(ctx, "race-worker", taskqueue.ClaimFilter{
		Kind: "team_workflow", WorkspaceID: h.app.WorkspaceID,
		RunSnapshotID: second.Invocation.RunID, IdentityKind: taskqueue.IdentityTeamWorkflow,
	})
	if err != nil || claimed == nil {
		t.Fatalf("claim invocation task: task=%+v error=%v", claimed, err)
	}
	if _, err := h.service.CancelInvocation(ctx, CancelInvocationRequest{
		Principal: h.principal, InvocationID: second.Invocation.InvocationID,
		Reason: "race cancellation",
	}); err != nil {
		t.Fatal(err)
	}
	consumer := &teamrun.Consumer{
		Transactions: h.pool, Snapshots: snapshot.NewStore(h.pool),
		Runs: h.service.Runs, Tasks: h.service.Tasks, Now: h.clock.Now,
	}
	if _, err := consumer.ConsumeClaimed(ctx, claimed, "race-worker"); !errors.Is(err, teamrun.ErrClaimFenced) {
		t.Fatalf("cancelled claimed task fence error = %v", err)
	}
	if err := h.service.Tasks.AcknowledgeExecutionStopped(ctx, claimed.ID, "race-worker"); err != nil {
		t.Fatal(err)
	}
	var runCount int
	if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM weave_team_runs WHERE workspace_id=$1 AND run_id=$2`,
		h.app.WorkspaceID, second.Invocation.RunID).Scan(&runCount); err != nil || runCount != 0 {
		t.Fatalf("cancelled claim established TeamRun count=%d error=%v", runCount, err)
	}
	if status, err := h.service.Get(ctx, h.principal, second.Invocation.InvocationID); err != nil ||
		status.ExecutionStatus != "cancelled" || status.CancellationStatus != "confirmed" {
		t.Fatalf("cancelled claimed projection=%+v error=%v", status, err)
	}

	success := h.submit(t, "success", validOrderInput("success"), nil)
	completeInvocation(t, h, success.Invocation, json.RawMessage(`{"output":{"summary":"materials complete","issues":[]}}`))
	status, err := h.service.Get(ctx, h.principal, success.Invocation.InvocationID)
	if err != nil || status.ExecutionStatus != "succeeded" ||
		status.ResultAvailability != "available" ||
		string(status.Result) != `{"summary":"materials complete"}` ||
		status.ContractEvidence == nil || !status.ContractEvidence.OutputValidated {
		t.Fatalf("successful result projection=%+v error=%v", status, err)
	}

	invalid := h.submit(t, "invalid-output", validOrderInput("invalid-output"), nil)
	completeInvocation(t, h, invalid.Invocation, json.RawMessage(`{"output":{"summary":"missing issues"}}`))
	status, err = h.service.Get(ctx, h.principal, invalid.Invocation.InvocationID)
	if err != nil || status.ExecutionStatus != "succeeded" ||
		status.ResultAvailability != "contract_invalid" || len(status.Result) != 0 {
		t.Fatalf("invalid result projection=%+v error=%v", status, err)
	}

	deadline := h.submit(t, "queue-deadline", validOrderInput("queue-deadline"), nil)
	h.clock.now = deadline.Invocation.AcceptedAt.Add(59 * time.Second)
	if swept, err := h.service.SweepControls(ctx, 10); err != nil || swept != 0 {
		t.Fatalf("early queue deadline sweep count=%d error=%v", swept, err)
	}
	h.clock.now = deadline.Invocation.AcceptedAt.Add(61 * time.Second)
	restarted := *h.service
	if swept, err := restarted.SweepControls(ctx, 10); err != nil || swept != 1 {
		t.Fatalf("queue deadline sweep count=%d error=%v", swept, err)
	}
	status, err = restarted.Get(ctx, h.principal, deadline.Invocation.InvocationID)
	if err != nil || status.ExecutionStatus != "cancelled" || status.CancellationStatus != "confirmed" {
		t.Fatalf("queue deadline projection=%+v error=%v", status, err)
	}

	runningDeadline := h.submit(t, "execution-deadline", validOrderInput("execution-deadline"), nil)
	h.clock.now = runningDeadline.Invocation.AcceptedAt.Add(10 * time.Second)
	startInvocation(t, h, runningDeadline.Invocation)
	h.clock.now = runningDeadline.Invocation.AcceptedAt.Add(309 * time.Second)
	if swept, err := restarted.SweepControls(ctx, 10); err != nil || swept != 0 {
		t.Fatalf("early execution deadline sweep count=%d error=%v", swept, err)
	}
	h.clock.now = runningDeadline.Invocation.AcceptedAt.Add(311 * time.Second)
	if swept, err := restarted.SweepControls(ctx, 10); err != nil || swept != 1 {
		t.Fatalf("execution deadline sweep count=%d error=%v", swept, err)
	}
	status, err = restarted.Get(ctx, h.principal, runningDeadline.Invocation.InvocationID)
	if err != nil || status.ExecutionStatus != "cancel_requested" || status.CancellationStatus != "requested" {
		t.Fatalf("execution deadline projection=%+v error=%v", status, err)
	}
}

func TestCapabilityManagementLifecycleAndAdminCancellationRealPG(t *testing.T) {
	h := newInvocationHarness(t, 2, 2)
	ctx := context.Background()
	receipt := h.submit(t, "managed-request", validOrderInput("managed"), nil)

	snapshot, err := h.store.ManagementSnapshot(ctx, h.app.WorkspaceID, h.service)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Apps) != 1 || len(snapshot.Credentials) != 1 ||
		len(snapshot.Capabilities) != 1 || len(snapshot.Releases) != 1 ||
		len(snapshot.Grants) != 1 || len(snapshot.Invocations) != 1 ||
		len(snapshot.Workflows) != 1 ||
		snapshot.Invocations[0].Invocation.InvocationID != receipt.Invocation.InvocationID {
		t.Fatalf("management snapshot = %+v", snapshot)
	}

	cancelled, err := h.service.AdminCancelInvocation(
		ctx, h.app.WorkspaceID, h.adminID, receipt.Invocation.InvocationID,
		"administrator stopped the invocation",
	)
	if err != nil || cancelled.Status != "requested" {
		t.Fatalf("administrator cancellation = %+v error=%v", cancelled, err)
	}
	var requestedBy *string
	var requestedCredential *string
	if err := h.pool.QueryRow(ctx, `
		SELECT requested_by_user_id,requested_credential_id
		FROM weave_capability_invocation_cancellations
		WHERE workspace_id=$1 AND app_id=$2 AND request_id=$3
	`, h.app.WorkspaceID, h.app.ID, receipt.Invocation.RequestID).Scan(
		&requestedBy, &requestedCredential,
	); err != nil || requestedBy == nil || *requestedBy != h.adminID || requestedCredential != nil {
		t.Fatalf("administrator cancellation actor user=%v credential=%v error=%v",
			requestedBy, requestedCredential, err)
	}

	if err := h.store.SetReleaseEnabled(
		ctx, h.app.WorkspaceID, h.capability.ID, h.release.Version, h.adminID, false,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Submit(ctx, SubmitInvocationRequest{
		Principal: h.principal, CapabilityID: h.capability.ID, ReleaseVersion: 1,
		RequestID: "disabled-release", Input: json.RawMessage(validOrderInput("disabled")),
	}); !errors.Is(err, ErrGrantUnavailable) {
		t.Fatalf("disabled release submit error = %v", err)
	}
	status, err := h.service.Get(ctx, h.principal, receipt.Invocation.InvocationID)
	if err != nil || status.Invocation.InvocationID != receipt.Invocation.InvocationID {
		t.Fatalf("historical read after release disable = %+v error=%v", status, err)
	}
	if err := h.store.SetReleaseEnabled(
		ctx, h.app.WorkspaceID, h.capability.ID, h.release.Version, h.adminID, true,
	); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetCapabilityEnabled(
		ctx, h.app.WorkspaceID, h.capability.ID, h.adminID, false,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Submit(ctx, SubmitInvocationRequest{
		Principal: h.principal, CapabilityID: h.capability.ID, ReleaseVersion: 1,
		RequestID: "disabled-capability", Input: json.RawMessage(validOrderInput("disabled")),
	}); !errors.Is(err, ErrGrantUnavailable) {
		t.Fatalf("disabled capability submit error = %v", err)
	}
	if err := h.store.SetAppEnabled(ctx, h.app.WorkspaceID, h.app.ID, h.adminID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.ValidateCredential(ctx, h.rawCredential); !errors.Is(err, ErrCredentialInvalid) {
		t.Fatalf("invalid credential after app disable = %v", err)
	}
}

func startInvocation(t *testing.T, h *invocationHarness, invocation Invocation) teamrun.TeamRun {
	t.Helper()
	ctx := context.Background()
	workerID := "result-worker"
	claimed, err := h.service.Tasks.Claim(ctx, workerID, taskqueue.ClaimFilter{
		Kind: "team_workflow", WorkspaceID: invocation.WorkspaceID,
		RunSnapshotID: invocation.RunID, IdentityKind: taskqueue.IdentityTeamWorkflow,
	})
	if err != nil || claimed == nil {
		t.Fatalf("claim result task: task=%+v error=%v", claimed, err)
	}
	consumer := &teamrun.Consumer{
		Transactions: h.pool, Snapshots: snapshot.NewStore(h.pool), Runs: h.service.Runs,
		Tasks: h.service.Tasks, Now: h.clock.Now,
	}
	queued, err := consumer.ConsumeClaimed(ctx, claimed, workerID)
	if err != nil {
		t.Fatal(err)
	}
	executorID := "executor:" + workerID
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	running, err := h.service.Runs.ClaimRunningTx(ctx, tx, teamrun.ClaimRequest{
		WorkspaceID: queued.WorkspaceID, RunID: queued.RunID,
		ExpectedStatus: queued.Status, ExpectedTeamRunGeneration: queued.Generation,
		ExpectedExecutionLeaseEpoch: queued.ExecutionLeaseEpoch,
		ExpectedResumeGeneration:    queued.ResumeGeneration, ExecutorID: executorID,
		IdempotencyKey: "claim:" + invocation.TaskID, Actor: executorID,
		Source: "capability-test", OccurredAt: h.clock.Now(),
	})
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return running
}

func completeInvocation(t *testing.T, h *invocationHarness, invocation Invocation, result json.RawMessage) {
	t.Helper()
	ctx := context.Background()
	workerID := "result-worker"
	running := startInvocation(t, h, invocation)
	executorID := "executor:" + workerID
	h.clock.now = h.clock.now.Add(time.Second)
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.service.Runs.SucceedTx(ctx, tx, teamrun.SucceedRequest{
		WorkspaceID: running.WorkspaceID, RunID: running.RunID,
		ExpectedStatus: running.Status, ExpectedTeamRunGeneration: running.Generation,
		ExpectedExecutionLeaseEpoch: running.ExecutionLeaseEpoch,
		ExpectedResumeGeneration:    running.ResumeGeneration, ExecutorID: executorID,
		IdempotencyKey: "succeed:" + invocation.TaskID, Actor: executorID,
		Source: "capability-test", OccurredAt: h.clock.Now(),
	})
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.service.Tasks.CompleteClaimed(ctx, invocation.TaskID, workerID, result, invocation.RunID); err != nil {
		t.Fatal(err)
	}
}

func assertInvocationCounts(t *testing.T, pool *pgxpool.Pool, workspaceID string, invocations, snapshots, tasks int) {
	t.Helper()
	ctx := context.Background()
	for name, query := range map[string]string{
		"invocations": `SELECT count(*) FROM weave_capability_invocations WHERE workspace_id=$1`,
		"snapshots":   `SELECT count(*) FROM weave_team_run_snapshots WHERE workspace_id=$1`,
		"tasks":       `SELECT count(*) FROM weave_task_queue WHERE workspace_id=$1`,
	} {
		var count int
		if err := pool.QueryRow(ctx, query, workspaceID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		want := map[string]int{"invocations": invocations, "snapshots": snapshots, "tasks": tasks}[name]
		if count != want {
			t.Fatalf("%s count=%d want=%d", name, count, want)
		}
	}
}
