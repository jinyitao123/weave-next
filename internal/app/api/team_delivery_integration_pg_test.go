package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/app/deliveryverify"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/base/fileartifact"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/base/storeext"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/delivery"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	"github.com/jinyitao123/weave/internal/kernel/mcpregistry"
	"github.com/jinyitao123/weave/internal/kernel/org"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/jinyitao123/weave/internal/kernel/schedule"
	"github.com/jinyitao123/weave/internal/kernel/skills"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/labstack/echo/v4"
)

// These are mechanism acceptance tests. Both adapters execute real frozen
// workflows, durable physical results, exact-source loading and final recording.
// The only external system is an isolated object table in the test schema.
func TestTeamDeliveryVerificationMatrixCLIAndLoomRealPG(t *testing.T) {
	for _, engineName := range []string{engine.Claude, "loom"} {
		t.Run(engineName, func(t *testing.T) {
			fixture := newTeamDeliveryFixture(t, engineName)
			for _, scenario := range []teamDeliveryScenario{
				{name: "required file missing", missingFile: true, want: deliverable.VerificationFailed, reason: "required_file_missing"},
				{name: "PASS prose lacks required check", missingCheck: true, want: deliverable.VerificationUnknown, reason: "verifier_unavailable"},
				{name: "isolated effects contain extra object", extraObject: true, want: deliverable.VerificationFailed, reason: "unexpected_objects_observed"},
				{name: "all frozen requirements satisfied", want: deliverable.VerificationPassed, reason: "isolated_effects_match"},
			} {
				t.Run(scenario.name, func(t *testing.T) { fixture.run(t, scenario) })
			}
		})
	}
}

type teamDeliveryScenario struct {
	name                                   string
	missingFile, missingCheck, extraObject bool
	want                                   deliverable.VerificationStatus
	reason                                 string
}

type teamDeliveryFixture struct {
	pool                                  *pgxpool.Pool
	server                                *Server
	runs                                  *teamrun.PGStore
	tasks                                 *taskqueue.Store
	flows                                 *workflow.Store
	snapshots                             *snapshot.Store
	runtimeStore                          *runtimes.Store
	runtime                               *runtimes.Runtime
	workerID, engineName, publishedDigest string
	keys                                  []byte
}

func newTeamDeliveryFixture(t *testing.T, engineName string) *teamDeliveryFixture {
	t.Helper()
	ctx := t.Context()
	seedPool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, seedPool); err != nil {
		t.Fatal(err)
	}
	if _, err := seedPool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES('ws','ws','ws'); INSERT INTO weave_users(id,tenant_id,username,password,role) VALUES('user','ws','user','x','admin'); CREATE TABLE fixture_delivery_objects(workspace_id text NOT NULL,run_id text NOT NULL,object_id text NOT NULL,value integer NOT NULL,PRIMARY KEY(workspace_id,run_id,object_id));`); err != nil {
		t.Fatal(err)
	}
	cfg := seedPool.Config()
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	f := &teamDeliveryFixture{pool: pool, engineName: engineName, keys: []byte(strings.Repeat("k", 32))}
	if engineName == "loom" {
		mcp := mcpregistry.New(pool, f.keys)
		registered, err := mcp.Create(ctx, "ws", "user", mcpregistry.UpsertServerRequest{Slug: "compute", DisplayName: "Compute", Transport: mcpregistry.TransportStreamableHTTP, URL: "http://127.0.0.1:1", Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := mcp.RecordProbeSuccess(ctx, "ws", registered.ID, "2025-03-26", json.RawMessage(`{}`), []mcpregistry.Tool{{Name: "compute", InputSchema: json.RawMessage(`{"type":"object"}`)}}); err != nil {
			t.Fatal(err)
		}
		bundle, _, published := publishMemberIntegrationSample(t, pool, f.keys, "ws", "http://127.0.0.1:1", registered.ID, registered.FunctionalRevision, "compute", "Compute and export the exact result.")
		f.workerID, f.publishedDigest = bundle.Agent.AgentID, published.ContentHash
	} else {
		f.runtimeStore = runtimes.NewStore(pool)
		runtime, _, err := f.runtimeStore.Create(ctx, "ws", "Isolated CLI")
		if err != nil {
			t.Fatal(err)
		}
		f.runtime = runtime
		if err := f.runtimeStore.HelloWithCapabilities(ctx, "ws", runtime.ID, []string{engineName}, []runtimes.EngineCapability{{Engine: engineName, BinaryPath: "/fixture/claude", BinaryVersion: "fixture-cli 1", ProtocolVersion: "1", AuthMode: runtimes.AuthModeOAuth, EndpointClass: "fixture"}}, 1); err != nil {
			t.Fatal(err)
		}
		f.workerID, f.publishedDigest = publishTeamDeliveryCLI(t, pool, f.keys, runtime.ID)
	}
	f.tasks = taskqueue.New(pool, nil, time.Minute)
	f.flows = workflow.New(pool, nil)
	f.snapshots = snapshot.NewStore(pool)
	f.runs = teamrun.NewPGStore()
	f.runs.Transactions = pool
	f.server = &Server{Store: teamDeliveryPoolStore{teamDispatchPoolStore{pool: pool}}, OrgStore: org.NewStore(pool), Registry: registry.New(pool), Workflow: f.flows, ScheduleTransactions: pool, Snapshots: f.snapshots, Tasks: f.tasks, Runtimes: f.runtimeStore, Deliverables: deliveryverify.NewStore(pool), teamRunCancel: &teamrun.CancelService{Transactions: pool, Runs: f.runs, Tasks: f.tasks}}
	return f
}

func (f *teamDeliveryFixture) run(t *testing.T, scenario teamDeliveryScenario) teamDeliveryExecution {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	verifierCalls := &atomic.Int32{}
	verifiers := deliveryverify.NewRegistry()
	if err := verifiers.RegisterExternalEffects("fixture.isolated-effects", "v1", teamDeliveryEffectsVerifier(f.pool, verifierCalls)); err != nil {
		t.Fatal(err)
	}
	outputStore := deliverable.NewWithVerifiers(f.pool, verifiers)
	capture := &teamDeliveryRecorder{Store: outputStore}
	f.server.Deliverables = outputStore
	checks := []deliverable.CheckSpec{{ID: "effects", VerifierID: "fixture.isolated-effects", VerifierVersion: "v1", Parameters: json.RawMessage(`{"expected_ids":["expected"],"expected_value":42}`)}}
	if scenario.missingCheck {
		checks = append(checks, deliverable.CheckSpec{ID: "required-review", VerifierID: "fixture.unregistered-review", VerifierVersion: "v1"})
	}
	contractInput := map[string]any{"version": 1, "coverage": "explicit", "required_artifacts": []deliverable.ArtifactRequirement{{ID: "result", Path: "model/result.json", ContentType: "application/json", Contains: []string{`"value":42`}}}, "required_checks": checks, "external_effects_check_id": "effects"}
	raw, _ := json.Marshal(contractInput)
	taskText := "Calculate the assigned result and report PASS only as prose.\n```weave-delivery-contract-v1\n" + string(raw) + "\n```"
	registered, err := registerInputForTest(f.server, dispatchInputRegistrationFixture("delivery-"+scenario.name, taskText, ""))
	if err != nil || registered.Code != http.StatusCreated {
		t.Fatalf("register=%d %s %v", registered.Code, registered.Body.String(), err)
	}
	var receipt dispatchInputReceipt
	if err := json.Unmarshal(registered.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	dispatchedResponse, err := boundDispatchForTest(f.server, map[string]any{"input_revision_id": receipt.InputRevisionID, "client_request_id": receipt.ClientRequestID}, "user")
	if err != nil || dispatchedResponse.Code != http.StatusCreated {
		t.Fatalf("dispatch=%d %s %v", dispatchedResponse.Code, dispatchedResponse.Body.String(), err)
	}
	var dispatched workflowManualRunResponse
	if err := json.Unmarshal(dispatchedResponse.Body.Bytes(), &dispatched); err != nil {
		t.Fatal(err)
	}
	frozenState, err := outputStore.GetDeliveryState(ctx, "ws", dispatched.RunID)
	if err != nil || frozenState.Binding.InputRevisionID != receipt.InputRevisionID || frozenState.Binding.PublishedDigest != f.publishedDigest || frozenState.Binding.Contract == nil || frozenState.Binding.Contract.Coverage != deliverable.CoverageExplicit || frozenState.Binding.Contract.RequiredArtifacts[0].Path != "model/result.json" {
		t.Fatalf("dispatch did not freeze exact user contract: %#v %v", frozenState, err)
	}
	checkpoints := teamrun.NewPGCheckpointStore()
	members, err := loomruntime.NewMemberRunner(storeext.New(f.pool))
	if err != nil {
		t.Fatal(err)
	}
	loader := &workflow.RuntimeLoader{Registry: memberIntegrationDescriptors(t)}
	hosts := &teamDeliveryHosts{workerID: f.workerID, pool: f.pool, runID: dispatched.RunID, scenario: scenario}
	runtime := &teamrun.WorkflowSerialRuntime{OutputRecorder: capture, Members: members, Artifacts: f.flows, Loader: loader, HostFactory: hosts, CredentialResolvers: func(string) (workflow.RuntimeCredentialResolver, error) { return memberIntegrationSecrets{}, nil }, Transactions: f.pool, Runs: f.runs, Checkpoints: checkpoints, Tasks: f.tasks, Snapshots: f.snapshots}
	var workerDone <-chan error
	if f.engineName != "loom" {
		loader.CLIExecutor = runtimes.NewExecutor(f.tasks, f.runtimeStore, "", "")
		workerDone = f.startCLIWorker(ctx, scenario)
	}
	executor := &teamrun.Executor{Tasks: f.tasks, Transactions: f.pool, Runs: f.runs, Checkpoints: checkpoints, Runtime: runtime, Consumer: &teamrun.Consumer{Transactions: f.pool, Snapshots: f.snapshots, Runs: f.runs, Tasks: f.tasks}}
	if ok, err := executor.ProcessNext(ctx, "delivery-executor"); err != nil || !ok {
		t.Fatalf("execute=%v %v", ok, err)
	}
	finished, err := f.runs.Get(ctx, "ws", dispatched.RunID)
	if err != nil || finished.Status != teamrun.StatusSucceeded {
		cancel()
		cause := ""
		if finished.CauseSummary != nil {
			cause = *finished.CauseSummary
		}
		t.Fatalf("business verification rewrote engine completion: status=%s cause=%s error=%v", finished.Status, cause, err)
	}
	if workerDone != nil {
		select {
		case err := <-workerDone:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	state, err := outputStore.GetDeliveryState(ctx, "ws", dispatched.RunID)
	if err != nil || state.Report == nil || state.Report.Status != scenario.want || state.ContractDigest != frozenState.ContractDigest || state.Report.RevisionID != state.RevisionID {
		t.Fatalf("delivery state=%#v error=%v", state, err)
	}
	if !teamDeliveryHasReason(state.Report.Checks, scenario.reason) {
		t.Fatalf("missing actual verdict evidence %s: %+v", scenario.reason, state.Report.Checks)
	}
	if verifierCalls.Load() != 1 {
		t.Fatalf("registered effects verifier calls=%d", verifierCalls.Load())
	}
	f.assertPhysicalSources(t, ctx, dispatched.RunID, state.Report, scenario)
	f.assertRunActivity(t, ctx, dispatched.RunID, state, scenario)
	if len(capture.outputs) == 0 || !strings.HasSuffix(capture.fence.ExecutorID, ":delivery-executor") {
		t.Fatalf("runtime bypassed verified final recorder: %+v", capture.fence)
	}
	if scenario.missingCheck && fmt.Sprint(capture.outputs[len(capture.outputs)-1].Output) != "PASS" {
		t.Fatalf("fixture did not carry the explicit PASS prose to final delivery: %#v", capture.outputs)
	}
	return teamDeliveryExecution{store: outputStore, capture: capture, state: state, runID: dispatched.RunID}
}

type teamDeliveryExecution struct {
	store   *deliverable.Store
	capture *teamDeliveryRecorder
	state   deliverable.DeliveryState
	runID   string
}

// Fence rejection is separate from the four successful execution cases. A late
// submitter cannot replace the report after the real TeamRun reaches succeeded.
func TestTeamDeliveryRejectsLateSubmissionFenceCLIAndLoomRealPG(t *testing.T) {
	for _, engineName := range []string{engine.Claude, "loom"} {
		t.Run(engineName, func(t *testing.T) {
			fixture := newTeamDeliveryFixture(t, engineName)
			result := fixture.run(t, teamDeliveryScenario{name: "fence baseline", want: deliverable.VerificationPassed, reason: "isolated_effects_match"})
			beforeSequence := result.state.SelectionSequence
			if _, err := result.store.RecordVerifiedWorkflowOutputs(t.Context(), result.capture.outputs, result.capture.fence); !errors.Is(err, deliverable.ErrVerificationFence) {
				t.Fatalf("terminal stale fence accepted: %v", err)
			}
			after, err := result.store.GetDeliveryState(t.Context(), "ws", result.runID)
			if err != nil || after.SelectionSequence != beforeSequence || after.VerificationID != result.state.VerificationID {
				t.Fatalf("late result changed current report: %#v %v", after, err)
			}
			run, err := fixture.runs.Get(t.Context(), "ws", result.runID)
			if err != nil || run.Status != teamrun.StatusSucceeded {
				t.Fatalf("late verification rewrote execution: %+v %v", run, err)
			}
		})
	}
}

type teamDeliveryRecorder struct {
	*deliverable.Store
	outputs []deliverable.WorkflowOutput
	fence   deliverable.VerificationFence
}

func (r *teamDeliveryRecorder) RecordVerifiedWorkflowOutputs(ctx context.Context, outputs []deliverable.WorkflowOutput, fence deliverable.VerificationFence) (deliverable.VerificationReport, error) {
	r.outputs, r.fence = outputs, fence
	return r.Store.RecordVerifiedWorkflowOutputs(ctx, outputs, fence)
}

func teamDeliveryHasReason(checks []deliverable.CheckResult, reason string) bool {
	for _, check := range checks {
		if check.Reason == reason {
			return true
		}
	}
	return false
}

func teamDeliveryEffectsVerifier(pool *pgxpool.Pool, calls *atomic.Int32) deliverable.Verifier {
	return func(ctx context.Context, input deliverable.VerificationInput) (deliverable.CheckResult, error) {
		calls.Add(1)
		var params struct {
			ExpectedIDs   []string `json:"expected_ids"`
			ExpectedValue int      `json:"expected_value"`
		}
		decoder := json.NewDecoder(bytes.NewReader(input.Check.Parameters))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&params); err != nil {
			return deliverable.CheckResult{}, err
		}
		if len(params.ExpectedIDs) != 1 || params.ExpectedIDs[0] != "expected" || params.ExpectedValue != 42 {
			return deliverable.CheckResult{}, errors.New("unsupported isolated effects scope")
		}
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead})
		if err != nil {
			return deliverable.CheckResult{}, err
		}
		defer tx.Rollback(ctx)
		rows, err := tx.Query(ctx, `SELECT object_id,value FROM fixture_delivery_objects WHERE workspace_id=$1 AND run_id=$2 ORDER BY object_id`, input.Candidate.WorkspaceID, input.Candidate.RunID)
		if err != nil {
			return deliverable.CheckResult{}, err
		}
		type object struct {
			ID    string `json:"id"`
			Value int    `json:"value"`
		}
		observed := []object{}
		for rows.Next() {
			var item object
			if err := rows.Scan(&item.ID, &item.Value); err != nil {
				rows.Close()
				return deliverable.CheckResult{}, err
			}
			observed = append(observed, item)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return deliverable.CheckResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return deliverable.CheckResult{}, err
		}
		status, reason := deliverable.VerificationPassed, "isolated_effects_match"
		expectedFound := false
		extra := []string{}
		for _, item := range observed {
			if item.ID == params.ExpectedIDs[0] && item.Value == params.ExpectedValue {
				expectedFound = true
			} else {
				extra = append(extra, item.ID)
			}
		}
		if !expectedFound {
			status, reason = deliverable.VerificationFailed, "required_object_missing_or_wrong"
		}
		if len(extra) > 0 {
			status, reason = deliverable.VerificationFailed, "unexpected_objects_observed"
		}
		evidence, err := json.Marshal(map[string]any{"system": "test-schema-object-store", "workspace_id": input.Candidate.WorkspaceID, "run_id": input.Candidate.RunID, "objects": observed, "unexpected_objects": extra, "complete": true, "observed_at": time.Now().UTC(), "scope": "all final objects in this isolated run", "adapter_version": "v1"})
		return deliverable.CheckResult{Status: status, Reason: reason, Evidence: evidence}, err
	}
}

func teamDeliveryWriteEffects(ctx context.Context, pool *pgxpool.Pool, runID string, extra bool) error {
	if _, err := pool.Exec(ctx, `INSERT INTO fixture_delivery_objects(workspace_id,run_id,object_id,value) VALUES('ws',$1,'expected',42)`, runID); err != nil {
		return err
	}
	if extra {
		_, err := pool.Exec(ctx, `INSERT INTO fixture_delivery_objects(workspace_id,run_id,object_id,value) VALUES('ws',$1,'unexpected',99)`, runID)
		return err
	}
	return nil
}

type teamDeliveryHosts struct {
	workerID, runID string
	pool            *pgxpool.Pool
	scenario        teamDeliveryScenario
}

func (h *teamDeliveryHosts) Build(_ context.Context, b frozen.FrozenExecutionBundle, _ workflow.RuntimeCredentialResolver) (compiler.FrozenBuildOpts, io.Closer, error) {
	if b.Agent.AgentID != h.workerID {
		return compiler.FrozenBuildOpts{LLM: &teamDeliveryModel{}, Tools: teamDeliveryNoTools{}}, io.NopCloser(strings.NewReader("")), nil
	}
	return compiler.FrozenBuildOpts{LLM: &teamDeliveryModel{worker: true}, Tools: &teamDeliveryExport{pool: h.pool, runID: h.runID, scenario: h.scenario}}, io.NopCloser(strings.NewReader("")), nil
}

type teamDeliveryModel struct{ worker bool }

func (m *teamDeliveryModel) Chat(_ context.Context, req contract.ChatRequest) (*contract.ChatResponse, error) {
	if m.worker {
		toolReturned := false
		for _, message := range req.Messages {
			if message.Role == "tool" {
				toolReturned = true
			}
		}
		if !toolReturned {
			return &contract.ChatResponse{ToolCalls: []contract.ToolCall{{ID: "export-once", Name: "compute", Args: `{}`}}, Usage: contract.Usage{InputTokens: 5, OutputTokens: 2, CostUSD: .01}}, nil
		}
	}
	return &contract.ChatResponse{Content: "PASS", Usage: contract.Usage{InputTokens: 5, OutputTokens: 2, CostUSD: .01}}, nil
}
func (*teamDeliveryModel) Stream(context.Context, contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	return nil, errors.New("unused")
}

type teamDeliveryExport struct {
	pool     *pgxpool.Pool
	runID    string
	scenario teamDeliveryScenario
}

func (*teamDeliveryExport) ListTools(context.Context) ([]contract.ToolDef, error) {
	return []contract.ToolDef{{Name: "compute", InputSchema: json.RawMessage(`{"type":"object"}`)}}, nil
}
func (e *teamDeliveryExport) Dispatch(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	if err := teamDeliveryWriteEffects(ctx, e.pool, e.runID, e.scenario.extraObject); err != nil {
		return nil, err
	}
	files := []fileartifact.File{}
	if !e.scenario.missingFile {
		files = append(files, fileartifact.File{Path: "model/result.json", ContentType: "application/json", Content: `{"value":42}`})
	}
	body, err := json.Marshal(map[string]any{"weave_member_artifacts_v1": files})
	return &contract.ToolResult{CallID: call.ID, Content: string(body)}, err
}

func (f *teamDeliveryFixture) startCLIWorker(ctx context.Context, scenario teamDeliveryScenario) <-chan error {
	done := make(chan error, 1)
	go func() {
		for count := 0; count < 2; {
			if ctx.Err() != nil {
				done <- ctx.Err()
				return
			}
			workerID := runtimes.RuntimeWorkerID("ws", f.runtime.ID)
			task, err := f.tasks.Claim(ctx, workerID, taskqueue.ClaimFilter{Kind: "engine_exec", WorkspaceID: "ws", RuntimeID: f.runtime.ID})
			if err != nil {
				done <- err
				return
			}
			if task == nil {
				select {
				case <-ctx.Done():
					done <- ctx.Err()
					return
				case <-time.After(10 * time.Millisecond):
				}
				continue
			}
			var request runtimes.EngineExecRequest
			if err := json.Unmarshal(task.Payload, &request); err != nil {
				done <- err
				return
			}
			if request.Model != "fixture-native" {
				done <- fmt.Errorf("native CLI model lost after publication: %q", request.Model)
				return
			}
			result := engine.RunResult{Status: "completed", ReportedModels: []string{request.Model}, SessionID: "session-" + task.ID, Output: "PASS", Usage: &engine.UsageReceipt{InputTokens: 7, OutputTokens: 3, CostUSD: .02, HasTokens: true, HasCost: true, Source: engine.UsageSourceCLIReported, Scope: engine.UsageScopeInvocation, EngineVersion: "fixture-cli 1"}}
			workDir, err := os.MkdirTemp("", "weave-team-delivery-cli-")
			if err != nil {
				done <- err
				return
			}
			before := runtimes.SnapshotOutputArtifacts(workDir)
			if request.NodeID == "compute" {
				if err := teamDeliveryWriteEffects(ctx, f.pool, task.RunSnapshotID, scenario.extraObject); err != nil {
					os.RemoveAll(workDir)
					done <- err
					return
				}
				if !scenario.missingFile {
					name := filepath.Join(workDir, "outputs", "model", "result.json")
					if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
						os.RemoveAll(workDir)
						done <- err
						return
					}
					if err := os.WriteFile(name, []byte(`{"value":42}`), 0o600); err != nil {
						os.RemoveAll(workDir)
						done <- err
						return
					}
				}
			}
			collectionErr := runtimes.CollectRunOutputArtifacts(workDir, before, &result)
			os.RemoveAll(workDir)
			if collectionErr != nil {
				done <- collectionErr
				return
			}
			wire, err := json.Marshal(runtimes.CLIEngineExecResult(result))
			if err != nil {
				done <- err
				return
			}
			recorder := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/runtime/tasks/"+task.ID+"/complete", bytes.NewReader(wire)).WithContext(ctx)
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			c := echo.New().NewContext(req, recorder)
			c.Set(runtimeContextKey, f.runtime)
			c.SetPath("/v1/runtime/tasks/:id/complete")
			c.SetParamNames("id")
			c.SetParamValues(task.ID)
			if err := f.server.handleRuntimeTaskComplete(c); err != nil {
				done <- err
				return
			}
			if recorder.Code != http.StatusNoContent {
				done <- fmt.Errorf("CLI complete API=%d %s", recorder.Code, recorder.Body.String())
				return
			}
			count++
		}
		done <- nil
	}()
	return done
}

func (f *teamDeliveryFixture) assertPhysicalSources(t *testing.T, ctx context.Context, runID string, report *deliverable.VerificationReport, scenario teamDeliveryScenario) {
	t.Helper()
	if len(report.Candidate.OutputSources) != 1 || len(report.Candidate.Sources) != 1 {
		t.Fatalf("selected physical source is absent or includes earlier lead: %+v", report.Candidate)
	}
	source := report.Candidate.Sources[0]
	if source.ParentRunID != runID || source.RunSnapshotID != runID {
		t.Fatalf("source escaped parent/snapshot: %+v", source)
	}
	if len(report.Candidate.SourceObservations) != 1 || report.Candidate.SourceObservations[0].Source != source {
		t.Fatalf("selected collection receipt lost in persisted report: %+v", report.Candidate.SourceObservations)
	}
	if f.engineName != "loom" {
		collection := report.Candidate.SourceObservations[0].Collection
		if collection == nil || !collection.Complete || collection.SchemaVersion != 1 {
			t.Fatalf("CLI collection receipt was not preserved: %+v", collection)
		}
	}
	for _, check := range report.Checks {
		if check.CheckID != "effects" {
			continue
		}
		var evidence struct {
			Complete   bool     `json:"complete"`
			RunID      string   `json:"run_id"`
			Unexpected []string `json:"unexpected_objects"`
		}
		if err := json.Unmarshal(check.Evidence, &evidence); err != nil {
			t.Fatal(err)
		}
		if !evidence.Complete || evidence.RunID != runID || (len(evidence.Unexpected) == 1) != scenario.extraObject {
			t.Fatalf("external observation scope or extra objects lost: %s", check.Evidence)
		}
	}
	var raw []byte
	if f.engineName == "loom" {
		if source.MemberRunID == "" || source.TaskID != "" {
			t.Fatalf("wrong Loom source: %+v", source)
		}
		if err := f.pool.QueryRow(ctx, `SELECT result FROM weave_workflow_member_runs WHERE workspace_id='ws' AND parent_run_id=$1 AND member_run_id=$2`, runID, source.MemberRunID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
	} else {
		if source.TaskID == "" || source.MemberRunID != "" {
			t.Fatalf("wrong CLI source: %+v", source)
		}
		task, err := f.tasks.Get(ctx, "ws", source.TaskID)
		if err != nil {
			t.Fatal(err)
		}
		raw = task.Result
		var original runtimes.EngineExecResult
		if err := json.Unmarshal(raw, &original); err != nil {
			t.Fatal(err)
		}
		if task.Status != taskqueue.StatusCompleted || original.Status != "completed" || original.Error != "" || original.SessionID != "session-"+source.TaskID || len(original.ReportedModels) != 1 || original.ReportedModels[0] != "fixture-native" || original.UsageReceipt == nil || original.UsageReceipt.InputTokens != 7 || original.ArtifactCollection == nil || !original.ArtifactCollection.Complete {
			t.Fatalf("CLI source facts changed: %+v", original)
		}
	}
	digest, err := deliverable.CanonicalJSONDigest(raw)
	if err != nil || digest != source.ResultDigest {
		t.Fatalf("source digest=%s wanted=%s err=%v", digest, source.ResultDigest, err)
	}
	expectedFiles := 1
	if scenario.missingFile {
		expectedFiles = 0
	}
	if len(report.Candidate.Artifacts) != expectedFiles {
		t.Fatalf("candidate file count=%d want=%d", len(report.Candidate.Artifacts), expectedFiles)
	}
	if expectedFiles == 1 {
		artifact := report.Candidate.Artifacts[0]
		if artifact.Path != "model/result.json" || len(artifact.Sources) != 1 || artifact.Sources[0] != source {
			t.Fatalf("file source mismatch: %+v", artifact)
		}
	}
	rawTerminal, _, err := storeext.New(f.pool).ReadValue(ctx, "audit:ws", runID)
	if err != nil {
		t.Fatal(err)
	}
	inspected := loomruntime.InspectTerminalRecord(true, rawTerminal)
	if inspected.Err != nil || inspected.Entry == nil {
		t.Fatalf("missing terminal usage: %v", inspected.Err)
	}
	wantTokens := 15
	if f.engineName != "loom" {
		wantTokens = 14
	}
	if inspected.Entry.SubtreeTotal.InputTokens != wantTokens {
		t.Fatalf("business verdict damaged observed usage: %+v", inspected.Entry)
	}
}

func (f *teamDeliveryFixture) assertRunActivity(t *testing.T, ctx context.Context, runID string, state deliverable.DeliveryState, scenario teamDeliveryScenario) {
	t.Helper()
	recorder := httptest.NewRecorder()
	c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/v1/runs/"+runID+"/activity", nil).WithContext(ctx), recorder)
	c.Set("tenant", "ws")
	c.Set("user_id", "user")
	c.SetParamNames("id")
	c.SetParamValues(runID)
	if err := f.server.handleGetRunActivity(c); err != nil || recorder.Code != http.StatusOK {
		t.Fatalf("activity=%d %s %v", recorder.Code, recorder.Body.String(), err)
	}
	var response struct {
		Status   string             `json:"status"`
		Delivery runDeliverySummary `json:"delivery"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Status != "succeeded" || response.Delivery.VerificationStatus != scenario.want || response.Delivery.RevisionID != state.RevisionID || response.Delivery.VerificationID != state.VerificationID || response.Delivery.ContractDigest != state.ContractDigest || response.Delivery.EvidenceCompleteness != "complete" || !teamDeliveryAPIHasReason(response.Delivery.Checks, scenario.reason) {
		t.Fatalf("API conflated execution/delivery or bound the wrong report: %s", recorder.Body.String())
	}
	detailsRecorder := httptest.NewRecorder()
	details := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/v1/runs/"+runID+"/delivery", nil).WithContext(ctx), detailsRecorder)
	details.Set("tenant", "ws")
	details.Set("user_id", "user")
	details.SetParamNames("id")
	details.SetParamValues(runID)
	if err := f.server.handleGetRunDelivery(details); err != nil || detailsRecorder.Code != http.StatusOK {
		t.Fatalf("delivery details=%d %s %v", detailsRecorder.Code, detailsRecorder.Body.String(), err)
	}
	var detailsResponse struct {
		RunID   string                         `json:"run_id"`
		Report  deliverable.VerificationReport `json:"report"`
		Binding deliverable.ContractBinding    `json:"binding"`
	}
	if err := json.Unmarshal(detailsRecorder.Body.Bytes(), &detailsResponse); err != nil {
		t.Fatal(err)
	}
	if detailsResponse.RunID != runID || detailsResponse.Report.ID != state.VerificationID || detailsResponse.Binding.InputRevisionID != state.Binding.InputRevisionID || len(detailsResponse.Report.Candidate.SourceObservations) != 1 || !teamDeliveryHasReason(detailsResponse.Report.Checks, scenario.reason) {
		t.Fatalf("detail API lost exact contract/source/check evidence: %s", detailsRecorder.Body.String())
	}

}

func publishTeamDeliveryCLI(t *testing.T, pool *pgxpool.Pool, key []byte, runtimeID string, mcpServers ...registry.MCPServerConfig) (string, string) {
	t.Helper()
	ctx := t.Context()
	agents := registry.New(pool)
	lead := &registry.AgentRecord{Name: "lead", Role: "avatar", Engine: engine.Claude, RuntimeID: runtimeID, RuntimePolicyMode: "strict_pin", Model: "fixture-native", GraphType: "standard", Spec: stdlib.AgentSpec{SystemPrompt: "Return the brief."}}
	worker := &registry.AgentRecord{Name: "worker", Role: "worker", Engine: engine.Claude, RuntimeID: runtimeID, RuntimePolicyMode: "strict_pin", Model: "fixture-native", GraphType: "standard", Spec: stdlib.AgentSpec{SystemPrompt: "Compute the result and export the physical file."}}
	worker.MCPServers = mcpServers
	for _, record := range []*registry.AgentRecord{lead, worker} {
		if len(record.MCPServers) > 0 {
			// Exercise the public configuration entry before publication: a
			// direct Registry.Put previously hid the obsolete CLI-MCP gate.
			body, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPut, "/v1/agents/"+record.Name, bytes.NewReader(body))
			request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			response := httptest.NewRecorder()
			c := echo.New().NewContext(request, response)
			c.SetParamNames("name")
			c.SetParamValues(record.Name)
			c.Set("tenant", "ws")
			c.Set("user_id", "user")
			s := &Server{Registry: agents, Runtimes: runtimes.NewStore(pool), MCPRegistry: mcpregistry.New(pool, key)}
			if err := s.handleUpdateAgent(c); err != nil || response.Code != http.StatusOK {
				t.Fatalf("configure CLI MCP: error=%v status=%d body=%s", err, response.Code, response.Body.String())
			}
			if err := json.Unmarshal(response.Body.Bytes(), record); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := agents.Put(ctx, "ws", record); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_teams(id,workspace_id,name,lead_avatar_id,status) VALUES('team','ws','CLI tools',$1,'active')`, lead.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.NewTeamWorkerRepository(pool).Create(ctx, "ws", registry.TeamWorker{TeamID: "team", WorkerAgentID: worker.ID, Duty: "Compute", AllowedKinds: []string{"consult"}, DefaultKind: "consult", ResultRequirement: "Compute and export", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	graph := json.RawMessage(fmt.Sprintf(`{"schema_version":1,"entry_node_id":"brief","input_contract":{"type":"text"},"output_contract":{"type":"text"},"nodes":[{"id":"brief","type":"lead","config":{"instruction":"Brief"},"inputs":{"task":{"value":{"source":"run_input","path":""},"expected_type":"text"}},"output":{"type":"text"}},{"id":"compute","type":"worker","config":{"kind":"consult","agent_id":%q,"agent_version":%d,"result_requirement":"Compute and export"},"inputs":{"task":{"value":{"source":"run_input","path":""},"expected_type":"text"}},"output":{"type":"text"}},{"id":"deliver","type":"deliver","config":{"result":{"source":"node_output","node_id":"compute","path":""}}}],"edges":[{"id":"a","from_node_id":"brief","to_node_id":"compute","route":"success"},{"id":"b","from_node_id":"compute","to_node_id":"deliver","route":"success"}]}`, worker.ID, worker.Version))
	flows := workflow.New(pool, nil)
	draft, err := flows.Create(ctx, &workflow.TeamWorkflow{ID: "flow", WorkspaceID: "ws", TeamID: "team", Name: "CLI delivery"}, workflow.DraftInput{CreatedBy: "user", TriggerConfig: json.RawMessage(`{"schema_version":1,"type":"conversation_explicit","config":{}}`), GraphDefinition: graph})
	if err != nil {
		t.Fatal(err)
	}
	builder := workflow.NewCandidateBuilder(flows, agents, delivery.New(pool, key), skills.New(pool), credentials.New(pool, key), schedule.New(pool, nil), memberIntegrationDescriptors(t))
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	candidate, report, err := builder.BuildTx(ctx, tx, workflow.CandidateInput{WorkspaceID: "ws", WorkflowID: "flow", WorkflowVersion: draft.Version})
	if err != nil || candidate == nil || report != nil && len(report.Issues) > 0 {
		t.Fatalf("publish CLI: error=%v report=%+v", err, report)
	}
	publication, err := workflow.PublicationFromCandidate(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if err := flows.InsertPublicationTx(ctx, tx, publication); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	published, err := flows.GetArtifact(ctx, "ws", "flow", draft.Version)
	if err != nil {
		t.Fatal(err)
	}
	return worker.ID, published.ContentHash
}

type teamDeliveryPoolStore struct{ teamDispatchPoolStore }

func (s teamDeliveryPoolStore) Get(ctx context.Context, namespace, key string) ([]byte, error) {
	value, exists, err := storeext.New(s.pool).ReadValue(ctx, namespace, key)
	if err == nil && !exists {
		return nil, pgx.ErrNoRows
	}
	return value, err
}

type teamDeliveryNoTools struct{}

func (teamDeliveryNoTools) ListTools(context.Context) ([]contract.ToolDef, error) { return nil, nil }
func (teamDeliveryNoTools) Dispatch(context.Context, contract.ToolCall) (*contract.ToolResult, error) {
	return nil, errors.New("lead has no external tools")
}

func teamDeliveryAPIHasReason(checks []runDeliveryCheck, reason string) bool {
	for _, check := range checks {
		if check.Reason == reason {
			return true
		}
	}
	return false
}
