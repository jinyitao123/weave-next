package publicationservice

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/publication"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

type proofAuthority func(context.Context, string, frozen.ArtifactEnvelopeV1) (machine.ValidationContext, error)

func (f proofAuthority) Authorize(ctx context.Context, operation string, envelope frozen.ArtifactEnvelopeV1) (machine.ValidationContext, error) {
	return f(ctx, operation, envelope)
}

func publicationPGFixture(t *testing.T) (context.Context, *Service, *pgxpool.Pool, publication.PublishRequest) {
	t.Helper()
	ctx := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "workspace", UserID: "alice"})
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	gate := proofAuthority(func(ctx context.Context, _ string, envelope frozen.ArtifactEnvelopeV1) (machine.ValidationContext, error) {
		subject, err := execution.RequireSubject(ctx, envelope.WorkspaceID)
		if err != nil {
			return machine.ValidationContext{}, err
		}
		if subject.UserID != "alice" && subject.UserID != "bob" {
			return machine.ValidationContext{}, errors.New("asset access denied")
		}
		return machine.ValidationContext{
			Authorization: machine.NewAuthorizationSnapshot("workspace", "team", machine.ProofResolved, nil, nil),
			Agents:        map[machine.AgentVersionKey]machine.AgentVersionProof{{AgentID: "lead", AgentVersion: 1}: machine.NewAgentVersionProof(machine.AgentProofResolved, machine.FactoryKey{}, nil, false, false)},
			Dependencies:  machine.NewDependencySnapshot(nil, machine.NewDependencyProof(machine.DependencyProofResolved, nil)),
		}, nil
	})
	connection, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	query := connection.Query()
	query.Set("search_path", pool.Config().ConnConfig.RuntimeParams["search_path"])
	connection.RawQuery = query.Encode()
	service, err := Open(ctx, connection.String(), gate)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	payload := frozen.ArtifactPayloadV1{SchemaVersion: 1,
		TriggerConfig:   json.RawMessage(`{"schema_version":1,"type":"conversation_explicit","config":{}}`),
		GraphDefinition: json.RawMessage(`{"schema_version":1,"entry_node_id":"deliver","input_contract":{"type":"text"},"output_contract":{"type":"text"},"nodes":[{"id":"deliver","type":"deliver","config":{"result":{"source":"run_input","path":""}}}],"edges":[]}`),
		Team:            frozen.ArtifactTeamV1{WorkspaceID: "workspace", TeamID: "team", LeadAgentID: "lead", LeadAgentVersion: 1, LeadAgentContentHash: strings.Repeat("a", 64)}}
	encoded, err := frozen.Canonicalize(payload, frozen.PreorderArtifactPayloadV1)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := frozen.ComputeArtifactContentHash(frozen.ArtifactEnvelopeHashInputV1{WorkspaceID: "workspace", WorkflowID: "workflow", WorkflowVersion: 1, ArtifactSchemaVersion: 1,
		CanonicalizationAlgorithm: frozen.ArtifactCanonicalizationAlgorithm, CanonicalizationVersion: 1, HashAlgorithm: "sha256", Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	request := publication.PublishRequest{Version: publication.ContractVersion, RequestID: "publish-1", Candidate: frozen.ArtifactEnvelopeV1{WorkspaceID: "workspace", WorkflowID: "workflow", WorkflowVersion: 1,
		ArtifactSchemaVersion: 1, CanonicalizationAlgorithm: frozen.ArtifactCanonicalizationAlgorithm, CanonicalizationVersion: 1, HashAlgorithm: "sha256", ContentHash: hash, Payload: encoded}}
	return ctx, service, pool, request
}

func TestPublicationOwnsFrozenTransactionWithoutProductRowsRealPG(t *testing.T) {
	ctx, service, pool, request := publicationPGFixture(t)
	var wg sync.WaitGroup
	results := make(chan publication.PublishReceipt, 8)
	failures := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			receipt, err := service.Publish(ctx, request)
			results <- receipt
			failures <- err
		}()
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	var expected publication.PublishReceipt
	for receipt := range results {
		if expected.RequestID == "" {
			expected = receipt
		}
		if receipt != expected {
			t.Fatal("concurrent publication returned different receipt")
		}
	}
	for table, want := range map[string]int{"weave_published_artifact_contents": 1, "weave_kernel_publication_requests": 1, "weave_workflow_version_admission_statuses": 1,
		"weave_team_workflows": 0, "weave_team_workflow_versions": 0, "weave_teams": 0, "weave_workspaces": 0} {
		var count int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != want {
			t.Fatalf("%s count=%d want=%d", table, count, want)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE weave_published_artifact_contents SET content_hash=repeat('b',64)`); err == nil {
		t.Fatal("published revision was mutable")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_team_workflow_dependencies(workspace_id,workflow_id,workflow_version,owner_type,owner_id,dependency_type,dependency_key,content_hash)
	VALUES('workspace','workflow',1,'workflow','workflow','factory','late',repeat('b',64))`); err == nil {
		t.Fatal("late dependency appended to frozen revision")
	}
	other := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "workspace", UserID: "bob"})
	if _, err := service.Publish(other, request); !errors.Is(err, execution.ErrSubjectMismatch) {
		t.Fatalf("foreign replay accepted: %v", err)
	}
}

func TestPublicationReceiptFailureRollsBackFrozenRevisionRealPG(t *testing.T) {
	ctx, service, pool, request := publicationPGFixture(t)
	if _, err := pool.Exec(ctx, `CREATE FUNCTION fail_publication_receipt() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected receipt failure'; END; $$;
	CREATE TRIGGER fail_publication_receipt BEFORE INSERT ON weave_kernel_publication_requests FOR EACH ROW EXECUTE FUNCTION fail_publication_receipt();`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Publish(ctx, request); err == nil {
		t.Fatal("expected receipt persistence failure")
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM weave_published_artifact_contents`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("orphan revision committed before receipt")
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER fail_publication_receipt ON weave_kernel_publication_requests`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Publish(ctx, request); err != nil {
		t.Fatal(err)
	}
}

func TestCandidateAdmissionOwnsSnapshotTaskAndReceiptRealPG(t *testing.T) {
	ctx, service, pool, publish := publicationPGFixture(t)
	deadline := time.Now().UTC().Add(time.Hour)
	request := publication.CandidateRunRequest{Version: publication.ContractVersion, RequestID: "candidate-1", Candidate: publish.Candidate,
		Input: json.RawMessage(`{"input":"test"}`), InputVersion: "input-1", SourceRef: "product-request", Purpose: "verification", DeadlineAt: &deadline}
	receipt, err := service.AdmitCandidate(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := service.AdmitCandidate(ctx, request)
	if err != nil || receipt != replayed {
		t.Fatalf("admission replay changed: %v", err)
	}
	var taskActor, snapshotActor execution.Subject
	var taskActorRaw, snapshotActorRaw []byte
	var candidateHash string
	var buildRunID *string
	if err = pool.QueryRow(ctx, `SELECT q.actor_subject,s.actor_subject,s.candidate_content_hash,s.build_run_id FROM weave_task_queue q JOIN weave_team_run_snapshots s ON s.run_id=q.run_snapshot_id WHERE q.id=$1`, receipt.TaskID).Scan(&taskActorRaw, &snapshotActorRaw, &candidateHash, &buildRunID); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(taskActorRaw, &taskActor); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(snapshotActorRaw, &snapshotActor); err != nil {
		t.Fatal(err)
	}
	if taskActor != receipt.Subject || snapshotActor != receipt.Subject || candidateHash != publish.Candidate.ContentHash || buildRunID != nil {
		t.Fatal("candidate identity was coupled to product build or changed actor")
	}
	other := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "workspace", UserID: "bob"})
	if _, err = service.AdmitCandidate(other, request); !errors.Is(err, execution.ErrSubjectMismatch) {
		t.Fatalf("foreign admission replay accepted: %v", err)
	}
	request.RequestID = "candidate-2"
	second, err := service.AdmitCandidate(other, request)
	if err != nil {
		t.Fatal(err)
	}
	if second.TaskID == receipt.TaskID || second.Subject == receipt.Subject {
		t.Fatal("distinct user admission shared task identity")
	}
	for _, table := range []string{"weave_teams", "weave_team_build_runs", "weave_workspaces"} {
		var count int
		if err = pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatal("kernel wrote product table", table)
		}
	}
}

func TestCandidateConflictChecksPersistedDependenciesRealPG(t *testing.T) {
	ctx, service, pool, publish := publicationPGFixture(t)
	envelope, _ := json.Marshal(publish.Candidate)
	if _, err := pool.Exec(ctx, `INSERT INTO weave_team_workflow_candidates(workspace_id,workflow_id,workflow_version,content_hash,envelope_json,dependencies_json,expected_updated_at,created_by,created_at)
	VALUES('workspace','workflow',1,$1,$2::jsonb,'[]',now(),'seed',now())`, publish.Candidate.ContentHash, string(envelope)); err != nil {
		t.Fatal(err)
	}
	request := publication.CandidateRunRequest{Version: publication.ContractVersion, RequestID: "corrupt-candidate", Candidate: publish.Candidate, Input: json.RawMessage(`{"input":"x"}`), InputVersion: "v1", SourceRef: "source", Purpose: "trial"}
	if _, err := service.AdmitCandidate(ctx, request); !errors.Is(err, publication.ErrRequestConflict) {
		t.Fatalf("corrupt stored dependencies reused: %v", err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM weave_task_queue`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("task admitted from conflicting candidate")
	}
}

func TestDraftDependenciesRemainMutableWhileFrozenDependenciesDoNotRealPG(t *testing.T) {
	ctx, service, pool, publish := publicationPGFixture(t)
	if _, err := service.Publish(ctx, publish); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES('workspace','workspace','Workspace');
	INSERT INTO weave_agents(id,workspace_id,name,role,spec) VALUES('lead','workspace','lead','avatar','{}');
	INSERT INTO weave_teams(id,workspace_id,name,lead_avatar_id,status) VALUES('team','workspace','Team','lead','building');
	INSERT INTO weave_team_workflows(workspace_id,id,team_id,name) VALUES('workspace','draft','team','Draft');
	INSERT INTO weave_team_workflow_versions(workspace_id,workflow_id,version,status,trigger_config,graph_definition,created_by)
	VALUES('workspace','draft',1,'draft','{"schema_version":1}','{"schema_version":1}','alice');
	INSERT INTO weave_draft_workflow_dependencies(workspace_id,workflow_id,workflow_version,owner_type,owner_id,dependency_type,dependency_key,content_hash)
	VALUES('workspace','draft',1,'workflow','draft','factory','draft-factory',repeat('a',64));`); err != nil {
		t.Fatal(err)
	}
	if err := workflow.New(pool, nil).DeletePureDraft(ctx, "workspace", "draft"); err != nil {
		t.Fatal("draft with dependencies cannot be deleted:", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE weave_team_workflow_dependencies SET content_hash=repeat('b',64) WHERE workflow_id='workflow'`); err == nil {
		t.Fatal("frozen dependency updated")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM weave_team_workflow_dependencies WHERE workflow_id='workflow'`); err == nil {
		t.Fatal("frozen dependency deleted")
	}
}

func TestCandidateUnknownRequiresStopBeforeSameTaskResumeRealPG(t *testing.T) {
	ctx, service, pool, publish := publicationPGFixture(t)
	deadline := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	request := publication.CandidateRunRequest{Version: publication.ContractVersion, RequestID: "unknown-candidate", Candidate: publish.Candidate, Input: json.RawMessage(`{"input":"test"}`), InputVersion: "v1", SourceRef: "trial", Purpose: "verification", DeadlineAt: &deadline}
	receipt, err := service.AdmitCandidate(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	tasks := taskqueue.New(pool, nil, time.Minute)
	claimed, err := tasks.Claim(ctx, "original-worker", taskqueue.ClaimFilter{Kind: "team_workflow", WorkspaceID: "workspace", IdentityKind: taskqueue.IdentityTeamWorkflow})
	if err != nil {
		t.Fatal(err)
	}
	if claimed == nil || claimed.ID != receipt.TaskID || !claimed.OutcomeSensitive {
		t.Fatal("candidate bypassed outcome-sensitive platform claim")
	}
	if _, err = pool.Exec(ctx, `UPDATE weave_task_queue SET lease_expires_at=now()-interval '1 minute' WHERE id=$1`, claimed.ID); err != nil {
		t.Fatal(err)
	}
	if count, err := tasks.RecoverStale(ctx); err != nil || count != 1 {
		t.Fatalf("recover count=%d err=%v", count, err)
	}
	unknown, err := tasks.Get(ctx, "workspace", claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unknown.Status != taskqueue.StatusFailed || unknown.WorkerID != "original-worker" {
		t.Fatal("unknown execution owner lost")
	}
	resume := func(ctx context.Context) (*taskqueue.Task, error) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return nil, err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		v, err := tasks.ResumeTaskTx(ctx, tx, "workspace", claimed.ID, "team_workflow", claimed.ContextKey)
		if err != nil {
			return nil, err
		}
		return v, tx.Commit(ctx)
	}
	if _, err = resume(ctx); err == nil {
		t.Fatal("unknown candidate resumed without physical stop")
	}
	if err = tasks.RecordClaimUsage(ctx, claimed.ID, "original-worker", claimed.ClaimEpoch, &execution.TerminalUsage{InputTokens: 17, OutputTokens: 9, ToolCalls: 2}); err != nil {
		t.Fatal(err)
	}
	if err = tasks.AcknowledgeExecutionStopped(ctx, claimed.ID, "original-worker"); err != nil {
		t.Fatal(err)
	}
	other := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "workspace", UserID: "bob"})
	if _, err = resume(other); err == nil {
		t.Fatal("other user resumed candidate")
	}
	resumed, err := resume(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.ID != receipt.TaskID || !resumed.DeadlineAt.Equal(deadline) || resumed.PhysicalUsage.InputTokens != 17 || resumed.PhysicalUsage.ToolCalls != 2 {
		t.Fatal("resume replaced task, deadline, or usage")
	}
	next, err := tasks.Claim(ctx, "next-worker", taskqueue.ClaimFilter{Kind: "team_workflow", WorkspaceID: "workspace", IdentityKind: taskqueue.IdentityTeamWorkflow})
	if err != nil {
		t.Fatal(err)
	}
	if next == nil || next.ClaimEpoch != claimed.ClaimEpoch+1 {
		t.Fatal("resume did not allocate next physical epoch")
	}
	if err = tasks.RecordClaimUsage(ctx, claimed.ID, "original-worker", claimed.ClaimEpoch, &execution.TerminalUsage{InputTokens: 999}); err == nil {
		t.Fatal("late old usage attached to resumed attempt")
	}
	replay, err := service.AdmitCandidate(ctx, request)
	if err != nil || replay != receipt {
		t.Fatal("admission replay replaced candidate after resume", err)
	}
}
