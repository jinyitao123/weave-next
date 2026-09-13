package publicationservice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/publication"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

func loadReceipt(ctx context.Context, tx pgx.Tx, workspaceID, requestID, operation, digest string, result any) (bool, error) {
	var storedOperation, storedDigest string
	var encodedSubject, encodedReceipt []byte
	err := tx.QueryRow(ctx, `SELECT operation,actor_subject,request_digest,receipt FROM weave_kernel_publication_requests WHERE workspace_id=$1 AND request_id=$2`, workspaceID, requestID).
		Scan(&storedOperation, &encodedSubject, &storedDigest, &encodedReceipt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var owner execution.Subject
	if err = json.Unmarshal(encodedSubject, &owner); err != nil {
		return false, err
	}
	subject, err := execution.RequireSubject(ctx, workspaceID)
	if err != nil {
		return false, err
	}
	if owner != subject {
		return false, execution.ErrSubjectMismatch
	}
	if storedOperation != operation || storedDigest != digest {
		return false, publication.ErrRequestConflict
	}
	return true, json.Unmarshal(encodedReceipt, result)
}

func saveReceipt(ctx context.Context, tx pgx.Tx, operation, requestID, digest string, subject execution.Subject, receipt any) error {
	encodedSubject, err := json.Marshal(subject)
	if err != nil {
		return err
	}
	encodedReceipt, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO weave_kernel_publication_requests(workspace_id,request_id,operation,actor_subject,request_digest,receipt) VALUES($1,$2,$3,$4,$5,$6)`,
		subject.WorkspaceID, requestID, operation, string(encodedSubject), digest, string(encodedReceipt))
	return err
}

func insertRevision(ctx context.Context, tx pgx.Tx, facts workflow.Publication) error {
	artifact := facts.Artifact
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "frozen-revision:"+facts.WorkspaceID+":"+facts.WorkflowID); err != nil {
		return err
	}
	var priorHash string
	err := tx.QueryRow(ctx, `SELECT content_hash FROM weave_published_artifact_contents WHERE workspace_id=$1 AND workflow_id=$2 AND workflow_version=$3`, facts.WorkspaceID, facts.WorkflowID, facts.WorkflowVersion).Scan(&priorHash)
	if err == nil {
		if priorHash != artifact.ContentHash {
			return publication.ErrRequestConflict
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO weave_published_artifact_contents(workspace_id,workflow_id,workflow_version,artifact_schema_version,canonicalization_algorithm,canonicalization_version,hash_algorithm,content_hash,payload) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		facts.WorkspaceID, facts.WorkflowID, facts.WorkflowVersion, artifact.ArtifactSchemaVersion, artifact.CanonicalizationAlgorithm, artifact.CanonicalizationVersion, artifact.HashAlgorithm, artifact.ContentHash, string(artifact.Payload)); err != nil {
		return err
	}
	for _, d := range facts.Dependencies {
		if _, err = tx.Exec(ctx, `INSERT INTO weave_team_workflow_dependencies(workspace_id,workflow_id,workflow_version,owner_type,owner_id,owner_agent_version,dependency_type,dependency_key,dependency_version,content_hash) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			facts.WorkspaceID, facts.WorkflowID, facts.WorkflowVersion, d.OwnerType, d.OwnerID, d.OwnerAgentVersion, d.DependencyType, d.DependencyKey, d.DependencyVersion, d.ContentHash); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO weave_workflow_version_admission_statuses(workspace_id,workflow_id,workflow_version,blocked) VALUES($1,$2,$3,false)`, facts.WorkspaceID, facts.WorkflowID, facts.WorkflowVersion)
	return err
}

func saveCandidate(ctx context.Context, tx pgx.Tx, envelope frozen.ArtifactEnvelopeV1, dependencies []workflow.TeamWorkflowDependency) error {
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	encodedDependencies, err := json.Marshal(dependencies)
	if err != nil {
		return err
	}
	subject, err := execution.RequireSubject(ctx, envelope.WorkspaceID)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO weave_team_workflow_candidates(workspace_id,workflow_id,workflow_version,content_hash,envelope_json,dependencies_json,expected_updated_at,created_by,created_at)
	VALUES($1,$2,$3,$4,$5,$6,$7,$8,$7) ON CONFLICT(workspace_id,workflow_id,workflow_version,content_hash) DO NOTHING`,
		envelope.WorkspaceID, envelope.WorkflowID, envelope.WorkflowVersion, envelope.ContentHash, string(encoded), string(encodedDependencies), time.Now().UTC(), subject.Digest())
	if err != nil {
		return err
	}
	var storedEnvelope, storedDependencies []byte
	if err = tx.QueryRow(ctx, `SELECT envelope_json,dependencies_json FROM weave_team_workflow_candidates WHERE workspace_id=$1 AND workflow_id=$2 AND workflow_version=$3 AND content_hash=$4`, envelope.WorkspaceID, envelope.WorkflowID, envelope.WorkflowVersion, envelope.ContentHash).Scan(&storedEnvelope, &storedDependencies); err != nil {
		return err
	}
	var stored frozen.ArtifactEnvelopeV1
	if err = json.Unmarshal(storedEnvelope, &stored); err != nil {
		return publication.ErrRequestConflict
	}
	storedFacts, err := workflow.PublicationFromEnvelope(stored)
	if err != nil {
		return publication.ErrRequestConflict
	}
	requestedFacts, err := workflow.PublicationFromEnvelope(envelope)
	if err != nil {
		return err
	}
	left, _ := json.Marshal(storedFacts.Artifact)
	right, _ := json.Marshal(requestedFacts.Artifact)
	left, err = frozen.CanonicalizeJSON(left)
	if err != nil {
		return publication.ErrRequestConflict
	}
	right, err = frozen.CanonicalizeJSON(right)
	if err != nil {
		return err
	}
	if !bytes.Equal(left, right) {
		return publication.ErrRequestConflict
	}
	rebuilt, _ := json.Marshal(storedFacts.Dependencies)
	rebuilt, err = frozen.CanonicalizeJSON(rebuilt)
	if err != nil {
		return publication.ErrRequestConflict
	}
	actual, err := frozen.CanonicalizeJSON(storedDependencies)
	if err != nil || !bytes.Equal(rebuilt, actual) {
		return publication.ErrRequestConflict
	}
	return nil
}

func parentDeadline(ctx context.Context, tx pgx.Tx, subject execution.Subject, parentTaskID string, deadline *time.Time) (*time.Time, error) {
	if deadline != nil {
		copy := deadline.UTC()
		deadline = &copy
	}
	if parentTaskID == "" {
		return deadline, nil
	}
	var ownerRaw []byte
	var status string
	var parentDeadline *time.Time
	err := tx.QueryRow(ctx, `SELECT actor_subject,status,deadline_at FROM weave_task_queue WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, subject.WorkspaceID, parentTaskID).Scan(&ownerRaw, &status, &parentDeadline)
	if err != nil {
		return nil, err
	}
	var owner execution.Subject
	if err = json.Unmarshal(ownerRaw, &owner); err != nil {
		return nil, err
	}
	if owner != subject {
		return nil, execution.ErrSubjectMismatch
	}
	if status != "running" && status != "queued" && status != "dispatched" {
		return nil, errors.New("candidate execution parent has stopped")
	}
	if parentDeadline != nil && (deadline == nil || parentDeadline.Before(*deadline)) {
		deadline = parentDeadline
	}
	return deadline, nil
}
