package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/app/capabilities"
	"github.com/jinyitao123/weave/internal/app/users"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/capability"
	"github.com/jinyitao123/weave/internal/kernel/config"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/publication"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/labstack/echo/v4"
)

type capabilityWorkflowContractFixture struct {
	server *Server
	pool   *pgxpool.Pool
	access *capabilities.AccessStore
}

type capabilityWorkflowHTTPResult struct {
	status int
	body   map[string]json.RawMessage
}

func newCapabilityWorkflowContractFixture(t *testing.T) *capabilityWorkflowContractFixture {
	t.Helper()
	graph := json.RawMessage(`{"schema_version":1,"entry_node_id":"deliver","input_contract":{"type":"json","schema":{"type":"object","additionalProperties":false,"properties":{"document":{"type":"string"}},"required":["document"]}},"output_contract":{"type":"json","schema":{"type":"object","additionalProperties":false,"properties":{"document":{"type":"string"}},"required":["document"]}},"nodes":[{"id":"deliver","type":"deliver","config":{"result":{"source":"run_input","path":""}}}],"edges":[]}`)
	server, pool := newTeamDispatchTestServerWithGraph(t, graph)
	access := capabilities.NewAccessStore(pool)
	store := capabilities.NewPGStore(pool)
	server.Echo = echo.New()
	server.Config = &config.Config{JWTSecret: "workflow-contract-test"}
	server.Pool = pool
	server.UserStore = users.NewStore(pool)
	server.Capabilities = capabilities.NewService(store)
	server.CapabilityAccess = access
	server.registerRoutes()
	return &capabilityWorkflowContractFixture{server: server, pool: pool, access: access}
}

func (f *capabilityWorkflowContractFixture) publishCapability(t *testing.T, capabilityID string) {
	t.Helper()
	definition := capability.Definition{
		SchemaVersion: 1,
		CapabilityID:  capabilityID,
		Name:          "Review materials",
		InputSchema:   json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"document":{"type":"string"}},"required":["document"]}`),
		OutputSchema:  json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"document":{"type":"string"}},"required":["document"]}`),
		Workflow:      &capability.WorkflowReference{WorkflowID: "flow", WorkflowVersion: 1},
	}
	if err := f.server.Capabilities.SaveDraft(t.Context(), capabilities.DraftRequest{WorkspaceID: "ws", Definition: definition}); err != nil {
		t.Fatalf("save workflow-backed capability %q: %v", capabilityID, err)
	}
	if _, err := f.server.Capabilities.Publish(t.Context(), "ws", capabilityID, 1); err != nil {
		t.Fatalf("publish workflow-backed capability %q: %v", capabilityID, err)
	}
}

func (f *capabilityWorkflowContractFixture) publishInlineCapability(t *testing.T, capabilityID string) {
	t.Helper()
	definition := capability.Definition{
		SchemaVersion: 1,
		CapabilityID:  capabilityID,
		Name:          "Inline review",
		InputSchema:   json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"document":{"type":"string"}},"required":["document"]}`),
		OutputSchema:  json.RawMessage(`{"type":"object"}`),
		Roles:         []capability.Role{{ID: "reviewer", Name: "Reviewer"}},
		Steps:         []capability.Step{{ID: "review", Name: "Review", RoleID: "reviewer", Kind: capability.StepWorker, Instruction: "Review the supplied document."}},
	}
	if err := f.server.Capabilities.SaveDraft(t.Context(), capabilities.DraftRequest{WorkspaceID: "ws", Definition: definition}); err != nil {
		t.Fatalf("save inline capability %q: %v", capabilityID, err)
	}
	if _, err := f.server.Capabilities.Publish(t.Context(), "ws", capabilityID, 1); err != nil {
		t.Fatalf("publish inline capability %q: %v", capabilityID, err)
	}
}

func (f *capabilityWorkflowContractFixture) application(t *testing.T, name string) (capabilities.Application, string) {
	t.Helper()
	app, err := f.access.CreateApplication(t.Context(), "ws", "user", name)
	if err != nil {
		t.Fatalf("create capability application: %v", err)
	}
	_, key, err := f.access.IssueCredential(t.Context(), "ws", "user", app.ID, name+" key", []string{"invoke", "read", "cancel"})
	if err != nil {
		t.Fatalf("issue capability application credential: %v", err)
	}
	return app, key
}

func (f *capabilityWorkflowContractFixture) adminToken(t *testing.T) string {
	t.Helper()
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		TenantID: "ws", UserID: "user", Roles: []string{"admin"},
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))},
	}).SignedString([]byte("workflow-contract-test"))
	if err != nil {
		t.Fatalf("sign capability manager token: %v", err)
	}
	return token
}

func (f *capabilityWorkflowContractFixture) grant(t *testing.T, appID, capabilityID string, enabled bool) {
	t.Helper()
	if err := f.access.SetGrant(t.Context(), "ws", "user", capabilities.VersionGrant{
		AppID: appID, CapabilityID: capabilityID, Revision: 1, Enabled: enabled,
	}); err != nil {
		t.Fatalf("set capability version grant: %v", err)
	}
}

func (f *capabilityWorkflowContractFixture) call(t *testing.T, key, method, path string, body any) capabilityWorkflowHTTPResult {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal capability workflow request: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	f.server.Echo.ServeHTTP(response, request)
	result := capabilityWorkflowHTTPResult{status: response.Code, body: map[string]json.RawMessage{}}
	if response.Body.Len() != 0 {
		if err := json.Unmarshal(response.Body.Bytes(), &result.body); err != nil {
			t.Fatalf("decode %s %s response %d: %s (%v)", method, path, response.Code, response.Body.String(), err)
		}
	}
	return result
}

func (r capabilityWorkflowHTTPResult) stringField(t *testing.T, name string) string {
	t.Helper()
	var value string
	if err := json.Unmarshal(r.body[name], &value); err != nil {
		t.Fatalf("response field %q is not a string: %s (%v)", name, r.body[name], err)
	}
	return value
}

func (r capabilityWorkflowHTTPResult) intField(t *testing.T, name string) int {
	t.Helper()
	var value int
	if err := json.Unmarshal(r.body[name], &value); err != nil {
		t.Fatalf("response field %q is not an integer: %s (%v)", name, r.body[name], err)
	}
	return value
}

func (r capabilityWorkflowHTTPResult) boolField(t *testing.T, name string) bool {
	t.Helper()
	var value bool
	if err := json.Unmarshal(r.body[name], &value); err != nil {
		t.Fatalf("response field %q is not a boolean: %s (%v)", name, r.body[name], err)
	}
	return value
}

type capabilityWorkflowEmptyCredentialResolver struct{}

func (capabilityWorkflowEmptyCredentialResolver) Validate(context.Context, frozen.CredentialReference) error {
	return nil
}

func (capabilityWorkflowEmptyCredentialResolver) Resolve(context.Context, credentials.ResolveRequest) (credentials.SecretMaterial, error) {
	return credentials.SecretMaterial{}, nil
}

func processNextCapabilityWorkflowRun(t *testing.T, f *capabilityWorkflowContractFixture) *teamrun.PGStore {
	t.Helper()
	runs := teamrun.NewPGStore()
	runs.Transactions = f.pool
	checkpoints := teamrun.NewPGCheckpointStore()
	runtime := &teamrun.WorkflowSerialRuntime{
		Artifacts:   f.server.WorkflowArtifacts,
		Loader:      &workflow.RuntimeLoader{},
		HostFactory: workflow.NewRuntimeHostFactory(),
		CredentialResolvers: func(string) (workflow.RuntimeCredentialResolver, error) {
			return capabilityWorkflowEmptyCredentialResolver{}, nil
		},
		Transactions: f.pool,
		Runs:         runs,
		Checkpoints:  checkpoints,
		Tasks:        f.server.Tasks,
		Snapshots:    f.server.Snapshots,
	}
	consumer := &teamrun.Consumer{Transactions: f.pool, Snapshots: f.server.Snapshots, Runs: runs, Tasks: f.server.Tasks}
	executor := &teamrun.Executor{
		Tasks: f.server.Tasks, Transactions: f.pool, Runs: runs, Checkpoints: checkpoints,
		Runtime: runtime, Consumer: consumer,
	}
	for attempt := 0; attempt < 2; attempt++ {
		worked, err := executor.ProcessNext(t.Context(), "capability-workflow-test-worker")
		if err != nil || !worked {
			t.Fatalf("process published capability workflow task %d: worked=%v err=%v", attempt+1, worked, err)
		}
	}
	return runs
}

func workflowInvocationPath(capabilityID string, revision int) string {
	return fmt.Sprintf("/v1/capabilities/%s/versions/%d/invocations", capabilityID, revision)
}

func TestCapabilityWorkflowInvocationContractRealPG(t *testing.T) {
	f := newCapabilityWorkflowContractFixture(t)
	f.publishCapability(t, "review")
	f.publishCapability(t, "summarize")
	f.publishCapability(t, "ungranted")
	appA, keyA := f.application(t, "review app A")
	appB, keyB := f.application(t, "review app B")
	f.grant(t, appA.ID, "review", true)
	f.grant(t, appA.ID, "summarize", true)
	f.grant(t, appB.ID, "review", true)

	invocationID := "11111111-1111-4111-8111-111111111111"
	request := map[string]any{
		"request_id":    "request-1",
		"input":         map[string]any{"document": "Quarterly materials"},
		"invocation_id": invocationID,
		"lane":          "quarterly_review",
	}
	first := f.call(t, keyA, http.MethodPost, workflowInvocationPath("review", 1), request)
	if first.status != http.StatusAccepted {
		t.Fatalf("application invocation status=%d body=%v", first.status, first.body)
	}
	if got := first.stringField(t, "invocation_id"); got != invocationID {
		t.Fatalf("invocation id=%q want client id %q", got, invocationID)
	}
	if first.stringField(t, "workflow_id") != "flow" || first.intField(t, "workflow_version") != 1 {
		t.Fatalf("invocation did not report its pinned workflow: %+v", first.body)
	}
	runA := first.stringField(t, "run_id")
	inputHash := first.stringField(t, "input_hash")
	if runA == "" || len(inputHash) != 64 || first.stringField(t, "status") == "" || first.boolField(t, "replayed") {
		t.Fatalf("invalid initial admission receipt: %+v", first.body)
	}

	replay := f.call(t, keyA, http.MethodPost, workflowInvocationPath("review", 1), request)
	if replay.status != http.StatusAccepted || replay.stringField(t, "run_id") != runA ||
		replay.stringField(t, "input_hash") != inputHash || !replay.boolField(t, "replayed") {
		t.Fatalf("exact replay changed the admitted run: status=%d body=%+v", replay.status, replay.body)
	}
	changedInput := map[string]any{
		"request_id": "request-1", "input": map[string]any{"document": "Changed materials"},
		"invocation_id": invocationID, "lane": "quarterly_review",
	}
	changed := f.call(t, keyA, http.MethodPost, workflowInvocationPath("review", 1), changedInput)
	if changed.status != http.StatusConflict || changed.stringField(t, "code") != "idempotency_conflict" {
		t.Fatalf("same request id with changed input was not rejected: status=%d body=%+v", changed.status, changed.body)
	}

	badID := map[string]any{"request_id": "bad-id", "input": request["input"], "invocation_id": "not-a-uuid"}
	invalidID := f.call(t, keyA, http.MethodPost, workflowInvocationPath("review", 1), badID)
	if invalidID.status != http.StatusBadRequest {
		t.Fatalf("non-UUID client invocation id status=%d body=%+v", invalidID.status, invalidID.body)
	}
	unauthorizedVersion := f.call(t, keyA, http.MethodPost, workflowInvocationPath("review", 2), map[string]any{
		"request_id": "wrong-version", "input": request["input"], "invocation_id": "22222222-2222-4222-8222-222222222222",
	})
	if unauthorizedVersion.status != http.StatusForbidden {
		t.Fatalf("ungranted capability revision status=%d body=%+v", unauthorizedVersion.status, unauthorizedVersion.body)
	}
	noGrant := f.call(t, keyA, http.MethodPost, workflowInvocationPath("ungranted", 1), map[string]any{
		"request_id": "no-grant", "input": request["input"], "invocation_id": "33333333-3333-4333-8333-333333333333",
	})
	if noGrant.status != http.StatusForbidden {
		t.Fatalf("ungranted capability invocation status=%d body=%+v", noGrant.status, noGrant.body)
	}

	got := f.call(t, keyA, http.MethodGet, "/v1/invocations/"+invocationID, nil)
	if got.status != http.StatusOK || got.stringField(t, "run_id") != runA || got.stringField(t, "input_hash") != inputHash {
		t.Fatalf("invocation read lost run projection: status=%d body=%+v", got.status, got.body)
	}
	events := f.call(t, keyA, http.MethodGet, "/v1/invocations/"+invocationID+"/events", nil)
	var eventProjection struct {
		Invocation struct {
			RunID string `json:"run_id"`
		}
	}
	if err := json.Unmarshal(events.body["invocation"], &eventProjection.Invocation); err != nil {
		t.Fatalf("decode invocation event projection: %v", err)
	}
	if events.status != http.StatusOK || eventProjection.Invocation.RunID != runA {
		t.Fatalf("event projection lost run association: status=%d body=%+v", events.status, events.body)
	}
	foreignRead := f.call(t, keyB, http.MethodGet, "/v1/invocations/"+invocationID, nil)
	if foreignRead.status != http.StatusNotFound {
		t.Fatalf("another application read invocation: status=%d body=%+v", foreignRead.status, foreignRead.body)
	}

	// The lane key is shared across capability versions within one app, while
	// the same lane and caller-supplied UUID remain independent in another app.
	laneConflict := f.call(t, keyA, http.MethodPost, workflowInvocationPath("summarize", 1), map[string]any{
		"request_id": "request-other-cap", "input": request["input"],
		"invocation_id": "44444444-4444-4444-8444-444444444444", "lane": "quarterly_review",
	})
	if laneConflict.status != http.StatusConflict || laneConflict.stringField(t, "code") != "invocation_lane_active" {
		t.Fatalf("same-app lane across capabilities was not serialized: status=%d body=%+v", laneConflict.status, laneConflict.body)
	}

	foreignRequest := map[string]any{
		"request_id": "request-1", "input": request["input"],
		"invocation_id": invocationID, "lane": "quarterly_review",
	}
	foreign := f.call(t, keyB, http.MethodPost, workflowInvocationPath("review", 1), foreignRequest)
	if foreign.status != http.StatusAccepted || foreign.stringField(t, "invocation_id") != invocationID || foreign.stringField(t, "run_id") == runA {
		t.Fatalf("application-scoped invocation identity/lane did not isolate: status=%d body=%+v", foreign.status, foreign.body)
	}
	runB := foreign.stringField(t, "run_id")

	// Process both deterministic published workflows with Weave's real TeamRun
	// and taskqueue executor. No model provider or external runtime is involved.
	runs := processNextCapabilityWorkflowRun(t, f)
	for _, runID := range []string{runA, runB} {
		finished, err := runs.Get(t.Context(), "ws", runID)
		if err != nil || finished.Status != teamrun.StatusSucceeded {
			t.Fatalf("published workflow run %s did not finish through TeamRun: status=%s err=%v", runID, finished.Status, err)
		}
	}
	projected := f.call(t, keyA, http.MethodGet, "/v1/invocations/"+invocationID, nil)
	if projected.status != http.StatusOK || projected.stringField(t, "run_id") != runA || projected.stringField(t, "status") != "completed" {
		t.Fatalf("invocation status did not project the successful TeamRun: status=%d body=%+v", projected.status, projected.body)
	}
	var projectedResult struct {
		Document string `json:"document"`
	}
	if err := json.Unmarshal(projected.body["result"], &projectedResult); err != nil || projectedResult.Document != "Quarterly materials" {
		t.Fatalf("invocation result did not project the published workflow output: result=%s err=%v", projected.body["result"], err)
	}

	var invocationCount, runCount, taskCount int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM weave_capability_workflow_invocations WHERE workspace_id='ws' AND invocation_id=$1`, invocationID).Scan(&invocationCount); err != nil || invocationCount != 2 {
		t.Fatalf("same UUID must be independently recorded for both apps: count=%d err=%v", invocationCount, err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM weave_team_run_snapshots WHERE workspace_id='ws' AND run_id=ANY($1)`, []string{runA, foreign.stringField(t, "run_id")}).Scan(&runCount); err != nil || runCount != 2 {
		t.Fatalf("expected one TeamRun snapshot per accepted invocation: count=%d err=%v", runCount, err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM weave_task_queue WHERE workspace_id='ws' AND id IN (
	 SELECT task_id FROM weave_capability_workflow_invocations WHERE workspace_id='ws' AND invocation_id=$1
	)`, invocationID).Scan(&taskCount); err != nil || taskCount != 2 {
		t.Fatalf("expected one platform task per accepted invocation: count=%d err=%v", taskCount, err)
	}
}

func TestCapabilityWorkflowCancelBeforeInvokeIsApplicationScopedRealPG(t *testing.T) {
	f := newCapabilityWorkflowContractFixture(t)
	f.publishCapability(t, "review")
	appA, keyA := f.application(t, "cancel app A")
	appB, keyB := f.application(t, "cancel app B")
	f.grant(t, appA.ID, "review", true)
	f.grant(t, appB.ID, "review", true)
	invocationID := "55555555-5555-4555-8555-555555555555"

	stop := f.call(t, keyA, http.MethodPost, "/v1/invocations/"+invocationID+"/cancel", nil)
	if stop.status != http.StatusOK || stop.stringField(t, "invocation_id") != invocationID || stop.stringField(t, "status") != "cancelled" {
		t.Fatalf("cancel-before-invoke did not persist the stop intent: status=%d body=%+v", stop.status, stop.body)
	}
	stopAgain := f.call(t, keyA, http.MethodPost, "/v1/invocations/"+invocationID+"/cancel", nil)
	if stopAgain.status != http.StatusOK || stopAgain.stringField(t, "status") != "cancelled" {
		t.Fatalf("repeated cancel changed its intent: status=%d body=%+v", stopAgain.status, stopAgain.body)
	}

	lateInvoke := f.call(t, keyA, http.MethodPost, workflowInvocationPath("review", 1), map[string]any{
		"request_id": "after-cancel", "input": map[string]any{"document": "Quarterly materials"},
		"invocation_id": invocationID, "lane": "cancelled_lane",
	})
	if lateInvoke.status == http.StatusAccepted {
		t.Fatalf("late invocation bypassed its earlier cancellation: %+v", lateInvoke.body)
	}
	projection := f.call(t, keyA, http.MethodGet, "/v1/invocations/"+invocationID, nil)
	if projection.status != http.StatusOK || projection.stringField(t, "status") != "cancelled" {
		t.Fatalf("cancel intent was not projected by GET: status=%d body=%+v", projection.status, projection.body)
	}
	foreignRead := f.call(t, keyB, http.MethodGet, "/v1/invocations/"+invocationID, nil)
	if foreignRead.status != http.StatusNotFound {
		t.Fatalf("cancel intent leaked across applications: status=%d body=%+v", foreignRead.status, foreignRead.body)
	}

	foreignInvoke := f.call(t, keyB, http.MethodPost, workflowInvocationPath("review", 1), map[string]any{
		"request_id": "after-cancel", "input": map[string]any{"document": "Quarterly materials"},
		"invocation_id": invocationID, "lane": "cancelled_lane",
	})
	if foreignInvoke.status != http.StatusAccepted || foreignInvoke.stringField(t, "invocation_id") != invocationID {
		t.Fatalf("same UUID in another application inherited the cancellation: status=%d body=%+v", foreignInvoke.status, foreignInvoke.body)
	}

	var cancelA, cancelB, invocationA, invocationB int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM weave_capability_workflow_cancel_intents WHERE workspace_id='ws' AND application_id=$1 AND invocation_id=$2`, appA.ID, invocationID).Scan(&cancelA); err != nil || cancelA != 1 {
		t.Fatalf("missing app A cancellation intent: count=%d err=%v", cancelA, err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM weave_capability_workflow_cancel_intents WHERE workspace_id='ws' AND application_id=$1 AND invocation_id=$2`, appB.ID, invocationID).Scan(&cancelB); err != nil || cancelB != 0 {
		t.Fatalf("app A cancellation polluted app B: count=%d err=%v", cancelB, err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT
	 count(*) FILTER (WHERE application_id=$1),
	 count(*) FILTER (WHERE application_id=$2)
	 FROM weave_capability_workflow_invocations WHERE workspace_id='ws' AND invocation_id=$3`, appA.ID, appB.ID, invocationID).Scan(&invocationA, &invocationB); err != nil || invocationA != 0 || invocationB != 1 {
		t.Fatalf("cancelled and live app invocations crossed identities: appA=%d appB=%d err=%v", invocationA, invocationB, err)
	}
}

type capabilityWorkflowLostResponseKernel struct {
	publication.Service
	published   publication.PublishedService
	reconciler  publication.PublishedReconciler
	failBefore  atomic.Bool
	loseAfterOK atomic.Bool
}

func (k *capabilityWorkflowLostResponseKernel) AdmitPublished(ctx context.Context, request publication.PublishedRunRequest) (publication.AdmissionReceipt, error) {
	if k.failBefore.CompareAndSwap(true, false) {
		return publication.AdmissionReceipt{}, errors.New("simulated connection failure before publication admission")
	}
	receipt, err := k.published.AdmitPublished(ctx, request)
	if err == nil && k.loseAfterOK.CompareAndSwap(true, false) {
		return publication.AdmissionReceipt{}, errors.New("simulated response loss after publication commit")
	}
	return receipt, err
}

func (k *capabilityWorkflowLostResponseKernel) ClosePublished(ctx context.Context, request publication.PublishedRunRequest) (*publication.AdmissionReceipt, error) {
	return k.reconciler.ClosePublished(ctx, request)
}

func TestCapabilityWorkflowUnknownAdmissionCommitRecoversAndGrantRevocationBlocksNewRunRealPG(t *testing.T) {
	f := newCapabilityWorkflowContractFixture(t)
	f.publishCapability(t, "review")
	app, key := f.application(t, "recovery app")
	f.grant(t, app.ID, "review", true)

	real := f.server.KernelPublication
	published, ok := real.(publication.PublishedService)
	if !ok {
		t.Fatal("published workflow admission service is unavailable")
	}
	reconciler, ok := real.(publication.PublishedReconciler)
	if !ok {
		t.Fatal("published workflow admission reconciliation is unavailable")
	}
	kernel := &capabilityWorkflowLostResponseKernel{Service: real, published: published, reconciler: reconciler}
	kernel.loseAfterOK.Store(true)
	f.server.KernelPublication = kernel
	request := map[string]any{
		"request_id":    "unknown-commit",
		"input":         map[string]any{"document": "Response recovery materials"},
		"invocation_id": "66666666-6666-4666-8666-666666666666",
		"lane":          "recovery_lane",
	}
	first := f.call(t, key, http.MethodPost, workflowInvocationPath("review", 1), request)
	if first.status < http.StatusInternalServerError {
		t.Fatalf("fault injection did not lose the post-commit response: status=%d body=%+v", first.status, first.body)
	}
	var acceptedTasks, acceptedSnapshots int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM weave_task_queue`).Scan(&acceptedTasks); err != nil || acceptedTasks != 1 {
		t.Fatalf("post-commit response loss did not leave exactly one admitted task: count=%d err=%v", acceptedTasks, err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM weave_team_run_snapshots`).Scan(&acceptedSnapshots); err != nil || acceptedSnapshots != 1 {
		t.Fatalf("post-commit response loss did not leave exactly one run snapshot: count=%d err=%v", acceptedSnapshots, err)
	}

	recovered := f.call(t, key, http.MethodPost, workflowInvocationPath("review", 1), request)
	if recovered.status != http.StatusAccepted || !recovered.boolField(t, "replayed") {
		t.Fatalf("same request did not recover the existing admission: status=%d body=%+v", recovered.status, recovered.body)
	}
	if recovered.stringField(t, "invocation_id") != request["invocation_id"] || recovered.stringField(t, "run_id") == "" {
		t.Fatalf("recovered response lost invocation/run identity: %+v", recovered.body)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM weave_task_queue`).Scan(&acceptedTasks); err != nil || acceptedTasks != 1 {
		t.Fatalf("response recovery created another task: count=%d err=%v", acceptedTasks, err)
	}

	if err := f.access.SetGrant(t.Context(), "ws", "user", capabilities.VersionGrant{
		AppID: app.ID, CapabilityID: "review", Revision: 1, Enabled: false,
	}); err != nil {
		t.Fatalf("revoke exact capability version grant: %v", err)
	}
	revoked := f.call(t, key, http.MethodPost, workflowInvocationPath("review", 1), map[string]any{
		"request_id": "after-revoke", "input": map[string]any{"document": "New materials"},
		"invocation_id": "77777777-7777-4777-8777-777777777777",
	})
	if revoked.status != http.StatusForbidden {
		t.Fatalf("revoked capability version admitted a new run: status=%d body=%+v", revoked.status, revoked.body)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM weave_task_queue`).Scan(&acceptedTasks); err != nil || acceptedTasks != 1 {
		t.Fatalf("revoked grant created another platform task: count=%d err=%v", acceptedTasks, err)
	}
}

func TestCapabilityWorkflowGrantRevocationDoesNotLeaveClosedAdmissionPendingRealPG(t *testing.T) {
	f := newCapabilityWorkflowContractFixture(t)
	f.publishCapability(t, "review")
	app, key := f.application(t, "pending revocation app")
	f.grant(t, app.ID, "review", true)

	real := f.server.KernelPublication
	published, ok := real.(publication.PublishedService)
	if !ok {
		t.Fatal("published workflow admission service is unavailable")
	}
	reconciler, ok := real.(publication.PublishedReconciler)
	if !ok {
		t.Fatal("published workflow admission reconciliation is unavailable")
	}
	kernel := &capabilityWorkflowLostResponseKernel{Service: real, published: published, reconciler: reconciler}
	kernel.failBefore.Store(true)
	f.server.KernelPublication = kernel
	invocationID := "88888888-8888-4888-8888-888888888888"
	first := f.call(t, key, http.MethodPost, workflowInvocationPath("review", 1), map[string]any{
		"request_id": "pending-before-kernel", "input": map[string]any{"document": "Pending review"},
		"invocation_id": invocationID, "lane": "pending_lane",
	})
	if first.status < http.StatusInternalServerError {
		t.Fatalf("fault injection did not leave an unaccepted admission: status=%d body=%+v", first.status, first.body)
	}
	var tasks int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM weave_task_queue`).Scan(&tasks); err != nil || tasks != 0 {
		t.Fatalf("pre-acceptance failure unexpectedly created a platform task: count=%d err=%v", tasks, err)
	}

	manager := f.adminToken(t)
	revoked := f.call(t, manager, http.MethodPost, "/v1/capability-apps/actions", map[string]any{
		"action": "grant", "app_id": app.ID, "capability_id": "review", "revision": 1, "enabled": false,
	})
	if revoked.status != http.StatusNoContent {
		t.Fatalf("revoke capability grant status=%d body=%+v", revoked.status, revoked.body)
	}
	var operation string
	if err := f.pool.QueryRow(t.Context(), `SELECT k.operation FROM weave_kernel_publication_requests k
	 JOIN weave_capability_workflow_invocations i ON i.workspace_id=k.workspace_id AND i.admission_request_id=k.request_id
	 WHERE i.workspace_id='ws' AND i.application_id=$1 AND i.invocation_id=$2`, app.ID, invocationID).Scan(&operation); err != nil || operation != "published_closed" {
		t.Fatalf("revoked pending admission was not fenced closed: operation=%q err=%v", operation, err)
	}

	view := f.call(t, key, http.MethodGet, "/v1/invocations/"+invocationID, nil)
	if view.status != http.StatusOK {
		t.Fatalf("read revoked invocation status=%d body=%+v", view.status, view.body)
	}
	status := view.stringField(t, "status")
	if status != "cancelled" {
		t.Fatalf("permanently closed admission is still projected as active: status=%q body=%+v", status, view.body)
	}
}

type capabilityWorkflowBlockingRuntime struct {
	started chan struct{}
	once    sync.Once
}

func (r *capabilityWorkflowBlockingRuntime) Execute(ctx context.Context, _ teamrun.TeamRun, _ *taskqueue.Task) (teamrun.RuntimeResult, error) {
	r.once.Do(func() { close(r.started) })
	<-ctx.Done()
	return teamrun.RuntimeResult{Status: teamrun.RuntimeFailed}, ctx.Err()
}

func (r *capabilityWorkflowBlockingRuntime) ResumeCheckpoint(ctx context.Context, _ teamrun.TeamRun, _ *taskqueue.Task, _ teamrun.WorkflowCheckpointV1) (teamrun.RuntimeResult, error) {
	return r.Execute(ctx, teamrun.TeamRun{}, nil)
}

func (r *capabilityWorkflowBlockingRuntime) TimerResumeTarget(context.Context, teamrun.TeamRun, *taskqueue.Task, teamrun.WorkflowCheckpointV1) (string, bool, error) {
	return "", false, nil
}

func TestCapabilityWorkflowDisablingApplicationStopsRunningTeamRunOnceRealPG(t *testing.T) {
	f := newCapabilityWorkflowContractFixture(t)
	f.publishCapability(t, "review")
	app, key := f.application(t, "running revoke app")
	f.grant(t, app.ID, "review", true)
	accepted := f.call(t, key, http.MethodPost, workflowInvocationPath("review", 1), map[string]any{
		"request_id": "running-revoke", "input": map[string]any{"document": "Running review"},
		"invocation_id": "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee",
	})
	if accepted.status != http.StatusAccepted {
		t.Fatalf("initial workflow invocation status=%d body=%+v", accepted.status, accepted.body)
	}
	runID := accepted.stringField(t, "run_id")

	runs := teamrun.NewPGStore()
	runs.Transactions = f.pool
	checkpoints := teamrun.NewPGCheckpointStore()
	blocking := &capabilityWorkflowBlockingRuntime{started: make(chan struct{})}
	executor := &teamrun.Executor{
		Tasks: f.server.Tasks, Transactions: f.pool, Runs: runs, Checkpoints: checkpoints,
		Runtime:           blocking,
		Consumer:          &teamrun.Consumer{Transactions: f.pool, Snapshots: f.server.Snapshots, Runs: runs, Tasks: f.server.Tasks},
		HeartbeatInterval: 5 * time.Millisecond,
	}
	type processResult struct {
		worked bool
		err    error
	}
	processed := make(chan processResult, 1)
	go func() {
		worked, err := executor.ProcessNext(t.Context(), "running-revocation-worker")
		processed <- processResult{worked: worked, err: err}
	}()
	select {
	case <-blocking.started:
	case <-time.After(5 * time.Second):
		t.Fatal("published TeamRun did not enter its controlled running phase")
	}
	started, err := runs.Get(t.Context(), "ws", runID)
	if err != nil || started.Status != teamrun.StatusRunning {
		t.Fatalf("TeamRun was not running before revocation: status=%s err=%v", started.Status, err)
	}

	manager := f.adminToken(t)
	disabled := f.call(t, manager, http.MethodPost, "/v1/capability-apps/actions", map[string]any{
		"action": "enable", "app_id": app.ID, "enabled": false,
	})
	if disabled.status != http.StatusNoContent {
		t.Fatalf("disable application status=%d body=%+v", disabled.status, disabled.body)
	}
	stopping, err := runs.Get(t.Context(), "ws", runID)
	if err != nil || stopping.Status != teamrun.StatusCancelRequested {
		t.Fatalf("running TeamRun was not asked to stop after app disable: status=%s err=%v", stopping.Status, err)
	}

	select {
	case result := <-processed:
		if !result.worked || result.err == nil {
			t.Fatalf("controlled worker did not stop on revoked ownership: worked=%v err=%v", result.worked, result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("running TeamRun worker did not observe the cancellation fence")
	}
	settled, err := (&teamrun.CancelGraceSweeper{Tasks: f.server.Tasks, Transactions: f.pool, Runs: runs}).Sweep(t.Context())
	if err != nil || settled != 1 {
		t.Fatalf("settle confirmed TeamRun cancellation: count=%d err=%v", settled, err)
	}
	finished, err := runs.Get(t.Context(), "ws", runID)
	if err != nil || finished.Status != teamrun.StatusCancelled {
		t.Fatalf("revoked running TeamRun terminal status=%s err=%v", finished.Status, err)
	}
	var terminalTransitions int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM weave_team_run_transitions
	 WHERE workspace_id='ws' AND run_id=$1 AND to_status IN ('succeeded','failed','cancelled','abandoned')`, runID).Scan(&terminalTransitions); err != nil || terminalTransitions != 1 {
		t.Fatalf("revocation wrote a second or wrong TeamRun terminal: transitions=%d err=%v", terminalTransitions, err)
	}
}

func TestCapabilityWorkflowReplayAfterRunCancellationUsesCancelledQueueProjectionRealPG(t *testing.T) {
	f := newCapabilityWorkflowContractFixture(t)
	f.publishCapability(t, "review")
	app, key := f.application(t, "cancel replay app")
	f.grant(t, app.ID, "review", true)
	invocationID := "99999999-9999-4999-8999-999999999999"
	request := map[string]any{
		"request_id": "cancel-replay", "input": map[string]any{"document": "Cancelled report"},
		"invocation_id": invocationID, "lane": "cancel_replay_lane",
	}
	first := f.call(t, key, http.MethodPost, workflowInvocationPath("review", 1), request)
	if first.status != http.StatusAccepted {
		t.Fatalf("initial workflow invoke status=%d body=%+v", first.status, first.body)
	}
	runID := first.stringField(t, "run_id")
	stop := f.call(t, key, http.MethodPost, "/v1/invocations/"+invocationID+"/cancel", nil)
	if stop.status != http.StatusOK || stop.stringField(t, "status") != "cancelled" {
		t.Fatalf("cancel queued TeamRun task status=%d body=%+v", stop.status, stop.body)
	}

	replay := f.call(t, key, http.MethodPost, workflowInvocationPath("review", 1), request)
	if replay.status != http.StatusAccepted || !replay.boolField(t, "replayed") ||
		replay.stringField(t, "run_id") != runID || replay.stringField(t, "status") != "cancelled" {
		t.Fatalf("exact replay did not project the existing cancelled execution: status=%d body=%+v", replay.status, replay.body)
	}
	changedRequest := map[string]any{
		"request_id": "cancel-replay-different-request", "input": map[string]any{"document": "Cancelled report"},
		"invocation_id": invocationID, "lane": "cancel_replay_lane",
	}
	reusedCancelledID := f.call(t, key, http.MethodPost, workflowInvocationPath("review", 1), changedRequest)
	if reusedCancelledID.status != http.StatusConflict || reusedCancelledID.stringField(t, "code") != "invocation_identity_conflict" {
		t.Fatalf("a cancelled invocation ID was accepted under a new request ID: status=%d body=%+v", reusedCancelledID.status, reusedCancelledID.body)
	}
	var taskID, taskStatus string
	if err := f.pool.QueryRow(t.Context(), `SELECT task_id FROM weave_capability_workflow_invocations
	 WHERE workspace_id='ws' AND application_id=$1 AND invocation_id=$2`, app.ID, invocationID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT status FROM weave_task_queue WHERE workspace_id='ws' AND id=$1`, taskID).Scan(&taskStatus); err != nil || taskStatus != "cancelled" {
		t.Fatalf("replayed cancellation differs from the taskqueue owner: status=%q err=%v", taskStatus, err)
	}
	var tasks, runs int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM weave_task_queue WHERE workspace_id='ws' AND id=$1`, taskID).Scan(&tasks); err != nil || tasks != 1 {
		t.Fatalf("replay created another task: count=%d err=%v", tasks, err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM weave_team_runs WHERE workspace_id='ws' AND run_id=$1`, runID).Scan(&runs); err != nil || runs != 0 {
		t.Fatalf("queued cancellation fabricated a second TeamRun terminal fact: count=%d err=%v", runs, err)
	}
}

func TestCapabilityWorkflowClosedAdmissionReleasesApplicationLaneRealPG(t *testing.T) {
	f := newCapabilityWorkflowContractFixture(t)
	f.publishCapability(t, "review")
	f.publishCapability(t, "summarize")
	app, key := f.application(t, "closed lane app")
	f.grant(t, app.ID, "review", true)
	f.grant(t, app.ID, "summarize", true)

	real := f.server.KernelPublication
	published, ok := real.(publication.PublishedService)
	if !ok {
		t.Fatal("published workflow admission service is unavailable")
	}
	reconciler, ok := real.(publication.PublishedReconciler)
	if !ok {
		t.Fatal("published workflow admission reconciliation is unavailable")
	}
	kernel := &capabilityWorkflowLostResponseKernel{Service: real, published: published, reconciler: reconciler}
	kernel.failBefore.Store(true)
	f.server.KernelPublication = kernel
	closedID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	first := f.call(t, key, http.MethodPost, workflowInvocationPath("review", 1), map[string]any{
		"request_id": "closed-lane-first", "input": map[string]any{"document": "Not accepted"},
		"invocation_id": closedID, "lane": "single-reviewer",
	})
	if first.status < http.StatusInternalServerError {
		t.Fatalf("fault injection did not leave a cancellable pre-admission intent: status=%d body=%+v", first.status, first.body)
	}
	closed := f.call(t, key, http.MethodPost, "/v1/invocations/"+closedID+"/cancel", nil)
	if closed.status != http.StatusOK || closed.stringField(t, "status") != "cancelled" {
		t.Fatalf("close unaccepted invocation status=%d body=%+v", closed.status, closed.body)
	}
	var operation string
	if err := f.pool.QueryRow(t.Context(), `SELECT k.operation FROM weave_kernel_publication_requests k
	 JOIN weave_capability_workflow_invocations i ON i.workspace_id=k.workspace_id AND i.admission_request_id=k.request_id
	 WHERE i.workspace_id='ws' AND i.application_id=$1 AND i.invocation_id=$2`, app.ID, closedID).Scan(&operation); err != nil || operation != "published_closed" {
		t.Fatalf("pre-admission cancellation did not persist Kernel closure: operation=%q err=%v", operation, err)
	}

	second := f.call(t, key, http.MethodPost, workflowInvocationPath("summarize", 1), map[string]any{
		"request_id": "closed-lane-second", "input": map[string]any{"document": "Accepted report"},
		"invocation_id": "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "lane": "single-reviewer",
	})
	if second.status != http.StatusAccepted || second.stringField(t, "run_id") == "" {
		t.Fatalf("closed no-receipt request kept its lane occupied: status=%d body=%+v", second.status, second.body)
	}
	var tasks int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM weave_task_queue`).Scan(&tasks); err != nil || tasks != 1 {
		t.Fatalf("only the post-closure invocation should enqueue a task: count=%d err=%v", tasks, err)
	}
}

func TestCapabilityWorkflowCredentialRotationReplaysSameInvocationRealPG(t *testing.T) {
	f := newCapabilityWorkflowContractFixture(t)
	f.publishCapability(t, "review")
	app, oldKey := f.application(t, "rotation app")
	f.grant(t, app.ID, "review", true)
	request := map[string]any{
		"request_id": "rotation-replay", "input": map[string]any{"document": "Rotation materials"},
		"invocation_id": "cccccccc-cccc-4ccc-8ccc-cccccccccccc", "lane": "rotation_lane",
	}
	first := f.call(t, oldKey, http.MethodPost, workflowInvocationPath("review", 1), request)
	if first.status != http.StatusAccepted {
		t.Fatalf("initial invocation status=%d body=%+v", first.status, first.body)
	}

	_, newKey, err := f.access.IssueCredential(t.Context(), "ws", "user", app.ID, "replacement", []string{"invoke", "read", "cancel"})
	if err != nil {
		t.Fatal(err)
	}
	var oldCredential capabilities.ApplicationCredential
	if err := f.pool.QueryRow(t.Context(), `SELECT id FROM weave_capability_credentials
	 WHERE workspace_id='ws' AND app_id=$1 AND revoked_at IS NULL AND name=$2`, app.ID, "rotation app key").Scan(&oldCredential.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.access.RevokeCredential(t.Context(), "ws", "user", app.ID, oldCredential.ID); err != nil {
		t.Fatalf("revoke original credential: %v", err)
	}

	replay := f.call(t, newKey, http.MethodPost, workflowInvocationPath("review", 1), request)
	if replay.status != http.StatusAccepted || !replay.boolField(t, "replayed") || replay.stringField(t, "run_id") != first.stringField(t, "run_id") {
		t.Fatalf("rotated app key did not recover the same invocation: status=%d body=%+v", replay.status, replay.body)
	}
	if oldRead := f.call(t, oldKey, http.MethodGet, "/v1/invocations/cccccccc-cccc-4ccc-8ccc-cccccccccccc", nil); oldRead.status != http.StatusUnauthorized {
		t.Fatalf("revoked credential retained API access: status=%d body=%+v", oldRead.status, oldRead.body)
	}
	newRead := f.call(t, newKey, http.MethodGet, "/v1/invocations/cccccccc-cccc-4ccc-8ccc-cccccccccccc", nil)
	if newRead.status != http.StatusOK || newRead.stringField(t, "run_id") != first.stringField(t, "run_id") {
		t.Fatalf("replacement credential cannot read app-owned invocation: status=%d body=%+v", newRead.status, newRead.body)
	}
	var tasks int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM weave_task_queue`).Scan(&tasks); err != nil || tasks != 1 {
		t.Fatalf("credential rotation duplicated the admitted task: count=%d err=%v", tasks, err)
	}
}

func TestCapabilityWorkflowOptionalInvocationIDValidationRealPG(t *testing.T) {
	f := newCapabilityWorkflowContractFixture(t)
	f.publishCapability(t, "review")
	app, key := f.application(t, "optional ID app")
	f.grant(t, app.ID, "review", true)
	input := map[string]any{"document": "Optional identity"}

	omitted := f.call(t, key, http.MethodPost, workflowInvocationPath("review", 1), map[string]any{
		"request_id": "generated-id", "input": input,
	})
	if omitted.status != http.StatusAccepted {
		t.Fatalf("omitted invocation_id should be server-generated: status=%d body=%+v", omitted.status, omitted.body)
	}
	generated := omitted.stringField(t, "invocation_id")
	parsed, err := uuid.Parse(generated)
	if err != nil || parsed.String() != generated {
		t.Fatalf("server-generated invocation id is not a canonical UUID: %q err=%v", generated, err)
	}

	for _, invalid := range []struct {
		requestID string
		value     any
	}{
		{requestID: "empty-id", value: ""},
		{requestID: "null-id", value: nil},
		{requestID: "invalid-id", value: "not-a-uuid"},
	} {
		result := f.call(t, key, http.MethodPost, workflowInvocationPath("review", 1), map[string]any{
			"request_id": invalid.requestID, "input": input, "invocation_id": invalid.value,
		})
		if result.status != http.StatusBadRequest {
			t.Fatalf("invocation_id=%#v should be rejected, got status=%d body=%+v", invalid.value, result.status, result.body)
		}
	}
	var rows int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM weave_capability_workflow_invocations WHERE workspace_id='ws' AND application_id=$1`, app.ID).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("invalid optional invocation IDs created records: count=%d err=%v", rows, err)
	}
}
