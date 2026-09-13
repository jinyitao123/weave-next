package publicationservice

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"time"

	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/kernel/publication"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

var _ publication.PublishedService = (*Service)(nil)

type PublishedAuthority interface {
	AuthorizePublished(context.Context, publication.PublishedRunRequest, frozen.ArtifactEnvelopeV1) error
}

// AdmitPublished serializes the kernel version block and execution admission
// on the same status row. Product authorization is refreshed before this local
// transaction; it is not a claim of atomicity across independent data owners.
func (s *Service) AdmitPublished(ctx context.Context, request publication.PublishedRunRequest) (publication.AdmissionReceipt, error) {
	digest, err := request.Fingerprint(ctx)
	if err != nil {
		return publication.AdmissionReceipt{}, err
	}
	if s == nil || s.pool == nil || s.authority == nil {
		return publication.AdmissionReceipt{}, errors.New("published admission unavailable")
	}
	artifact, err := workflow.NewArtifactStore(s.pool, nil).GetArtifact(ctx, request.Revision.WorkspaceID, request.Revision.WorkflowID, request.Revision.WorkflowVersion)
	if err != nil {
		return publication.AdmissionReceipt{}, err
	}
	envelope := frozen.ArtifactEnvelopeV1{WorkspaceID: artifact.WorkspaceID, WorkflowID: artifact.WorkflowID, WorkflowVersion: artifact.WorkflowVersion,
		ArtifactSchemaVersion: artifact.ArtifactSchemaVersion, CanonicalizationAlgorithm: artifact.CanonicalizationAlgorithm,
		CanonicalizationVersion: artifact.CanonicalizationVersion, HashAlgorithm: artifact.HashAlgorithm, ContentHash: artifact.ContentHash, Payload: artifact.Payload}
	if publication.CandidateRevision(envelope) != request.Revision {
		return publication.AdmissionReceipt{}, publication.ErrRequestConflict
	}
	payload, err := frozen.DecodeArtifactEnvelopeV1(envelope)
	if err != nil {
		return publication.AdmissionReceipt{}, err
	}
	fences, err := s.captureResourceFences(ctx, envelope)
	if err != nil {
		return publication.AdmissionReceipt{}, err
	}
	authority, ok := s.authority.(PublishedAuthority)
	if !ok {
		return publication.AdmissionReceipt{}, errors.New("published authority unavailable")
	}
	if err = authority.AuthorizePublished(ctx, request, envelope); err != nil {
		return publication.AdmissionReceipt{}, err
	}
	for _, bundle := range payload.Bundles {
		for _, ref := range bundle.Credentials {
			if err = s.authorizeCredential(ctx, envelope, ref); err != nil {
				return publication.AdmissionReceipt{}, err
			}
		}
	}
	for _, target := range payload.DeliveryTargets {
		if err = s.authorizeCredential(ctx, envelope, target.AccessRef); err != nil {
			return publication.AdmissionReceipt{}, err
		}
		for _, binding := range target.CredentialBindings {
			if err = s.authorizeCredential(ctx, envelope, binding.CredentialRef); err != nil {
				return publication.AdmissionReceipt{}, err
			}
		}
	}
	graph, report := machine.DecodeGraphDefinitionV1(payload.GraphDefinition)
	if report != nil && len(report.Issues) > 0 {
		return publication.AdmissionReceipt{}, errors.New("frozen published graph is invalid")
	}
	contract := &deliverable.DeliveryContract{Version: 1, Coverage: "incomplete", Limitations: []string{"explicit_user_delivery_scope_missing"}}
	if graph.DeliveryContract != nil {
		contract = deliverable.CloneDeliveryContract(graph.DeliveryContract)
	}
	if request.DeliveryContract != nil {
		contract = deliverable.CloneDeliveryContract(request.DeliveryContract)
	}
	contract.Output = deliverable.OutputRequirement{Type: string(graph.OutputContract.Type), Schema: graph.OutputContract.Schema}
	if err = deliverable.ValidateDeliveryContract(contract); err != nil {
		return publication.AdmissionReceipt{}, err
	}
	tx, err := s.begin(ctx, request.Revision.WorkspaceID, request.RequestID)
	if err != nil {
		return publication.AdmissionReceipt{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err = checkResourceFencesTx(ctx, tx, request.Revision.WorkspaceID, fences); err != nil {
		return publication.AdmissionReceipt{}, err
	}
	var blocked bool
	if err = tx.QueryRow(ctx, `SELECT blocked FROM weave_workflow_version_admission_statuses WHERE workspace_id=$1 AND workflow_id=$2 AND workflow_version=$3 FOR SHARE`, request.Revision.WorkspaceID, request.Revision.WorkflowID, request.Revision.WorkflowVersion).Scan(&blocked); err != nil {
		return publication.AdmissionReceipt{}, err
	}
	var receipt publication.AdmissionReceipt
	if previous, closed, err := loadPublishedReceipt(ctx, tx, request, digest); err != nil {
		return receipt, err
	} else if closed {
		return receipt, publication.ErrAdmissionClosed
	} else if previous != nil {
		return *previous, nil
	}
	if blocked {
		return publication.AdmissionReceipt{}, &workflow.FixedWorkflowAdmissionDenial{ReasonCode: workflow.FixedWorkflowAdmissionVersionBlocked, WorkflowVersion: request.Revision.WorkflowVersion}
	}
	subject, _ := execution.RequireSubject(ctx, request.Revision.WorkspaceID)
	deadline, err := parentDeadline(ctx, tx, subject, request.ParentTaskID, request.DeadlineAt)
	if err != nil {
		return receipt, err
	}
	if deadline != nil && !deadline.After(time.Now()) {
		return receipt, errors.New("published execution deadline exceeded")
	}
	decision, _ := json.Marshal(map[string]any{"schema_version": 1, "team_active": true, "workflow_active": true, "workers_enabled": true, "version_blocked": false, "decided_at": time.Now().UTC().Format(time.RFC3339Nano)})
	trigger := map[string]any{"schema_version": 1, "type": request.Trigger.Type, "source_ref": request.Trigger.SourceRef}
	if request.Trigger.OccurrenceKey != "" {
		trigger["occurrence_key"] = request.Trigger.OccurrenceKey
	}
	triggerJSON, _ := json.Marshal(trigger)
	fixed := snapshot.TeamRunSnapshot{Subject: subject, SourceRef: request.Trigger.SourceRef, RunID: request.RunID, WorkspaceID: subject.WorkspaceID, TeamID: payload.Team.TeamID, ProjectID: request.ProjectID,
		SnapshotSchemaVersion: 2, Mode: "fixed_workflow", WorkflowID: request.Revision.WorkflowID, WorkflowVersion: request.Revision.WorkflowVersion,
		ArtifactWorkflowID: request.Revision.WorkflowID, ArtifactWorkflowVersion: request.Revision.WorkflowVersion,
		AdmissionDecision: decision, TriggerSourceV2: triggerJSON,
		RunAssociations: json.RawMessage(`{"schema_version":1,"parent_run_id":null,"source_snapshot_id":null,"task_group_id":null}`)}
	if _, err = snapshot.NewStore(s.pool).CreateTx(ctx, tx, fixed); err != nil {
		return receipt, err
	}
	if _, err = deliverable.New(s.pool).FreezeContractTx(ctx, tx, deliverable.ContractBinding{WorkspaceID: subject.WorkspaceID, RunSnapshotID: request.RunID, InputRevisionID: request.InputVersion,
		WorkflowID: request.Revision.WorkflowID, WorkflowVersion: request.Revision.WorkflowVersion, PublishedDigest: request.Revision.ContentHash, Contract: contract}); err != nil {
		return receipt, err
	}
	source := request.Trigger.Type
	if source == "conversation_explicit" {
		source = "session"
	}
	task := &taskqueue.Task{SourceRef: request.Trigger.SourceRef, ID: request.TaskID, Subject: subject, WorkspaceID: subject.WorkspaceID, ProjectID: request.ProjectID,
		IdentityKind: taskqueue.IdentityTeamWorkflow, IdentitySchemaVersion: 2, Kind: "team_workflow", Source: source,
		WorkflowID: request.Revision.WorkflowID, WorkflowVersion: request.Revision.WorkflowVersion, RunSnapshotID: request.RunID,
		ParentTaskID: request.ParentTaskID, ContextKey: request.ContextKey, DeadlineAt: deadline, OutcomeSensitive: true, Payload: append([]byte(nil), request.Input...)}
	if err = taskqueue.New(s.pool, nil, time.Minute).EnqueueTx(ctx, tx, task); err != nil {
		return receipt, err
	}
	receipt = publication.AdmissionReceipt{Version: publication.ContractVersion, RequestID: request.RequestID, RequestDigest: digest,
		Subject: subject, Revision: request.Revision, TaskID: request.TaskID, RunID: request.RunID, RunSnapshotID: request.RunID}
	if err = saveReceipt(ctx, tx, "published_run", request.RequestID, digest, subject, receipt); err != nil {
		return publication.AdmissionReceipt{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return publication.AdmissionReceipt{}, err
	}
	return receipt, nil
}

func (s *Service) ClosePublished(ctx context.Context, request publication.PublishedRunRequest) (*publication.AdmissionReceipt, error) {
	digest, err := request.Fingerprint(ctx)
	if err != nil {
		return nil, err
	}
	tx, err := s.begin(ctx, request.Revision.WorkspaceID, request.RequestID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	previous, closed, err := loadPublishedReceipt(ctx, tx, request, digest)
	if err != nil {
		return nil, err
	}
	if previous != nil || closed {
		return previous, nil
	}
	subject, _ := execution.RequireSubject(ctx, request.Revision.WorkspaceID)
	marker := publication.PublishReceipt{Version: publication.ContractVersion, RequestID: request.RequestID, RequestDigest: digest, Subject: subject, Revision: request.Revision}
	if err = saveReceipt(ctx, tx, "published_closed", request.RequestID, digest, subject, marker); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return nil, nil
}

func loadPublishedReceipt(ctx context.Context, tx pgx.Tx, request publication.PublishedRunRequest, digest string) (*publication.AdmissionReceipt, bool, error) {
	var operation string
	err := tx.QueryRow(ctx, `SELECT operation FROM weave_kernel_publication_requests WHERE workspace_id=$1 AND request_id=$2`, request.Revision.WorkspaceID, request.RequestID).Scan(&operation)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if operation == "published_closed" {
		var marker publication.PublishReceipt
		if _, err = loadReceipt(ctx, tx, request.Revision.WorkspaceID, request.RequestID, operation, digest, &marker); err != nil {
			return nil, false, err
		}
		subject, _ := execution.RequireSubject(ctx, request.Revision.WorkspaceID)
		if marker.Version != publication.ContractVersion || marker.RequestID != request.RequestID || marker.RequestDigest != digest || marker.Subject != subject || marker.Revision != request.Revision {
			return nil, false, publication.ErrInvalidReceipt
		}
		return nil, true, nil
	}
	var receipt publication.AdmissionReceipt
	if _, err = loadReceipt(ctx, tx, request.Revision.WorkspaceID, request.RequestID, "published_run", digest, &receipt); err != nil {
		return nil, false, err
	}
	if err = receipt.VerifyPublished(ctx, request); err != nil {
		return nil, false, err
	}
	return &receipt, false, nil
}
