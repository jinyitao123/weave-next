package deliverable

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/fileartifact"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/base/testutil"
)

type verificationHarness struct {
	pool  *pgxpool.Pool
	store *Store
}

func newVerificationHarness(t *testing.T, registry *VerifierRegistry) *verificationHarness {
	t.Helper()
	pool := testutil.PostgresPool(t)
	ctx := context.Background()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	_, err := pool.Exec(ctx, `
		INSERT INTO weave_workspaces(id,slug,name) VALUES ('workspace-1','verification','Verification');
		INSERT INTO weave_teams(id,workspace_id,name) VALUES ('team-1','workspace-1','Team');
		INSERT INTO weave_agents(id,workspace_id,name,role,spec) VALUES ('lead-1','workspace-1','Lead','avatar','{"role":"avatar"}'::jsonb);
		UPDATE weave_teams SET lead_avatar_id='lead-1' WHERE id='team-1';
		INSERT INTO weave_team_workflows(workspace_id,id,team_id,name) VALUES ('workspace-1','workflow-1','team-1','Workflow');
		INSERT INTO weave_team_workflow_versions(workspace_id,workflow_id,version,trigger_config,graph_definition,created_by)
		  VALUES ('workspace-1','workflow-1',1,'{"schema_version":1}'::jsonb,'{"schema_version":1}'::jsonb,'fixture');
		UPDATE weave_team_workflow_versions SET status='published',published_at=now() WHERE workspace_id='workspace-1' AND workflow_id='workflow-1';
		INSERT INTO weave_published_artifact_contents(workspace_id,workflow_id,workflow_version,artifact_schema_version,canonicalization_algorithm,canonicalization_version,hash_algorithm,content_hash,payload)
		  VALUES ('workspace-1','workflow-1',1,1,'rfc8785+jcs-preorder',1,'sha256',$1,'{"schema_version":1}'::jsonb);
	`, strings.Repeat("a", 64))
	if err != nil {
		t.Fatalf("seed verification graph: %v", err)
	}
	return &verificationHarness{pool: pool, store: NewWithVerifiers(pool, registry)}
}

func (h *verificationHarness) seedSnapshot(t *testing.T, id string) {
	t.Helper()
	_, err := snapshot.NewStore(h.pool).Create(context.Background(), snapshot.TeamRunSnapshot{
		Subject: execution.Subject{WorkspaceID: "workspace-1", UserID: "user-1"},
		RunID:   id, WorkspaceID: "workspace-1", TeamID: "team-1", SnapshotSchemaVersion: 2, Mode: "fixed_workflow",
		WorkflowID: "workflow-1", WorkflowVersion: 1, ArtifactWorkflowID: "workflow-1", ArtifactWorkflowVersion: 1,
		AdmissionDecision: json.RawMessage(`{"schema_version":1,"team_active":true,"workflow_active":true,"workers_enabled":true,"version_blocked":false,"decided_at":"2026-09-08T00:00:00Z"}`),
		RunAssociations:   json.RawMessage(`{"schema_version":1,"parent_run_id":null,"source_snapshot_id":null,"task_group_id":null}`),
		TriggerSourceV2:   json.RawMessage(`{"schema_version":1,"type":"manual","source_ref":"user-1"}`), RuntimeAssignment: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
}

func (h *verificationHarness) seedRun(t *testing.T, snapshotID, runID string) VerificationFence {
	t.Helper()
	_, err := h.pool.Exec(context.Background(), `INSERT INTO weave_team_runs(workspace_id,run_id,status,team_run_generation,execution_lease_epoch,resume_generation,team_id,workflow_id,workflow_version,run_snapshot_id,source_kind,source_task_id,establish_idempotency_key,current_executor_id,created_at,updated_at)
		VALUES ('workspace-1',$1,'running',1,1,0,'team-1','workflow-1',1,$2,'api',$3,$4,'executor-1',now(),now())`, runID, snapshotID, "parent-task-"+runID, "establish-"+runID)
	if err != nil {
		t.Fatalf("seed running execution: %v", err)
	}
	return VerificationFence{WorkspaceID: "workspace-1", RunID: runID, RunSnapshotID: snapshotID, TeamRunGeneration: 1, ExecutionLeaseEpoch: 1, ResumeGeneration: 0, ExecutorID: "executor-1"}
}

func (h *verificationHarness) freeze(t *testing.T, snapshotID string, contract *DeliveryContract) DeliveryState {
	t.Helper()
	ctx := context.Background()
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	state, err := h.store.FreezeContractTx(ctx, tx, ContractBinding{WorkspaceID: "workspace-1", RunSnapshotID: snapshotID, InputRevisionID: "input-" + snapshotID, WorkflowID: "workflow-1", WorkflowVersion: 1, PublishedDigest: strings.Repeat("a", 64), Contract: contract})
	if err != nil {
		t.Fatalf("freeze delivery contract: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return state
}

func (h *verificationHarness) source(t *testing.T, fence VerificationFence, engine, id string, files []fileartifact.File, collection *fileartifact.CollectionEvidence) SourceObservation {
	t.Helper()
	ctx := context.Background()
	var raw []byte
	var err error
	source := ArtifactSource{RunSnapshotID: fence.RunSnapshotID, ParentRunID: fence.RunID}
	if engine == "cli" {
		source.TaskID = id
		raw, err = json.Marshal(map[string]any{"artifacts": files, "artifact_collection": collection, "answer": "PASS"})
		if err != nil {
			t.Fatal(err)
		}
		_, err = h.pool.Exec(ctx, `INSERT INTO weave_task_queue(id,workspace_id,kind,status,identity_kind,identity_schema_version,workflow_id,workflow_version,run_snapshot_id,result,payload,actor_subject)
			VALUES ($1,$2,'engine_exec','completed','team_workflow',2,'workflow-1',1,$3,$4::jsonb,'{}'::jsonb,jsonb_build_object('workspace_id',$2::text,'user_id','user-1'))`, id, fence.WorkspaceID, fence.RunSnapshotID, string(raw))
	} else {
		source.MemberRunID = id
		raw, err = json.Marshal(map[string]any{"result": map[string]any{"RunID": id, "StopReason": "completed", "State": map[string]any{fileartifact.MemberStateKey: files}}, "error": ""})
		if err != nil {
			t.Fatal(err)
		}
		_, err = h.pool.Exec(ctx, `INSERT INTO weave_workflow_member_runs(workspace_id,parent_run_id,member_run_id,call_id,run_snapshot_id,node_id,parent_generation,identity_hash,initial_state,result)
			VALUES ($1,$2,$3,$3,$4,'worker',1,$5,'{}'::jsonb,$6::jsonb)`, fence.WorkspaceID, fence.RunID, id, fence.RunSnapshotID, strings.Repeat("b", 64), string(raw))
	}
	if err != nil {
		t.Fatalf("persist %s physical source: %v", engine, err)
	}
	source.ResultDigest, err = CanonicalJSONDigest(raw)
	if err != nil {
		t.Fatal(err)
	}
	return SourceObservation{Source: source, Collection: collection}
}

func bundleFor(fence VerificationFence, source SourceObservation, files []fileartifact.File) []WorkflowOutput {
	base := WorkflowOutput{WorkspaceID: fence.WorkspaceID, RunID: fence.RunID, RunSnapshotID: fence.RunSnapshotID, NodeID: "deliver", NodeType: "deliver", Final: true}
	outputs := make([]WorkflowOutput, 0, len(files)+1)
	for _, file := range files {
		item := base
		item.Artifact = &WorkflowArtifact{Path: file.Path, ContentType: file.ContentType, Content: file.Content, Sources: []ArtifactSource{source.Source}}
		outputs = append(outputs, item)
	}
	base.Output = "PASS"
	base.Sources = []ArtifactSource{source.Source}
	base.SourceObservations = []SourceObservation{source}
	return append(outputs, base)
}

func (h *verificationHarness) counts(t *testing.T, runID string) (int, int) {
	t.Helper()
	var files, reports int
	if err := h.pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM weave_final_deliverables WHERE run_id=$1),(SELECT count(*) FROM weave_run_delivery_verifications WHERE run_id=$1)`, runID).Scan(&files, &reports); err != nil {
		t.Fatal(err)
	}
	return files, reports
}

func TestVerificationStorePhysicalSourcesAndVersions(t *testing.T) {
	h := newVerificationHarness(t, fixtureEffectsRegistry(t, VerificationPassed))
	for _, engine := range []string{"cli", "loom"} {
		t.Run(engine, func(t *testing.T) {
			snapshotID, runID := "snapshot-"+engine, "physical-"+engine
			h.seedSnapshot(t, snapshotID)
			// The frozen binding must exist before a differently named TeamRun does.
			state := h.freeze(t, snapshotID, fixtureContract())
			if state.Binding.RunID != "" {
				t.Fatal("dispatch guessed an execution run")
			}
			fence := h.seedRun(t, snapshotID, runID)
			files := []fileartifact.File{{Path: "report.md", ContentType: "text/markdown", Content: "verified content"}}
			source := h.source(t, fence, engine, engine+"-source", files, nil)
			outputs := bundleFor(fence, source, files)
			report, err := h.store.RecordVerifiedWorkflowOutputs(context.Background(), outputs, fence)
			if err != nil {
				t.Fatal(err)
			}
			if report.Status != VerificationPassed {
				t.Fatalf("got %+v", report)
			}
			replay, err := h.store.RecordVerifiedWorkflowOutputs(context.Background(), outputs, fence)
			if err != nil {
				t.Fatal(err)
			}
			if replay.ID != report.ID || replay.RevisionID != report.RevisionID {
				t.Fatal("replay duplicated verification")
			}
			state, err = h.store.GetDeliveryState(context.Background(), "workspace-1", runID)
			if err != nil {
				t.Fatal(err)
			}
			if state.SelectionSequence != 1 || state.Binding.RunID != runID || state.Report == nil || state.Report.ID != report.ID {
				t.Fatalf("bad current state %+v", state)
			}
			if fileCount, reportCount := h.counts(t, runID); fileCount != 2 || reportCount != 1 {
				t.Fatalf("counts=%d,%d", fileCount, reportCount)
			}
			files[0].Content += " revised"
			newSource := h.source(t, fence, engine, engine+"-source-v2", files, nil)
			changed, err := h.store.RecordVerifiedWorkflowOutputs(context.Background(), bundleFor(fence, newSource, files), fence)
			if err != nil {
				t.Fatal(err)
			}
			if changed.RevisionID == report.RevisionID {
				t.Fatal("new physical result reused old revision")
			}
			state, err = h.store.GetDeliveryState(context.Background(), "workspace-1", runID)
			if err != nil {
				t.Fatal(err)
			}
			if state.SelectionSequence != 2 || state.RevisionID != changed.RevisionID {
				t.Fatal("current revision did not advance")
			}
			if _, err := h.pool.Exec(context.Background(), `UPDATE weave_run_delivery_verifications SET status='failed' WHERE verification_id=$1`, report.ID); err == nil {
				t.Fatal("immutable report changed")
			}
			if _, err := h.pool.Exec(context.Background(), `UPDATE weave_run_delivery_state SET contract='null'::jsonb WHERE run_snapshot_id=$1`, snapshotID); err == nil {
				t.Fatal("frozen contract changed")
			}
		})
	}
}

func TestVerificationStoreRollsBackFilesAndReport(t *testing.T) {
	h := newVerificationHarness(t, fixtureEffectsRegistry(t, VerificationPassed))
	h.seedSnapshot(t, "snapshot")
	h.freeze(t, "snapshot", fixtureContract())
	fence := h.seedRun(t, "snapshot", "run")
	files := []fileartifact.File{{Path: "report.md", ContentType: "text/markdown", Content: "verified content"}}
	source := h.source(t, fence, "loom", "source", files, nil)
	_, err := h.pool.Exec(context.Background(), `CREATE FUNCTION reject_verification_fixture() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected report failure'; END; $$;
		CREATE TRIGGER reject_verification_fixture BEFORE INSERT ON weave_run_delivery_verifications FOR EACH ROW EXECUTE FUNCTION reject_verification_fixture();`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.RecordVerifiedWorkflowOutputs(context.Background(), bundleFor(fence, source, files), fence); err == nil {
		t.Fatal("injected report failure succeeded")
	}
	if fileCount, reportCount := h.counts(t, "run"); fileCount != 0 || reportCount != 0 {
		t.Fatalf("partial publication: %d files, %d reports", fileCount, reportCount)
	}
	state, err := h.store.GetDeliveryState(context.Background(), "workspace-1", "run")
	if err != nil {
		t.Fatal(err)
	}
	if state.VerificationID != "" || state.SelectionSequence != 0 {
		t.Fatal("failed transaction changed pointer")
	}
}

func TestVerificationStoreRejectsStaleAndForgedEvidence(t *testing.T) {
	h := newVerificationHarness(t, fixtureEffectsRegistry(t, VerificationPassed))
	h.seedSnapshot(t, "snapshot")
	h.freeze(t, "snapshot", fixtureContract())
	fence := h.seedRun(t, "snapshot", "run")
	files := []fileartifact.File{{Path: "report.md", ContentType: "text/markdown", Content: "verified content"}}
	source := h.source(t, fence, "cli", "source", files, nil)
	for _, mode := range []string{"lease", "generation", "resume", "executor", "file bytes", "result digest", "collection receipt"} {
		t.Run(mode, func(t *testing.T) {
			current := fence
			receipt := source
			actualFiles := append([]fileartifact.File(nil), files...)
			switch mode {
			case "lease":
				current.ExecutionLeaseEpoch++
			case "generation":
				current.TeamRunGeneration++
			case "resume":
				current.ResumeGeneration++
			case "executor":
				current.ExecutorID = "other"
			case "file bytes":
				actualFiles[0].Content += " forged"
			case "result digest":
				receipt.Source.ResultDigest = strings.Repeat("c", 64)
			case "collection receipt":
				receipt.Collection = &fileartifact.CollectionEvidence{SchemaVersion: 1, Complete: true, Limits: fileartifact.CollectionLimits{MaxFiles: 128, MaxFileBytes: 256 * 1024, MaxTotalBytes: 512 * 1024}}
			}
			if _, err := h.store.RecordVerifiedWorkflowOutputs(context.Background(), bundleFor(fence, receipt, actualFiles), current); err == nil {
				t.Fatal("stale or forged evidence accepted")
			}
			if fileCount, reportCount := h.counts(t, "run"); fileCount != 0 || reportCount != 0 {
				t.Fatal("rejected evidence published results")
			}
		})
	}
}

func TestVerificationStoreCancellationAndConcurrentRevision(t *testing.T) {
	for _, mode := range []string{"cancel", "new revision"} {
		t.Run(mode, func(t *testing.T) {
			registry := NewVerifierRegistry()
			started, release := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			_ = registry.RegisterExternalEffects("fixture.effects", "v1", func(ctx context.Context, _ VerificationInput) (CheckResult, error) {
				if calls.Add(1) == 1 {
					close(started)
					select {
					case <-release:
					case <-ctx.Done():
						return CheckResult{}, ctx.Err()
					}
				}
				return CheckResult{Status: VerificationPassed, Reason: "isolated", Evidence: json.RawMessage(`{"complete":true,"objects":[]}`)}, nil
			})
			h := newVerificationHarness(t, registry)
			h.seedSnapshot(t, "snapshot")
			h.freeze(t, "snapshot", fixtureContract())
			fence := h.seedRun(t, "snapshot", "run")
			files := []fileartifact.File{{Path: "report.md", ContentType: "text/markdown", Content: "verified content"}}
			source := h.source(t, fence, "loom", "source", files, nil)
			done := make(chan error, 1)
			go func() {
				_, err := h.store.RecordVerifiedWorkflowOutputs(context.Background(), bundleFor(fence, source, files), fence)
				done <- err
			}()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("verifier did not begin")
			}
			if mode == "cancel" {
				_, err := h.pool.Exec(context.Background(), `UPDATE weave_team_runs SET status='cancelled',current_executor_id=NULL,error_code='team_run_cancelled',terminal_at=now(),updated_at=now(),team_run_generation=2 WHERE run_id='run'`)
				if err != nil {
					close(release)
					t.Fatal(err)
				}
			} else {
				revisedFiles := append([]fileartifact.File(nil), files...)
				revisedFiles[0].Content += " new"
				newSource := h.source(t, fence, "loom", "new-source", revisedFiles, nil)
				if _, err := h.store.RecordVerifiedWorkflowOutputs(context.Background(), bundleFor(fence, newSource, revisedFiles), fence); err != nil {
					close(release)
					t.Fatal(err)
				}
			}
			close(release)
			err := <-done
			want := ErrVerificationFence
			if mode == "new revision" {
				want = ErrVerificationConflict
			}
			if !errors.Is(err, want) {
				t.Fatalf("late result err=%v, want %v", err, want)
			}
			fileCount, reportCount := h.counts(t, "run")
			if mode == "cancel" && (fileCount != 0 || reportCount != 0) {
				t.Fatal("cancelled execution published results")
			}
			if mode == "new revision" && (fileCount != 2 || reportCount != 1) {
				t.Fatal("late old version overwrote current result")
			}
		})
	}
}

func TestVerificationStoreExternalStateAndLegacyUnknown(t *testing.T) {
	var writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			writes.Add(1)
			http.Error(w, "read only", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"benchmark_score":7,"benchmark_total":7,"complete":true,"events":[{"id":"required"},{"id":"unexpected-calendar-event"}]}`))
	}))
	defer server.Close()
	registry := NewVerifierRegistry()
	_ = registry.RegisterExternalEffects("fixture.effects", "v1", func(ctx context.Context, _ VerificationInput) (CheckResult, error) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			return CheckResult{}, err
		}
		defer response.Body.Close()
		var state struct {
			Score    int  `json:"benchmark_score"`
			Total    int  `json:"benchmark_total"`
			Complete bool `json:"complete"`
			Events   []struct {
				ID string `json:"id"`
			} `json:"events"`
		}
		if err := json.NewDecoder(response.Body).Decode(&state); err != nil {
			return CheckResult{}, err
		}
		evidence, _ := json.Marshal(state)
		status := VerificationPassed
		for _, event := range state.Events {
			if event.ID != "required" {
				status = VerificationFailed
			}
		}
		return CheckResult{Status: status, Reason: "external_inventory_observed", Evidence: evidence}, nil
	})
	h := newVerificationHarness(t, registry)
	for _, mode := range []string{"explicit", "legacy"} {
		h.seedSnapshot(t, mode)
		if mode == "explicit" {
			h.freeze(t, mode, fixtureContract())
		}
		fence := h.seedRun(t, mode, "run-"+mode)
		files := []fileartifact.File{{Path: "report.md", ContentType: "text/markdown", Content: "verified content"}}
		source := h.source(t, fence, "loom", "source-"+mode, files, nil)
		report, err := h.store.RecordVerifiedWorkflowOutputs(context.Background(), bundleFor(fence, source, files), fence)
		if err != nil {
			t.Fatal(err)
		}
		want := VerificationFailed
		if mode == "legacy" {
			want = VerificationUnknown
		}
		if report.Status != want {
			t.Fatalf("%s got %s", mode, report.Status)
		}
		if mode == "explicit" {
			encoded, _ := json.Marshal(report)
			if !strings.Contains(string(encoded), `"benchmark_score":7`) || !strings.Contains(string(encoded), "unexpected-calendar-event") {
				t.Fatal("original full benchmark score or extra-write evidence lost")
			}
		}
	}
	if writes.Load() != 0 {
		t.Fatal("verifier attempted a write")
	}
}

func TestVerificationStoreDispatchInputFactsImmutable(t *testing.T) {
	h := newVerificationHarness(t, nil)
	ctx := context.Background()
	_, err := h.pool.Exec(ctx, `INSERT INTO weave_dispatch_input_revisions(workspace_id,user_id,workbench_session_id,input_revision_id,registration_id,registration_sha256,source_messages,task,task_sha256,team_id,mode,workflow_id,workflow_version,client_request_id,execution_task,revision_kind,root_input_revision_id)
		VALUES ('workspace-1','user','session','input','registration',$1,'[{"id":"message"}]'::jsonb,'task',$1,'team-1','workflow','workflow-1',1,'request','task','initial','input')`, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	for _, set := range []string{`task='changed'`, `delivery_contract='{"version":1}'::jsonb`} {
		if _, err := h.pool.Exec(ctx, `UPDATE weave_dispatch_input_revisions SET `+set+` WHERE input_revision_id='input'`); err == nil {
			t.Fatalf("immutable input change accepted: %s", set)
		}
	}
	_, err = h.pool.Exec(ctx, `UPDATE weave_dispatch_input_revisions SET is_current=false,consumed_run_id='run',consumed_task_id='task',consumed_at=now() WHERE input_revision_id='input'`)
	if err != nil {
		t.Fatal(err)
	}
	for _, set := range []string{`is_current=true`, `consumed_run_id='other'`} {
		if _, err := h.pool.Exec(ctx, `UPDATE weave_dispatch_input_revisions SET `+set+` WHERE input_revision_id='input'`); err == nil {
			t.Fatal(fmt.Sprintf("input lifecycle reversal accepted: %s", set))
		}
	}
}
