// Package publicationservice implements the kernel publication/admission unit
// of work. Its database connection and transactions never cross the service
// boundary; product catalog activation remains a separate server operation.
package publicationservice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/publication"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

// Authority resolves current resource authorization and exact-version proof
// snapshots through owner-provided ports. It must reject an unauthorized actor
// before returning proofs. The kernel supplies identity/graph/trigger itself and
// runs the ordered validator; a source cannot replace the requested definition.
type Authority interface {
	Authorize(context.Context, string, frozen.ArtifactEnvelopeV1) (machine.ValidationContext, error)
}

// CredentialAuthority is supplied by the product owner; no secret, grant, or
// storage transaction crosses this port.
type CredentialAuthority interface {
	AuthorizeCredential(context.Context, frozen.ArtifactEnvelopeV1, frozen.CredentialReference) error
}

type Service struct {
	pool      *pgxpool.Pool
	authority Authority
}

func Open(ctx context.Context, databaseURL string, authority Authority) (*Service, error) {
	if databaseURL == "" || authority == nil {
		return nil, errors.New("publication database and authority are required")
	}
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	return &Service{pool: pool, authority: authority}, nil
}

func (s *Service) Close() {
	if s != nil && s.pool != nil {
		s.pool.Close()
	}
}

var _ publication.Service = (*Service)(nil)

func (s *Service) Publish(ctx context.Context, request publication.PublishRequest) (publication.PublishReceipt, error) {
	digest, err := request.Fingerprint(ctx)
	if err != nil {
		return publication.PublishReceipt{}, err
	}
	facts, err := s.validate(ctx, "publish", request.Candidate)
	if err != nil {
		return publication.PublishReceipt{}, err
	}
	tx, err := s.begin(ctx, request.Candidate.WorkspaceID, request.RequestID)
	if err != nil {
		return publication.PublishReceipt{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var receipt publication.PublishReceipt
	if found, err := loadReceipt(ctx, tx, request.Candidate.WorkspaceID, request.RequestID, "publish", digest, &receipt); err != nil {
		return receipt, err
	} else if found {
		return receipt, receipt.Verify(ctx, request)
	}
	if err = insertRevision(ctx, tx, facts); err != nil {
		return receipt, err
	}
	subject, _ := execution.RequireSubject(ctx, request.Candidate.WorkspaceID)
	receipt = publication.PublishReceipt{Version: publication.ContractVersion, RequestID: request.RequestID,
		RequestDigest: digest, Subject: subject, Revision: publication.CandidateRevision(request.Candidate)}
	if err = saveReceipt(ctx, tx, "publish", receipt.RequestID, digest, subject, receipt); err != nil {
		return publication.PublishReceipt{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return publication.PublishReceipt{}, err
	}
	return receipt, nil
}

func (s *Service) AdmitCandidate(ctx context.Context, request publication.CandidateRunRequest) (publication.AdmissionReceipt, error) {
	digest, err := request.Fingerprint(ctx)
	if err != nil {
		return publication.AdmissionReceipt{}, err
	}
	facts, err := s.validate(ctx, "candidate_run", request.Candidate)
	if err != nil {
		return publication.AdmissionReceipt{}, err
	}
	tx, err := s.begin(ctx, request.Candidate.WorkspaceID, request.RequestID)
	if err != nil {
		return publication.AdmissionReceipt{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var receipt publication.AdmissionReceipt
	if found, err := loadReceipt(ctx, tx, request.Candidate.WorkspaceID, request.RequestID, "candidate_run", digest, &receipt); err != nil {
		return receipt, err
	} else if found {
		return receipt, receipt.Verify(ctx, request)
	}
	if err = saveCandidate(ctx, tx, request.Candidate, facts.Dependencies); err != nil {
		return receipt, err
	}
	subject, _ := execution.RequireSubject(ctx, request.Candidate.WorkspaceID)
	deadline, err := parentDeadline(ctx, tx, subject, request.ParentTaskID, request.DeadlineAt)
	if err != nil {
		return receipt, err
	}
	if deadline != nil && !deadline.After(time.Now()) {
		return receipt, errors.New("candidate execution deadline exceeded")
	}
	payload, _ := frozen.DecodeArtifactEnvelopeV1(request.Candidate)
	runID := "run-" + uuid.NewString()
	decision, _ := json.Marshal(map[string]any{"schema_version": 1, "team_active": true, "workflow_active": true, "workers_enabled": true, "version_blocked": false, "decided_at": time.Now().UTC().Format(time.RFC3339Nano)})
	trigger, _ := json.Marshal(map[string]any{"schema_version": 1, "type": "api", "source_ref": request.SourceRef})
	fixed := snapshot.TeamRunSnapshot{Subject: subject, RunID: runID, WorkspaceID: subject.WorkspaceID, TeamID: payload.Team.TeamID,
		SnapshotSchemaVersion: 2, Mode: "fixed_workflow", WorkflowID: request.Candidate.WorkflowID, WorkflowVersion: request.Candidate.WorkflowVersion,
		ArtifactWorkflowID: request.Candidate.WorkflowID, ArtifactWorkflowVersion: request.Candidate.WorkflowVersion,
		CandidateContentHash: request.Candidate.ContentHash, AdmissionDecision: decision, TriggerSourceV2: trigger,
		RunAssociations: json.RawMessage(`{"schema_version":1,"parent_run_id":null,"source_snapshot_id":null,"task_group_id":null}`)}
	if _, err = snapshot.NewStore(s.pool).CreateTx(ctx, tx, fixed); err != nil {
		return receipt, err
	}
	task := &taskqueue.Task{ID: "task-" + uuid.NewString(), Subject: subject, WorkspaceID: subject.WorkspaceID,
		IdentityKind: taskqueue.IdentityTeamWorkflow, IdentitySchemaVersion: 2, Kind: "team_workflow", Source: "api",
		WorkflowID: request.Candidate.WorkflowID, WorkflowVersion: request.Candidate.WorkflowVersion, RunSnapshotID: runID,
		ParentTaskID: request.ParentTaskID, ContextKey: request.RequestID, DeadlineAt: deadline, OutcomeSensitive: true, Payload: append([]byte(nil), request.Input...)}
	if err = taskqueue.New(s.pool, nil, time.Minute).EnqueueTx(ctx, tx, task); err != nil {
		return receipt, err
	}
	receipt = publication.AdmissionReceipt{Version: publication.ContractVersion, RequestID: request.RequestID, RequestDigest: digest,
		Subject: subject, Revision: publication.CandidateRevision(request.Candidate), TaskID: task.ID, RunID: runID, RunSnapshotID: runID}
	if err = saveReceipt(ctx, tx, "candidate_run", request.RequestID, digest, subject, receipt); err != nil {
		return publication.AdmissionReceipt{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return publication.AdmissionReceipt{}, err
	}
	return receipt, nil
}

func (s *Service) begin(ctx context.Context, workspaceID, requestID string) (pgx.Tx, error) {
	if s == nil || s.pool == nil || s.authority == nil {
		return nil, errors.New("publication service unavailable")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	_, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "kernel-publication:"+workspaceID+":"+requestID)
	if err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return tx, nil
}

func (s *Service) validate(ctx context.Context, operation string, envelope frozen.ArtifactEnvelopeV1) (workflow.Publication, error) {
	if s == nil || s.pool == nil || s.authority == nil {
		return workflow.Publication{}, errors.New("publication service unavailable")
	}
	payload, err := frozen.DecodeArtifactEnvelopeV1(envelope)
	if err != nil {
		return workflow.Publication{}, err
	}
	proofs, err := s.authority.Authorize(ctx, operation, envelope)
	if err != nil {
		return workflow.Publication{}, err
	}
	trigger, triggerReport := machine.DecodeTriggerConfigV1(payload.TriggerConfig)
	graph, graphReport := machine.DecodeGraphDefinitionV1(payload.GraphDefinition)
	if (triggerReport != nil && len(triggerReport.Issues) > 0) || (graphReport != nil && len(graphReport.Issues) > 0) {
		return workflow.Publication{}, errors.New("candidate graph or trigger is invalid")
	}
	proofs.WorkspaceID, proofs.TeamID = envelope.WorkspaceID, payload.Team.TeamID
	proofs.Lead = machine.AgentVersionKey{AgentID: payload.Team.LeadAgentID, AgentVersion: payload.Team.LeadAgentVersion}
	proofs.Trigger, proofs.Graph = trigger, graph
	if report := machine.Validate(proofs); len(report.Issues) > 0 {
		return workflow.Publication{}, fmt.Errorf("candidate validation: %s", report.Issues[0].Code)
	}
	for _, bundle := range payload.Bundles {
		for _, ref := range bundle.Credentials {
			if err = s.authorizeCredential(ctx, envelope, ref); err != nil {
				return workflow.Publication{}, err
			}
		}
	}
	for _, target := range payload.DeliveryTargets {
		if err = s.authorizeCredential(ctx, envelope, target.AccessRef); err != nil {
			return workflow.Publication{}, err
		}
		for _, binding := range target.CredentialBindings {
			if err = s.authorizeCredential(ctx, envelope, binding.CredentialRef); err != nil {
				return workflow.Publication{}, err
			}
		}
	}
	return workflow.PublicationFromEnvelope(envelope)
}

func (s *Service) authorizeCredential(ctx context.Context, envelope frozen.ArtifactEnvelopeV1, ref frozen.CredentialReference) error {
	if authority, ok := s.authority.(CredentialAuthority); ok {
		return authority.AuthorizeCredential(ctx, envelope, ref)
	}
	return credentials.AuthorizeReference(ctx, ref)
}
