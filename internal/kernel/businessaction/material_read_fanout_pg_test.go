package businessaction_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/businessaction"
	"github.com/jinyitao123/weave/internal/kernel/fanout"
	"github.com/jinyitao123/weave/internal/kernel/secret"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
)

func TestMaterialReadAllowsOnlyCurrentParkedFanoutLegOnPostgres(t *testing.T) {
	ctx := t.Context()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate isolated test schema: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	workspaceID, teamID, workflowID := "workspace-"+uuid.NewString(), "team-"+uuid.NewString(), "workflow-"+uuid.NewString()
	parentRunID, snapshotID := "parent-"+uuid.NewString(), "snapshot-"+uuid.NewString()
	userID, inputID := "employee-"+uuid.NewString(), "input-"+uuid.NewString()
	resourceID, materialID := "file-"+uuid.NewString(), strings.Repeat("a", 24)
	subject := execution.Subject{WorkspaceID: workspaceID, UserID: userID}
	if _, err := pool.Exec(ctx, `
		INSERT INTO weave_workspaces(id,slug,name) VALUES($1,$1,'Fixture workspace');
		INSERT INTO weave_teams(id,workspace_id,name) VALUES($2,$1,'Fixture team');
		INSERT INTO weave_team_workflows(workspace_id,id,team_id,name) VALUES($1,$3,$2,'Fixture workflow');
		INSERT INTO weave_team_workflow_versions(workspace_id,workflow_id,version,status,trigger_config,graph_definition,created_by)
		VALUES($1,$3,1,'draft','{"schema_version":1}','{"schema_version":1}','fixture');
		INSERT INTO weave_agents(id,workspace_id,team_id,name,spec,version)
		VALUES($4,$1,$2,'fixture-lead','{}',1);
		INSERT INTO weave_agent_versions(workspace_id,agent_id,version,spec) VALUES($1,$4,1,'{}');`,
		workspaceID, teamID, workflowID, "agent-"+uuid.NewString()); err != nil {
		t.Fatalf("seed workspace and workflow identity: %v", err)
	}
	contentHash := strings.Repeat("c", 64)
	candidateEnvelope, _ := json.Marshal(map[string]any{
		"workspace_id": workspaceID, "workflow_id": workflowID, "workflow_version": 1, "content_hash": contentHash,
	})
	if _, err := pool.Exec(ctx, `INSERT INTO weave_team_workflow_candidates(
		workspace_id,workflow_id,workflow_version,content_hash,envelope_json,dependencies_json,expected_updated_at,created_by,created_at)
		VALUES($1,$2,1,$3,$4::jsonb,'[]',$5,'fixture',$5)`, workspaceID, workflowID, contentHash, string(candidateEnvelope), now); err != nil {
		t.Fatalf("seed candidate identity: %v", err)
	}
	trigger, _ := json.Marshal(map[string]any{"schema_version": 1, "type": "api", "source_ref": "fixture-source"})
	_, err := snapshot.NewStore(pool).Create(ctx, snapshot.TeamRunSnapshot{
		Subject: subject, RunID: snapshotID, WorkspaceID: workspaceID, TeamID: teamID,
		SnapshotSchemaVersion: 2, Mode: "fixed_workflow", WorkflowID: workflowID, WorkflowVersion: 1,
		ArtifactWorkflowID: workflowID, ArtifactWorkflowVersion: 1,
		AdmissionDecision: json.RawMessage(`{"schema_version":1,"team_active":true,"workflow_active":true,"workers_enabled":true,"version_blocked":false,"decided_at":"2026-10-04T00:00:00Z"}`),
		RunAssociations:   json.RawMessage(`{"schema_version":1,"parent_run_id":null,"source_snapshot_id":null,"task_group_id":null}`),
		TriggerSourceV2:   trigger, RuntimeAssignment: json.RawMessage(`{}`), SourceRef: "fixture-source", CandidateContentHash: contentHash,
	})
	if err != nil {
		t.Fatalf("create frozen parent snapshot: %v", err)
	}

	original := []byte("原始文本附件")
	content := "冻结的设备交付正文：版本 8A，校验通过。"
	originalHash := digestHex(original)
	extractionHash := digestHex([]byte(content))
	taskInput, err := json.Marshal(map[string]any{
		"goal": "核对设备材料", "materialHandling": "逐份读取冻结材料",
		"materials": []any{map[string]any{
			"materialId": materialID, "name": "设备清单.txt", "mediaType": "text/plain", "bytes": len(original), "sha256": originalHash,
			"extraction": map[string]any{"status": "complete", "mediaType": "text/plain; charset=utf-8", "bytes": len(content),
				"sha256": extractionHash, "sourceSha256": originalHash, "content": content, "extractor": "utf8",
				"coverage": map[string]any{"plainText": true}, "limitations": []string{}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	taskHash := digestHex(taskInput)
	resources := []businessaction.TaskDelegationResource{
		{Type: "dispatch-input", ID: inputID, SHA256: taskHash},
		{Type: "forge-file", ID: resourceID, MaterialID: materialID, Name: "设备清单.txt", MediaType: "text/plain", Bytes: int64(len(original)), SHA256: originalHash},
	}
	resourcesRaw, _ := json.Marshal(resources)
	credential := []byte("fixture-only-task-token")
	key := []byte(strings.Repeat("k", 32))
	sealed, err := secret.Seal(key, credential)
	if err != nil {
		t.Fatal(err)
	}
	grantScope := businessaction.TaskDelegationScope{
		InputRevisionID: inputID, RegistrationID: "registration-" + uuid.NewString(), TaskSHA256: taskHash,
		WorkflowID: workflowID, WorkflowVersion: 1, AllowedActions: []string{}, Resources: []businessaction.TaskDelegationResource{resources[1]},
	}
	grantScopeRaw, _ := json.Marshal(grantScope)
	scopeDigest, err := frozen.HashCanonicalJSON(grantScopeRaw)
	if err != nil {
		t.Fatal(err)
	}
	issuedAt, expiresAt := now.Add(-time.Minute), now.Add(time.Hour)
	grant := businessaction.TaskDelegationGrant{
		Version: "1", Active: true, TokenType: "forge_task", Issuer: "", IdentityIssuer: "forge:fixture",
		GrantID: "grant-" + uuid.NewString(), Generation: 1, IssuedAt: issuedAt, ExpiresAt: expiresAt,
		ScopeSHA256: scopeDigest, Scope: grantScope,
	}
	grant.Subject.ID, grant.Subject.OrganizationID = userID, "native-org"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != businessaction.TaskDelegationPath+"/current" || r.Header.Get("Authorization") != "Bearer "+string(credential) {
			http.Error(w, "fixture request mismatch", http.StatusUnauthorized)
			return
		}
		grant.Issuer = "http://" + r.Host
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(grant)
	}))
	defer server.Close()
	grant.Issuer = server.URL
	if _, err := pool.Exec(ctx, `INSERT INTO weave_dispatch_input_revisions(
		workspace_id,user_id,workbench_session_id,input_revision_id,registration_id,registration_sha256,source_messages,
		task,task_sha256,team_id,mode,workflow_id,workflow_version,client_request_id,execution_task,revision_kind,
		root_input_revision_id,consumed_run_id,consumed_task_id,consumed_at,native_organization)
		VALUES($1,$2,'session',$3,$4,$5,'["fixture-message"]',$6,$7,$8,'workflow',$9,1,$10,$6,'initial',$3,$11,'parent-task',$12,'native-org')`,
		workspaceID, userID, inputID, grantScope.RegistrationID, strings.Repeat("d", 64), string(taskInput), taskHash,
		teamID, workflowID, "client-"+uuid.NewString(), parentRunID, now); err != nil {
		t.Fatalf("seed frozen dispatch input: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_task_business_delegations(
		workspace_id,user_id,input_revision_id,delegation_id,credential_ref,issuer,external_subject,external_organization,
		credential_ciphertext,credential_sha256,allowed_actions,resources,workflow_id,workflow_version,issued_at,expires_at,
		grant_id,scope_sha256,refresh_generation,forge_base_url,forge_delegation_id)
		VALUES($1,$2,$3,$4,$5,'forge:fixture',$2,'native-org',$6,$7,'[]',$8::jsonb,$9,1,$10,$11,$12,$13,1,$14,$12)`,
		workspaceID, userID, inputID, uuid.NewString(), "credential-"+uuid.NewString(), string(sealed), digestHex(credential),
		string(resourcesRaw), workflowID, issuedAt, expiresAt, grant.GrantID, scopeDigest, server.URL); err != nil {
		t.Fatalf("seed task-scoped material grant: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_run_delivery_state(
		workspace_id,run_snapshot_id,run_id,input_revision_id,workflow_id,workflow_version,published_digest,contract,contract_digest)
		VALUES($1,$2,$3,$4,$5,1,$6,'null',$7)`, workspaceID, snapshotID, parentRunID, inputID,
		workflowID, contentHash, strings.Repeat("e", 64)); err != nil {
		t.Fatalf("seed delivery binding: %v", err)
	}

	runs := teamrun.NewPGStore()
	runTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	queued, err := runs.EstablishQueuedTx(ctx, runTx, teamrun.EstablishRequest{
		WorkspaceID: workspaceID, RunID: parentRunID, TeamID: teamID, WorkflowID: workflowID,
		WorkflowVersion: 1, RunSnapshotID: snapshotID, SourceKind: teamrun.SourceAPI,
		SourceTaskID: "source-" + uuid.NewString(), EstablishIdempotencyKey: "establish-" + uuid.NewString(),
		Actor: "fixture", Source: "fixture", OccurredAt: now,
	})
	if err != nil {
		_ = runTx.Rollback(ctx)
		t.Fatalf("establish parent run: %v", err)
	}
	running, err := runs.ClaimRunningTx(ctx, runTx, teamrun.ClaimRequest{
		WorkspaceID: workspaceID, RunID: parentRunID, ExpectedStatus: teamrun.StatusQueued,
		ExpectedTeamRunGeneration: queued.Generation, ExpectedExecutionLeaseEpoch: queued.ExecutionLeaseEpoch,
		ExpectedResumeGeneration: queued.ResumeGeneration, ExecutorID: "parent-executor",
		IdempotencyKey: "claim-" + uuid.NewString(), Actor: "fixture", Source: "fixture", OccurredAt: now,
	})
	if err != nil {
		_ = runTx.Rollback(ctx)
		t.Fatalf("claim parent run: %v", err)
	}
	attemptID := uuid.New()
	runStartedAt := now.Format(time.RFC3339Nano)
	if _, err := runTx.Exec(ctx, `INSERT INTO weave_run_attempt_leases(
		workspace_id,run_id,attempt_generation,attempt_id,graph_name,run_started_at,attempt_started_at,state,
		heartbeat_at,lease_expires_at,retry_count)
		VALUES($1,$2,1,$3,$4,$5,$6,'active',$6,$7,0)`,
		workspaceID, parentRunID, attemptID, workflowID, runStartedAt, now.Add(-3*time.Minute), now.Add(time.Hour)); err != nil {
		_ = runTx.Rollback(ctx)
		t.Fatalf("seed current parent attempt: %v", err)
	}
	if err := runTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	queue := taskqueue.New(pool, nil, time.Minute)
	fanoutStore := fanout.New(pool, fanout.RealClock{})
	resumeToken := "resume-" + uuid.NewString()
	intentTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := fanoutStore.CreateParkIntentTx(ctx, intentTx, fanout.PrepareParkRequest{
		WorkspaceID: workspaceID, ParentRunID: parentRunID, WorkflowID: workflowID, WorkflowVersion: 1,
		RunSnapshotID: snapshotID, NodeID: "parallel", PreviousCheckpointSequence: 0, NodeEntryOrdinal: 0,
		CreatorEpoch: int64(running.ExecutionLeaseEpoch), CreatorAttemptGeneration: 1, CreatorAttemptID: attemptID.String(),
		ActivationDeadline: now.Add(time.Hour), ResumeToken: resumeToken,
		JoinPolicy: fanout.JoinPolicy{Kind: fanout.JoinAllSuccess, DeadlineAt: now.Add(time.Hour), MaxDeadlineSeconds: 3600},
		Legs: []fanout.PlannedLeg{
			{LegID: "leg-a", BranchID: "worker-a", BranchOrdinal: 0, FrozenBundleRef: json.RawMessage(`{"agent":"a"}`), InputRef: json.RawMessage(`{"task":"a"}`), MayYieldProof: json.RawMessage(`{"may_yield":false}`)},
			{LegID: "leg-b", BranchID: "worker-b", BranchOrdinal: 1, FrozenBundleRef: json.RawMessage(`{"agent":"b"}`), InputRef: json.RawMessage(`{"task":"b"}`), MayYieldProof: json.RawMessage(`{"may_yield":false}`)},
		},
	})
	if err != nil {
		_ = intentTx.Rollback(ctx)
		t.Fatalf("create fanout intent and legs: %v", err)
	}
	waitDetail, _ := json.Marshal(map[string]any{
		"wait_type": "fanout_group", "parked": true, "intent_id": intent.IntentID, "group_id": intent.GroupID,
		"generation": intent.Generation, "resume_token": resumeToken, "parent_run_id": parentRunID, "join_node_id": "join",
	})
	resumeHash := sha256.Sum256([]byte(resumeToken))
	checkpoint := teamrun.WorkflowCheckpointV1{
		SchemaVersion: teamrun.WorkflowCheckpointSchemaVersion,
		Stamp:         teamrun.WorkflowRunStamp{WorkspaceID: workspaceID, WorkflowID: workflowID, WorkflowVersion: 1, RunSnapshotID: snapshotID},
		RunID:         parentRunID, TeamRunGeneration: running.Generation, ExecutionLeaseEpoch: running.ExecutionLeaseEpoch,
		NodeID: "join", CompletedOutputs: map[string]json.RawMessage{"previous": json.RawMessage(`{"ok":true}`)}, WrittenAt: now,
	}
	checkpoints := teamrun.NewPGCheckpointStore()
	parked, err := runs.ParkTx(ctx, intentTx, teamrun.ParkRequest{
		WorkspaceID: workspaceID, RunID: parentRunID, ExpectedStatus: teamrun.StatusRunning,
		ExpectedTeamRunGeneration: running.Generation, ExpectedExecutionLeaseEpoch: running.ExecutionLeaseEpoch,
		ExpectedResumeGeneration: running.ResumeGeneration, ExecutorID: "parent-executor", WaitKind: teamrun.WaitFanout,
		WaitDetail: waitDetail, ResumeTokenHash: resumeHash[:], CheckpointRef: teamrun.CheckpointRef(workspaceID, parentRunID),
		IdempotencyKey: "park-" + uuid.NewString(), Actor: "fixture", Source: "fixture", OccurredAt: now,
	})
	if err != nil {
		_ = intentTx.Rollback(ctx)
		t.Fatalf("park parent run: %v", err)
	}
	if _, err := checkpoints.PutTx(ctx, intentTx, checkpoint); err != nil {
		_ = intentTx.Rollback(ctx)
		t.Fatalf("persist yielded checkpoint: %v", err)
	}
	if _, err := intentTx.Exec(ctx, `UPDATE weave_run_attempt_leases
		SET state='yielded',heartbeat_at=$3,lease_expires_at=$4 WHERE workspace_id=$1 AND run_id=$2`,
		workspaceID, parentRunID, now.Add(-2*time.Minute), now.Add(-time.Minute)); err != nil {
		_ = intentTx.Rollback(ctx)
		t.Fatalf("release yielded parent attempt lease: %v", err)
	}
	teamIDValue, workflowIDValue, snapshotIDValue := teamID, workflowID, snapshotID
	workflowVersion := int32(1)
	checkpointSequence := int64(parked.ResumeGeneration)
	if _, err := intentTx.Exec(ctx, `INSERT INTO weave_run_terminal_markers(
		workspace_id,run_id,schema_version,attempt_generation,attempt_id,agent,attribution_scope,team_id,workflow_id,
		workflow_version,run_snapshot_id,run_started_at,phase,status,stop_reason,source,terminal_at,evidence_kind,
		checkpoint_graph,checkpoint_seq,checkpoint_saved_at,usage_input_tokens,usage_output_tokens,usage_cost_usd,
		usage_tool_calls,audit_state,audit_schema_version,lineage_state,created_at,updated_at)
		VALUES($1,$2,1,1,$3,$4,'fixed_workflow',$5,$6,$7,$8,$9,'yielded','yielded','yielded','normal',$10,
		'checkpoint',$6,$11,$10,0,0,0,0,'materialized',3,'pending',$10,$10)`,
		workspaceID, parentRunID, attemptID, workflowID, teamIDValue, workflowIDValue, workflowVersion,
		snapshotIDValue, runStartedAt, now, checkpointSequence); err != nil {
		_ = intentTx.Rollback(ctx)
		t.Fatalf("persist yielded parent marker: %v", err)
	}
	if err := intentTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	coordinator := &fanout.WorkflowCoordinator{
		Transactions: pool, Store: fanoutStore,
		Checkpoints: &teamrun.FanoutCheckpointReader{Transactions: pool, Runs: runs, Checkpoints: checkpoints}, Tasks: queue,
		Now: func() time.Time { return now },
	}
	if _, err := coordinator.ActivatePark(ctx, fanout.ActivateParkRequest{
		WorkspaceID: workspaceID, IntentID: intent.IntentID, ParentRunID: parentRunID,
		CheckpointSequence: int64(parked.ResumeGeneration), Generation: intent.Generation, ResumeToken: resumeToken,
	}); err != nil {
		t.Fatalf("activate actual fanout leg tasks: %v", err)
	}
	claimed, err := queue.Claim(ctx, "fanout-worker", taskqueue.ClaimFilter{
		Kind: "team_workflow", WorkspaceID: workspaceID, RunSnapshotID: snapshotID,
		IdentityKind: taskqueue.IdentityTeamWorkflow,
	})
	if err != nil || claimed == nil {
		t.Fatalf("claim actual fanout leg: task=%v err=%v", claimed, err)
	}
	boundCtx, err := taskqueue.BindTaskExecution(execution.WithSubject(ctx, subject), claimed, queue)
	if err != nil {
		t.Fatalf("bind current fanout claim: %v", err)
	}
	reader := businessaction.NewStore(pool, queue, key)
	dispatcher, err := reader.MaterialReadDispatcher(boundCtx)
	if err != nil || dispatcher == nil {
		t.Fatalf("material reader rejected current parked fanout leg: dispatcher=%v err=%v", dispatcher, err)
	}
	args, _ := json.Marshal(map[string]any{"materialId": materialID, "sha256": originalHash})
	read, err := dispatcher.Dispatch(boundCtx, contract.ToolCall{ID: "material-read", Name: "read_frozen_material", Args: string(args)})
	if err != nil || read == nil || read.IsError {
		t.Fatalf("read frozen fanout material: result=%+v err=%v", read, err)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(read.Content), &result); err != nil || result["status"] != "complete" || result["content"] != content {
		t.Fatalf("fanout material result=%s err=%v, want exact frozen body", read.Content, err)
	}
	secondClaim, err := queue.Claim(ctx, "second-fanout-worker", taskqueue.ClaimFilter{
		Kind: "team_workflow", WorkspaceID: workspaceID, RunSnapshotID: snapshotID,
		IdentityKind: taskqueue.IdentityTeamWorkflow,
	})
	if err != nil || secondClaim == nil || secondClaim.Source != "fanout" {
		t.Fatalf("claim second registered fanout leg: task=%+v err=%v", secondClaim, err)
	}
	secondCtx, err := taskqueue.BindTaskExecution(execution.WithSubject(ctx, subject), secondClaim, queue)
	if err != nil {
		t.Fatalf("bind second fanout leg: %v", err)
	}
	secondDispatcher, err := reader.MaterialReadDispatcher(secondCtx)
	if err != nil || secondDispatcher == nil {
		t.Fatalf("material reader rejected second registered fanout leg: dispatcher=%v err=%v", secondDispatcher, err)
	}
	secondRead, err := secondDispatcher.Dispatch(secondCtx, contract.ToolCall{ID: "second-material-read", Name: "read_frozen_material", Args: string(args)})
	if err != nil || secondRead == nil || secondRead.IsError || !strings.Contains(secondRead.Content, content) {
		t.Fatalf("second registered fanout leg could not read frozen material: result=%+v err=%v", secondRead, err)
	}
	if err := queue.CompleteClaimed(ctx, secondClaim.ID, "second-fanout-worker", json.RawMessage(`{"read":true}`), parentRunID); err != nil {
		t.Fatalf("complete second fixture fanout task: %v", err)
	}

	assertUnavailable := func(label string) {
		t.Helper()
		resultTool, dispatchErr := dispatcher.Dispatch(boundCtx, contract.ToolCall{ID: label, Name: "read_frozen_material", Args: string(args)})
		if dispatchErr != nil || resultTool == nil || resultTool.IsError {
			t.Fatalf("%s rejected dispatcher call unexpectedly: result=%+v err=%v", label, resultTool, dispatchErr)
		}
		var blocked map[string]any
		if json.Unmarshal([]byte(resultTool.Content), &blocked) != nil || blocked["status"] != "unavailable" || blocked["content"] != nil {
			t.Fatalf("%s exposed frozen text outside a valid parked fanout: %s", label, resultTool.Content)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE weave_team_runs SET wait_kind='runtime',wait_detail='{"node_id":"other"}'::jsonb WHERE workspace_id=$1 AND run_id=$2`, workspaceID, parentRunID); err != nil {
		t.Fatalf("set non-fanout wait: %v", err)
	}
	assertUnavailable("non-fanout-wait")
	if _, err := pool.Exec(ctx, `UPDATE weave_team_runs SET wait_kind='fanout',wait_detail=$3::jsonb WHERE workspace_id=$1 AND run_id=$2`, workspaceID, parentRunID, string(waitDetail)); err != nil {
		t.Fatalf("restore exact parked fanout wait: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE weave_team_runs SET wait_kind='fanout',wait_detail=jsonb_set(wait_detail,'{group_id}','"unrelated-group"') WHERE workspace_id=$1 AND run_id=$2`, workspaceID, parentRunID); err != nil {
		t.Fatalf("set unrelated group identity: %v", err)
	}
	assertUnavailable("unrelated-group")
	if _, err := pool.Exec(ctx, `UPDATE weave_team_runs SET wait_detail=$3::jsonb WHERE workspace_id=$1 AND run_id=$2`, workspaceID, parentRunID, string(waitDetail)); err != nil {
		t.Fatalf("restore exact fanout group identity: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE weave_team_runs SET wait_detail=jsonb_set(wait_detail,'{generation}','"wrong-generation"') WHERE workspace_id=$1 AND run_id=$2`, workspaceID, parentRunID); err != nil {
		t.Fatalf("set wrong parent fanout generation: %v", err)
	}
	assertUnavailable("wrong-parent-generation")
	if _, err := pool.Exec(ctx, `UPDATE weave_team_runs SET wait_detail=$3::jsonb WHERE workspace_id=$1 AND run_id=$2`, workspaceID, parentRunID, string(waitDetail)); err != nil {
		t.Fatalf("restore parent fanout generation: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE weave_task_queue SET payload=jsonb_set(payload,'{generation}','"wrong-generation"') WHERE workspace_id=$1 AND id=$2`, workspaceID, claimed.ID); err != nil {
		t.Fatalf("set wrong fanout leg generation: %v", err)
	}
	assertUnavailable("wrong-leg-generation")
	if _, err := pool.Exec(ctx, `UPDATE weave_task_queue SET payload=$3::jsonb WHERE workspace_id=$1 AND id=$2`, workspaceID, claimed.ID, string(claimed.Payload)); err != nil {
		t.Fatalf("restore exact fanout leg payload: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE weave_fanout_leg SET branch_id='unrelated-branch' WHERE workspace_id=$1 AND group_id=$2 AND leg_id='leg-a'`, workspaceID, intent.GroupID); err != nil {
		t.Fatalf("set unrelated fanout leg branch: %v", err)
	}
	assertUnavailable("unrelated-leg-branch")
	if _, err := pool.Exec(ctx, `UPDATE weave_fanout_leg SET branch_id='worker-a' WHERE workspace_id=$1 AND group_id=$2 AND leg_id='leg-a'`, workspaceID, intent.GroupID); err != nil {
		t.Fatalf("restore fanout leg branch: %v", err)
	}
	wrongActor := execution.WithSubject(boundCtx, execution.Subject{WorkspaceID: workspaceID, UserID: "unrelated-" + uuid.NewString()})
	if leaked, dispatchErr := dispatcher.Dispatch(wrongActor, contract.ToolCall{ID: "wrong-actor", Name: "read_frozen_material", Args: string(args)}); dispatchErr == nil && leaked != nil && strings.Contains(leaked.Content, content) {
		t.Fatal("wrong actor received frozen material")
	}
	expiredAt := time.Now().UTC().Add(-time.Minute)
	if _, err := pool.Exec(ctx, `UPDATE weave_task_queue SET lease_expires_at=$3 WHERE workspace_id=$1 AND id=$2`, workspaceID, claimed.ID, expiredAt); err != nil {
		t.Fatalf("expire claimed fanout task: %v", err)
	}
	if leaked, dispatchErr := dispatcher.Dispatch(boundCtx, contract.ToolCall{ID: "expired-claim", Name: "read_frozen_material", Args: string(args)}); dispatchErr == nil && leaked != nil && strings.Contains(leaked.Content, content) {
		t.Fatal("expired fanout claim received frozen material")
	}
	if _, err := pool.Exec(ctx, `UPDATE weave_task_queue SET lease_expires_at=$3 WHERE workspace_id=$1 AND id=$2`, workspaceID, claimed.ID, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatalf("restore current fanout claim lease: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE weave_team_runs SET status='cancel_requested',wait_kind=NULL,wait_detail=NULL,
		resume_token_hash=NULL,checkpoint_ref=NULL,cancel_actor='fixture',cancel_reason='fixture cancellation',
		cancel_idempotency_key='cancel-fixture',cancel_requested_at=$3::timestamptz,cancel_grace_deadline_at=$3::timestamptz+interval '1 minute'
		WHERE workspace_id=$1 AND run_id=$2`, workspaceID, parentRunID, time.Now().UTC()); err != nil {
		t.Fatalf("set cancelled parent state: %v", err)
	}
	assertUnavailable("cancelled-parent")
	if _, err := pool.Exec(ctx, `UPDATE weave_team_runs SET status='failed',current_executor_id=NULL,wait_kind=NULL,wait_detail=NULL,
		resume_token_hash=NULL,checkpoint_ref=NULL,cancel_actor=NULL,cancel_reason=NULL,cancel_idempotency_key=NULL,
		cancel_requested_at=NULL,cancel_grace_deadline_at=NULL,error_code='team_run_execution_failed',cause_summary='fixture terminal',terminal_at=$3,updated_at=$3
		WHERE workspace_id=$1 AND run_id=$2`, workspaceID, parentRunID, now); err != nil {
		t.Fatalf("set terminal parent state: %v", err)
	}
	assertUnavailable("terminal-parent")

	if _, err := pool.Exec(ctx, `UPDATE weave_team_runs SET status='running',team_run_generation=team_run_generation+1,
		current_executor_id='resumed-executor',error_code=NULL,cause_summary=NULL,terminal_at=NULL,updated_at=$3
		WHERE workspace_id=$1 AND run_id=$2`, workspaceID, parentRunID, time.Now().UTC()); err != nil {
		t.Fatalf("restore a live running parent for compatibility check: %v", err)
	}
	err = queue.Enqueue(ctx, &taskqueue.Task{
		ID: "running-root-" + uuid.NewString(), WorkspaceID: workspaceID, Subject: subject,
		IdentityKind: taskqueue.IdentityTeamWorkflow, IdentitySchemaVersion: 2,
		WorkflowID: workflowID, WorkflowVersion: 1, RunSnapshotID: snapshotID,
		Source: "api", SourceRef: "fixture-running-task", Kind: "team_workflow", Payload: json.RawMessage(`{"input":"running-root"}`),
	})
	if err != nil {
		t.Fatalf("enqueue normal running task: %v", err)
	}
	runningClaim, err := queue.Claim(ctx, "running-worker", taskqueue.ClaimFilter{
		Kind: "team_workflow", WorkspaceID: workspaceID, RunSnapshotID: snapshotID,
		IdentityKind: taskqueue.IdentityTeamWorkflow,
	})
	if err != nil || runningClaim == nil || runningClaim.Source != "api" {
		t.Fatalf("claim normal running task: task=%+v err=%v", runningClaim, err)
	}
	runningCtx, err := taskqueue.BindTaskExecution(execution.WithSubject(ctx, subject), runningClaim, queue)
	if err != nil {
		t.Fatalf("bind normal running task: %v", err)
	}
	runningDispatcher, err := reader.MaterialReadDispatcher(runningCtx)
	if err != nil || runningDispatcher == nil {
		t.Fatalf("normal running material reader lost compatibility: dispatcher=%v err=%v", runningDispatcher, err)
	}
	runningRead, err := runningDispatcher.Dispatch(runningCtx, contract.ToolCall{ID: "running-read", Name: "read_frozen_material", Args: string(args)})
	if err != nil || runningRead == nil || runningRead.IsError || !strings.Contains(runningRead.Content, content) {
		t.Fatalf("normal running task could not read frozen material: result=%+v err=%v", runningRead, err)
	}
}

func digestHex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
