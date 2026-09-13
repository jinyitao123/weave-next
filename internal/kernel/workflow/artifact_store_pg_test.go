package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/testutil"
)

// The fixture contains only frozen publication and admission records. No
// account, team, workflow catalog or draft table is available to the reader.
func artifactReadFixture(t *testing.T) (*pgxpool.Pool, *ArtifactStore, frozen.ArtifactEnvelopeV1) {
	t.Helper()
	seed := testutil.PostgresPool(t)
	_, err := seed.Exec(t.Context(), `
 CREATE TABLE weave_published_artifact_contents(workspace_id text, workflow_id text, workflow_version int, artifact_schema_version int, canonicalization_algorithm text, canonicalization_version int, hash_algorithm text, content_hash text, payload jsonb, created_at timestamptz DEFAULT now(), PRIMARY KEY(workspace_id,workflow_id,workflow_version));
 CREATE TABLE weave_team_workflow_dependencies(workspace_id text, workflow_id text, workflow_version int, owner_type text, owner_id text, owner_agent_version bigint, dependency_type text, dependency_key text, dependency_version bigint, content_hash text);
 CREATE TABLE weave_team_workflow_candidates(workspace_id text, workflow_id text, workflow_version int, content_hash text, envelope_json jsonb, dependencies_json jsonb, expected_updated_at timestamptz, created_by text, created_at timestamptz DEFAULT now());
 CREATE TABLE weave_workflow_version_admission_statuses(workspace_id text, workflow_id text, workflow_version int, blocked bool, PRIMARY KEY(workspace_id,workflow_id,workflow_version));
 CREATE TABLE weave_workflow_version_admission_audits(workspace_id text, workflow_id text, workflow_version int, audit_id text, idempotency_key text, old_blocked bool, new_blocked bool, operator_id text, reason text, created_at timestamptz);
 CREATE TABLE weave_workflow_version_admission_receipts(workspace_id text, workflow_id text, workflow_version int, idempotency_key text, request_hash text, response jsonb, created_at timestamptz, PRIMARY KEY(workspace_id,workflow_id,workflow_version,idempotency_key));
 CREATE TABLE weave_workflow_admission_denials(workspace_id text, workflow_id text, workflow_version int, trigger_type text, admission_attempt_key text, reason_code text, decided_at timestamptz, PRIMARY KEY(workspace_id,workflow_id,workflow_version,trigger_type,admission_attempt_key));
 INSERT INTO weave_workflow_version_admission_statuses VALUES ('workspace','workflow',1,false);
 INSERT INTO weave_team_workflow_dependencies VALUES ('workspace','workflow',1,'worker','z',2,'skill','report',1,'bbbb'),('workspace','workflow',1,'lead','a',1,'factory','chat',NULL,'aaaa'),('foreign','workflow',1,'lead','private',1,'factory','private',NULL,'private');
 `)
	if err != nil {
		t.Fatal(err)
	}
	cfg := seed.Config()
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	payload := frozen.ArtifactPayloadV1{SchemaVersion: 1,
		TriggerConfig:   json.RawMessage(`{"schema_version":1,"type":"conversation_explicit","config":{}}`),
		GraphDefinition: json.RawMessage(`{"schema_version":1,"entry_node_id":"deliver","input_contract":{"type":"text"},"output_contract":{"type":"text"},"nodes":[{"id":"deliver","type":"deliver","config":{"result":{"source":"run_input","path":""}}}],"edges":[]}`),
		Team:            frozen.ArtifactTeamV1{WorkspaceID: "workspace", TeamID: "team", LeadAgentID: "lead", LeadAgentVersion: 1, LeadAgentContentHash: strings.Repeat("a", 64)}}
	encoded, err := frozen.Canonicalize(payload, frozen.PreorderArtifactPayloadV1)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := frozen.ComputeArtifactContentHash(frozen.ArtifactEnvelopeHashInputV1{WorkspaceID: "workspace", WorkflowID: "workflow", WorkflowVersion: 1, ArtifactSchemaVersion: 1, CanonicalizationAlgorithm: frozen.ArtifactCanonicalizationAlgorithm, CanonicalizationVersion: 1, HashAlgorithm: "sha256", Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	envelope := frozen.ArtifactEnvelopeV1{WorkspaceID: "workspace", WorkflowID: "workflow", WorkflowVersion: 1, ArtifactSchemaVersion: 1, CanonicalizationAlgorithm: frozen.ArtifactCanonicalizationAlgorithm, CanonicalizationVersion: 1, HashAlgorithm: "sha256", ContentHash: hash, Payload: encoded}
	if _, err := frozen.DecodeArtifactEnvelopeV1(envelope); err != nil {
		t.Fatalf("invalid fixture envelope: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO weave_published_artifact_contents(workspace_id,workflow_id,workflow_version,artifact_schema_version,canonicalization_algorithm,canonicalization_version,hash_algorithm,content_hash,payload) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, envelope.WorkspaceID, envelope.WorkflowID, envelope.WorkflowVersion, envelope.ArtifactSchemaVersion, envelope.CanonicalizationAlgorithm, envelope.CanonicalizationVersion, envelope.HashAlgorithm, envelope.ContentHash, envelope.Payload); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO weave_team_workflow_candidates(workspace_id,workflow_id,workflow_version,content_hash,envelope_json,dependencies_json,expected_updated_at,created_by) VALUES($1,$2,$3,$4,$5,'[]',now(),'user')`, envelope.WorkspaceID, envelope.WorkflowID, envelope.WorkflowVersion, envelope.ContentHash, raw); err != nil {
		t.Fatal(err)
	}
	return pool, NewArtifactStore(pool, nil), envelope
}

func TestArtifactStoreReadsFrozenFactsWithoutProductTables(t *testing.T) {
	_, store, envelope := artifactReadFixture(t)
	ctx := t.Context()
	artifact, err := store.GetArtifact(ctx, "workspace", "workflow", 1)
	if err != nil || artifact.ContentHash != envelope.ContentHash {
		t.Fatalf("artifact read: %+v %v", artifact, err)
	}
	candidate, err := store.GetCandidate(ctx, "workspace", "workflow", envelope.ContentHash)
	if err != nil || candidate.Payload.Team.LeadAgentID != "lead" {
		t.Fatalf("candidate read: %+v %v", candidate, err)
	}
	candidateArtifact, err := store.GetCandidateArtifact(ctx, "workspace", "workflow", 1, envelope.ContentHash)
	if err != nil || candidateArtifact.ContentHash != artifact.ContentHash {
		t.Fatalf("candidate artifact: %+v %v", candidateArtifact, err)
	}
	dependencies, err := store.ListDependencies(ctx, "workspace", "workflow", 1)
	if err != nil || len(dependencies) != 2 || dependencies[0].OwnerID != "a" || dependencies[1].OwnerID != "z" {
		t.Fatalf("dependency order or workspace leaked: %+v %v", dependencies, err)
	}
	for _, request := range []struct {
		workspace, workflow string
		version             int
	}{{"foreign", "workflow", 1}, {"workspace", "workflow", 2}, {"workspace", "missing", 1}} {
		if _, err := store.GetArtifact(ctx, request.workspace, request.workflow, request.version); !errors.Is(err, ErrNotFound) {
			t.Fatalf("artifact scope: %+v %v", request, err)
		}
		if _, err := store.GetCandidateArtifact(ctx, request.workspace, request.workflow, request.version, envelope.ContentHash); !errors.Is(err, ErrCandidateNotFound) {
			t.Fatalf("candidate scope: %+v %v", request, err)
		}
	}
	if _, err := store.GetCandidate(ctx, "foreign", "workflow", envelope.ContentHash); !errors.Is(err, ErrCandidateNotFound) {
		t.Fatalf("candidate crossed workspace: %v", err)
	}
	if has, err := store.HasHistory(ctx, "workspace", "workflow"); err != nil || !has {
		t.Fatalf("frozen history missing: %v %v", has, err)
	}
	if has, err := store.HasHistory(ctx, "foreign", "workflow"); err != nil || has {
		t.Fatalf("foreign history leaked: %v %v", has, err)
	}
}

func TestCandidateReaderRejectsEnvelopeStoredUnderDifferentIdentity(t *testing.T) {
	pool, store, envelope := artifactReadFixture(t)
	if _, err := pool.Exec(t.Context(), `UPDATE weave_team_workflow_candidates SET workspace_id='foreign'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetCandidate(t.Context(), "foreign", "workflow", envelope.ContentHash); !errors.Is(err, ErrCandidateInvalid) {
		t.Fatalf("candidate envelope scope mismatch accepted: %v", err)
	}
	if _, err := store.GetCandidateArtifact(t.Context(), "foreign", "workflow", 1, envelope.ContentHash); !errors.Is(err, ErrCandidateInvalid) {
		t.Fatalf("artifact envelope scope mismatch accepted: %v", err)
	}
}

func TestArtifactAdmissionIsIdempotentAndShowsCurrentBlockReason(t *testing.T) {
	_, store, _ := artifactReadFixture(t)
	ctx := t.Context()
	state, err := store.ReadAdmission(ctx, "workspace", "workflow", 1)
	if err != nil || state.Blocked || state.LatestAuditAt != nil || state.LatestBlockReason != nil {
		t.Fatalf("initial admission: %+v %v", state, err)
	}
	change := AdmissionChange{WorkspaceID: "workspace", WorkflowID: "workflow", WorkflowVersion: 1, IdempotencyKey: "block", OperatorID: "operator", Reason: "Review required", DesiredBlocked: true}
	for range 2 {
		result, err := store.SetBlocked(ctx, change)
		if err != nil || !result.Changed {
			t.Fatalf("block/replay: %+v %v", result, err)
		}
	}
	state, err = store.ReadAdmission(ctx, "workspace", "workflow", 1)
	if err != nil || !state.Blocked || state.LatestBlockReason == nil || *state.LatestBlockReason != change.Reason || state.LatestAuditAt == nil {
		t.Fatalf("blocked view: %+v %v", state, err)
	}
	change.Reason = "Changed request"
	if _, err := store.SetBlocked(ctx, change); !errors.Is(err, ErrAdmissionIdempotencyConflict) {
		t.Fatalf("conflicting admission replay accepted: %v", err)
	}
	change.IdempotencyKey = "unblock"
	change.DesiredBlocked = false
	if _, err := store.SetBlocked(ctx, change); err != nil {
		t.Fatal(err)
	}
	state, err = store.ReadAdmission(ctx, "workspace", "workflow", 1)
	if err != nil || state.Blocked || state.LatestBlockReason != nil || state.LatestAuditAt == nil {
		t.Fatalf("cleared view retained block reason: %+v %v", state, err)
	}
	audits, err := store.ListAdmissionAudit(ctx, "workspace", "workflow", 1)
	if err != nil || len(audits) != 2 {
		t.Fatalf("admission audit: %+v %v", audits, err)
	}
	if _, err := store.ReadAdmission(ctx, "foreign", "workflow", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign admission accepted: %v", err)
	}
	attempt := FixedWorkflowAdmissionDenialAttempt{WorkspaceID: "workspace", WorkflowID: "workflow", WorkflowVersion: 1, TriggerType: "manual", AdmissionAttemptKey: "denied", ReasonCode: FixedWorkflowAdmissionVersionBlocked}
	first, err := store.RecordFixedWorkflowAdmissionDenial(ctx, attempt)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.RecordFixedWorkflowAdmissionDenial(ctx, attempt)
	if err != nil || first.ReasonCode != second.ReasonCode || !first.DecidedAt.Equal(second.DecidedAt) {
		t.Fatalf("denial replay changed audit: %+v %+v %v", first, second, err)
	}
}

func TestArtifactHistoryIncludesCandidateBeforePublication(t *testing.T) {
	pool, store, _ := artifactReadFixture(t)
	if _, err := pool.Exec(context.Background(), `DELETE FROM weave_published_artifact_contents`); err != nil {
		t.Fatal(err)
	}
	if has, err := store.HasHistory(t.Context(), "workspace", "workflow"); err != nil || !has {
		t.Fatalf("candidate history missing: %v %v", has, err)
	}
}
