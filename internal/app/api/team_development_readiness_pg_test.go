package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/app/deliveryverify"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/businessaction"
	"github.com/jinyitao123/weave/internal/kernel/publication"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

const publishedSecondBusinessCapability = "forge:action:sales_quote.AdjustDiscount"

func TestDevelopmentPublicationReadinessUnionsCurrentCandidateSimulationReceiptsRealPG(t *testing.T) {
	t.Setenv("WEAVE_SECRET_KEY_FILE", "")
	t.Setenv("WEAVE_SECRET_KEY", strings.Repeat("31", 32))
	ctx := execution.WithSubject(t.Context(), execution.Subject{WorkspaceID: "ws", UserID: "user"})
	seed := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, seed); err != nil {
		t.Fatal(err)
	}
	cfg := seed.Config()
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES('ws','ws','ws');
		INSERT INTO weave_users(id,tenant_id,username,password,role) VALUES('user','ws','user','unused','admin');
		INSERT INTO weave_external_identities(issuer,subject,workspace_id,user_id,native_organization) VALUES('forge:task-delegation-test','native-user','ws','user','native-org')`); err != nil {
		t.Fatal(err)
	}
	key := []byte(strings.Repeat("1", 32))
	_, _, envelope, authority := publishBusinessCompletionSample(t, pool, key, "http://127.0.0.1:1", publishedSecondBusinessCapability)
	kernelPublication := openAPIKernelPublication(t, ctx, pool, authority)
	payload, err := frozen.DecodeArtifactEnvelopeV1(envelope)
	if err != nil {
		t.Fatal(err)
	}
	graph, report := machine.DecodeGraphDefinitionV1(payload.GraphDefinition)
	if report != nil && len(report.Issues) > 0 {
		t.Fatalf("published graph is invalid: %+v", report.Issues)
	}
	capabilities := machine.GraphBusinessCapabilities(graph, payload)
	if len(capabilities) != 2 {
		t.Fatalf("candidate required capabilities=%v, want 2", capabilities)
	}
	allDefinitions := developmentTestActionDefinitions(t, true, false)

	first := admitDevelopmentCoverageTrial(t, ctx, pool, kernelPublication, envelope, allDefinitions, "first simulated action")
	writeSimulationOutcome(t, pool, first, publishedBusinessCapability)
	readiness, err := buildDevelopmentPublicationReadiness(ctx, pool, "ws", "team", "user", "user", 1, 1, []developmentPrepared{{ID: "flow", Envelope: envelope}})
	if err != nil || readiness.Ready || len(readiness.Workflows) != 1 || readiness.Workflows[0].Passed ||
		len(readiness.Workflows[0].CoveredCapabilityIDs) != 1 || len(readiness.Workflows[0].MissingCapabilityIDs) != 1 {
		t.Fatalf("partial action simulation unlocked publication: readiness=%+v err=%v", readiness, err)
	}
	frame, err := deliveryverify.BusinessReceiptReader(pool)(ctx, deliverable.Candidate{WorkspaceID: "ws", RunID: first.runID, RunSnapshotID: first.snapshotID})
	if err != nil || !frame.Scope.DevelopmentTrial || len(frame.Scope.AllowedCapabilityIDs) != 1 || len(frame.Receipts) != 1 || !frame.Receipts[0].Simulated {
		t.Fatalf("first trial did not preserve its own simulation scope: frame=%+v err=%v", frame, err)
	}

	secondDefinitions := developmentTestActionDefinitions(t, false, true)
	second := admitDevelopmentCoverageTrial(t, ctx, pool, kernelPublication, envelope, secondDefinitions, "second simulated action")
	writeSimulationOutcome(t, pool, second, publishedSecondBusinessCapability)
	readiness, err = buildDevelopmentPublicationReadiness(ctx, pool, "ws", "team", "user", "user", 1, 1, []developmentPrepared{{ID: "flow", Envelope: envelope}})
	if err != nil || !readiness.Ready || len(readiness.Workflows) != 1 || !readiness.Workflows[0].Passed ||
		len(readiness.Workflows[0].CoveredCapabilityIDs) != 2 || len(readiness.Workflows[0].MissingCapabilityIDs) != 0 {
		t.Fatalf("same-candidate action union did not unlock publication: readiness=%+v err=%v", readiness, err)
	}
}

type developmentCoverageTrial struct {
	runID, snapshotID, requestID, inputVersion string
}

func developmentTestActionDefinitions(t *testing.T, priceAuthorized, discountAuthorized bool) []businessaction.DevelopmentAction {
	t.Helper()
	priceFlag := "false"
	if priceAuthorized {
		priceFlag = "true"
	}
	discountFlag := "false"
	if discountAuthorized {
		discountFlag = "true"
	}
	raw := `[
		{"capability_id":"forge:action:sales_quote.AdjustPrice","name":"AdjustPrice","object_name":"sales_quote","label":"Adjust price","requires_record":true,"params":[{"name":"line_id","type":"string","required":true},{"name":"idempotency_key","type":"string","required":true}],"simulation_authorized":` + priceFlag + `},
		{"capability_id":"forge:action:sales_quote.AdjustDiscount","name":"AdjustDiscount","object_name":"sales_quote","label":"Adjust discount","requires_record":true,"params":[{"name":"line_id","type":"string","required":true},{"name":"idempotency_key","type":"string","required":true}],"simulation_authorized":` + discountFlag + `}
	]`
	var actions []businessaction.DevelopmentAction
	if err := json.Unmarshal([]byte(raw), &actions); err != nil {
		t.Fatal(err)
	}
	return actions
}

func admitDevelopmentCoverageTrial(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	publicationService interface {
		AdmitCandidate(context.Context, publication.CandidateRunRequest) (publication.AdmissionReceipt, error)
	},
	envelope frozen.ArtifactEnvelopeV1,
	actions []businessaction.DevelopmentAction,
	input string,
) developmentCoverageTrial {
	t.Helper()
	requestID := uuid.NewString()
	inputJSON, _ := json.Marshal(input)
	inputHash := sha256.Sum256(inputJSON)
	request := publication.CandidateRunRequest{
		Version: publication.ContractVersion, RequestID: "development:" + requestID, Candidate: envelope,
		Input: inputJSON, InputVersion: hex.EncodeToString(inputHash[:]), SourceRef: "team-development:team", Purpose: "developer-trial",
	}
	payload, err := frozen.DecodeArtifactEnvelopeV1(envelope)
	if err != nil {
		t.Fatal(err)
	}
	graph, report := machine.DecodeGraphDefinitionV1(payload.GraphDefinition)
	if report != nil && len(report.Issues) > 0 {
		t.Fatalf("candidate graph is invalid: %+v", report.Issues)
	}
	normalized, err := businessaction.ValidateDevelopmentActions(machine.GraphBusinessCapabilities(graph, payload), actions)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := businessaction.DevelopmentTrialRequestDigest(ctx, request, normalized)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_team_development_trials(workspace_id,team_id,request_id,revision,workflow_id,actor_id,request_digest,request,business_actions)
		VALUES('ws','team',$1,1,$2,'user',$3,$4,$5)`, requestID, envelope.WorkflowID, digest, encodeDevelopment(request), encodeDevelopment(normalized)); err != nil {
		t.Fatal(err)
	}
	admission, err := publicationService.AdmitCandidate(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := admission.Verify(ctx, request); err != nil {
		t.Fatal(err)
	}
	runs := teamrun.NewPGStore()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runs.EstablishQueuedTx(ctx, tx, teamrun.EstablishRequest{
		WorkspaceID: "ws", RunID: admission.RunID, TeamID: "team", WorkflowID: envelope.WorkflowID,
		WorkflowVersion: envelope.WorkflowVersion, RunSnapshotID: admission.RunSnapshotID,
		SourceKind: teamrun.SourceAPI, SourceTaskID: admission.TaskID,
		EstablishIdempotencyKey: "teamrun-establish:" + admission.TaskID,
		Actor:                   "coverage-fixture", Source: "coverage-fixture", OccurredAt: time.Now().UTC(),
	}); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	executorID := "coverage-fixture-worker"
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = runs.ClaimRunningTx(ctx, tx, teamrun.ClaimRequest{
		WorkspaceID: "ws", RunID: admission.RunID, ExpectedStatus: teamrun.StatusQueued,
		ExpectedTeamRunGeneration: 0, ExpectedExecutionLeaseEpoch: 0, ExpectedResumeGeneration: 0,
		ExecutorID: executorID, IdempotencyKey: "coverage-claim:" + admission.RunID,
		Actor: executorID, Source: "coverage-fixture", OccurredAt: time.Now().UTC(),
	})
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE weave_team_development_trials SET receipt=$2 WHERE workspace_id='ws' AND request_id=$1::uuid AND team_id='team'`, requestID, encodeDevelopment(admission)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE weave_team_runs SET status='running' WHERE workspace_id='ws' AND run_id=$1 AND run_snapshot_id=$2`, admission.RunID, admission.RunSnapshotID); err != nil {
		t.Fatal(err)
	}
	return developmentCoverageTrial{runID: admission.RunID, snapshotID: admission.RunSnapshotID, requestID: requestID, inputVersion: request.InputVersion}
}

func writeSimulationOutcome(t *testing.T, pool *pgxpool.Pool, trial developmentCoverageTrial, capabilityID string) {
	t.Helper()
	parts := strings.Split(strings.TrimPrefix(capabilityID, "forge:action:"), ".")
	if len(parts) != 2 {
		t.Fatalf("invalid test capability %q", capabilityID)
	}
	objectName, actionName := parts[0], parts[1]
	invocationID, slot, callID := "snapshot/0/lead", "simulation/"+actionName, "simulation-"+actionName
	operationID := execution.EngineOperationID(trial.inputVersion, invocationID, slot, capabilityID)
	paramsHash := strings.Repeat("a", 64)
	occurred := time.Now().UTC()
	store := &teamrun.PGActivityStore{Transactions: pool}
	for _, phase := range []string{"started", "result"} {
		outcome := businessaction.ActionOutcomeEvent{
			Source: businessaction.ActionOutcomeSourceDevelopmentSimulation, RunSnapshotID: trial.snapshotID, ActorID: "user",
			OperationID: operationID, OperationSlot: slot, Phase: phase, InvocationID: invocationID,
			CallID: callID, CapabilityID: capabilityID, ActionKey: objectName + "." + actionName,
			ActionName: actionName, ActionLabel: actionName, ObjectName: objectName,
			InputRevisionID: trial.inputVersion, RecordID: "synthetic-record", ParamsSHA256: paramsHash,
		}
		if phase == "result" {
			outcome.Status = businessaction.ActionOutcomeStatusSucceeded
			outcome.Result = &contract.ToolResult{CallID: callID, ToolName: "run_action", Content: `{"ok":true,"simulated":true}`}
		}
		detail, err := json.Marshal(outcome)
		if err != nil {
			t.Fatal(err)
		}
		kind := "business_action_started"
		if phase == "result" {
			kind = "business_action_result"
		}
		if err := store.Record(t.Context(), teamrun.ActivityEvent{
			WorkspaceID: "ws", RunID: trial.runID, EventID: uuid.NewString(), Kind: kind,
			NodeID: "lead", MemberID: "lead-agent", MemberVersion: 1, Detail: detail, OccurredAt: occurred,
		}); err != nil {
			t.Fatal(err)
		}
		occurred = occurred.Add(time.Millisecond)
	}
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := teamrun.NewPGStore().SucceedTx(t.Context(), tx, teamrun.SucceedRequest{
		WorkspaceID: "ws", RunID: trial.runID, ExpectedStatus: teamrun.StatusRunning,
		ExpectedTeamRunGeneration: 1, ExpectedExecutionLeaseEpoch: 1, ExpectedResumeGeneration: 0,
		ExecutorID: "coverage-fixture-worker", IdempotencyKey: "coverage-succeed:" + trial.runID,
		Actor: "coverage-fixture-worker", Source: "coverage-fixture", OccurredAt: time.Now().UTC(),
	}); err != nil {
		_ = tx.Rollback(t.Context())
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}
