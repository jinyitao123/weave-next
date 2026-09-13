package deliveryverify

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/deliverycheck"
)

func TestDeterministicDeliveryPersistsFailureAndRechecksFrozenRequirements(t *testing.T) {
	ctx := context.Background()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	_, err := pool.Exec(ctx, `
 INSERT INTO weave_workspaces(id,slug,name) VALUES ('ws','deterministic','Verification');
 INSERT INTO weave_teams(id,workspace_id,name) VALUES ('team','ws','Team');
 INSERT INTO weave_agents(id,workspace_id,name,role,spec) VALUES ('lead','ws','Lead','avatar','{"role":"avatar"}');
 UPDATE weave_teams SET lead_avatar_id='lead' WHERE id='team';
 INSERT INTO weave_team_workflows(workspace_id,id,team_id,name) VALUES ('ws','workflow','team','Workflow');
 INSERT INTO weave_team_workflow_versions(workspace_id,workflow_id,version,trigger_config,graph_definition,created_by)
 VALUES ('ws','workflow',1,'{"schema_version":1}','{"schema_version":1}','fixture');
 UPDATE weave_team_workflow_versions SET status='published',published_at=now() WHERE workspace_id='ws';
 INSERT INTO weave_published_artifact_contents(workspace_id,workflow_id,workflow_version,artifact_schema_version,canonicalization_algorithm,canonicalization_version,hash_algorithm,content_hash,payload)
 VALUES ('ws','workflow',1,1,'rfc8785+jcs-preorder',1,'sha256',$1,'{"schema_version":1}');`, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	_, err = snapshot.NewStore(pool).Create(ctx, snapshot.TeamRunSnapshot{
		RunID: "snapshot", WorkspaceID: "ws", TeamID: "team", SnapshotSchemaVersion: 2, Mode: "fixed_workflow", WorkflowID: "workflow", WorkflowVersion: 1, ArtifactWorkflowID: "workflow", ArtifactWorkflowVersion: 1,
		AdmissionDecision: json.RawMessage(`{"schema_version":1,"team_active":true,"workflow_active":true,"workers_enabled":true,"version_blocked":false,"decided_at":"2026-09-13T00:00:00Z"}`),
		RunAssociations:   json.RawMessage(`{"schema_version":1,"parent_run_id":null,"source_snapshot_id":null,"task_group_id":null}`),
		TriggerSourceV2:   json.RawMessage(`{"schema_version":1,"type":"manual","source_ref":"user"}`), RuntimeAssignment: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `
 INSERT INTO weave_team_runs(workspace_id,run_id,status,team_run_generation,execution_lease_epoch,resume_generation,team_id,workflow_id,workflow_version,run_snapshot_id,source_kind,source_task_id,establish_idempotency_key,current_executor_id,created_at,updated_at)
 VALUES ('ws','run','running',1,1,0,'team','workflow',1,'snapshot','api','original','establish','executor',now(),now());
 INSERT INTO weave_task_queue(id,workspace_id,kind,status,identity_kind,identity_schema_version,workflow_id,workflow_version,run_snapshot_id,payload)
 VALUES ('original','ws','team_run','completed','team_workflow',2,'workflow',1,'snapshot','{"rows":[{"amount":0.1},{"amount":0.2}]}');
 INSERT INTO weave_task_queue(id,workspace_id,kind,status,identity_kind,identity_schema_version,workflow_id,workflow_version,run_snapshot_id,payload)
 VALUES ('resume','ws','team_run','queued','team_workflow',2,'workflow',1,'snapshot','{"rows":[{"amount":999}]}');
 `)
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(pool)
	contract := &deliverable.DeliveryContract{Version: 1, Coverage: deliverable.CoverageExplicit, Output: deliverable.OutputRequirement{Type: "json"}, ExternalEffects: deliverable.ExternalEffectsNone,
		RequiredChecks: []deliverable.CheckSpec{{ID: "total", Title: "总额应等于原始明细合计", VerifierID: deliverycheck.ID, VerifierVersion: deliverycheck.Version, Parameters: json.RawMessage(`{"actual":{"source":"output","path":"/total"},"operator":"equals","expected":{"source":"input","path":"/rows","reduce":"sum","field":"/amount"}}`)}}}
	if err := deliverycheck.ValidateContract(contract); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	binding := deliverable.ContractBinding{WorkspaceID: "ws", RunSnapshotID: "snapshot", WorkflowID: "workflow", WorkflowVersion: 1, PublishedDigest: strings.Repeat("a", 64), Contract: contract}
	state, err := store.FreezeContractTx(ctx, tx, binding)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// Changing the caller's proposed contract after freeze cannot change checks.
	contract.RequiredChecks[0].Parameters = json.RawMessage(`{"actual":{"source":"output","path":"/review"},"operator":"equals","expected":{"source":"literal","value":"PASS"}}`)
	fence := deliverable.VerificationFence{WorkspaceID: "ws", RunID: "run", RunSnapshotID: "snapshot", TeamRunGeneration: 1, ExecutionLeaseEpoch: 1, ExecutorID: "executor"}
	deliver := func(taskID, total string) deliverable.VerificationReport {
		t.Helper()
		raw := json.RawMessage(`{"answer":{"total":` + total + `,"review":"PASS"}}`)
		_, err := pool.Exec(ctx, `INSERT INTO weave_task_queue(id,workspace_id,kind,status,identity_kind,identity_schema_version,workflow_id,workflow_version,run_snapshot_id,payload,result)
 VALUES ($1,'ws','engine_exec','completed','team_workflow',2,'workflow',1,'snapshot','{}',$2)`, taskID, raw)
		if err != nil {
			t.Fatal(err)
		}
		digest, err := deliverable.CanonicalJSONDigest(raw)
		if err != nil {
			t.Fatal(err)
		}
		source := deliverable.ArtifactSource{TaskID: taskID, RunSnapshotID: "snapshot", ParentRunID: "run", ResultDigest: digest}
		output := deliverable.WorkflowOutput{WorkspaceID: "ws", RunID: "run", RunSnapshotID: "snapshot", NodeID: "deliver", NodeType: "deliver", Final: true, Output: json.RawMessage(`{"total":` + total + `,"review":"PASS"}`), Sources: []deliverable.ArtifactSource{source}, SourceObservations: []deliverable.SourceObservation{{Source: source}}}
		report, err := store.RecordVerifiedWorkflowOutputs(ctx, []deliverable.WorkflowOutput{output}, fence)
		if err != nil {
			t.Fatal(err)
		}
		return report
	}
	failed := deliver("wrong", "0.4")
	if failed.Status != deliverable.VerificationFailed {
		t.Fatalf("false success: %+v", failed)
	}
	check := failed.Checks[len(failed.Checks)-1]
	if check.CheckID != "total" || check.Title != "总额应等于原始明细合计" || check.Reason != "deterministic_mismatch" || !strings.Contains(string(check.Evidence), `"input_digest"`) {
		t.Fatalf("failure evidence lost: %+v", check)
	}
	rechecked, err := store.RecheckCurrentDelivery(ctx, deliverable.RecheckRequest{WorkspaceID: "ws", RunID: "run", RevisionID: failed.RevisionID, ContractDigest: state.ContractDigest})
	if err != nil || !rechecked.Current || rechecked.Report.Status != deliverable.VerificationFailed || rechecked.Report.ID == failed.ID {
		t.Fatalf("recheck: %+v %v", rechecked, err)
	}
	correct := deliver("correct", "0.3")
	if correct.Status != deliverable.VerificationPassed {
		t.Fatalf("corrected: %+v", correct)
	}
	historical, err := store.GetVerificationReport(ctx, "ws", "run", failed.ID)
	if err != nil || historical.Status != deliverable.VerificationFailed {
		t.Fatalf("history lost: %+v %v", historical, err)
	}
	if _, err := store.GetVerificationReport(ctx, "other", "run", failed.ID); !errors.Is(err, deliverable.ErrNotFound) {
		t.Fatalf("workspace isolation: %v", err)
	}
	if _, err := frozenInputReader(pool)(ctx, deliverable.Candidate{WorkspaceID: "ws", RunID: "run", RunSnapshotID: "other"}); err == nil {
		t.Fatal("input crossed snapshot boundary")
	}
	// No task execution, approval or status change is caused by verification.
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM weave_team_runs WHERE run_id='run'`).Scan(&status); err != nil || status != "running" {
		t.Fatalf("verification changed execution: %s %v", status, err)
	}
}
