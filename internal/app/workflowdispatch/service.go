// Package workflowdispatch provides the entrypoint-independent application
// service for admitting and durably enqueueing a published workflow run.
package workflowdispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

var (
	// ErrUnavailable indicates that the shared admission service is not wired.
	ErrUnavailable = errors.New("workflow dispatch service unavailable")
	// ErrInvalidRequest indicates incomplete or inconsistent caller facts.
	ErrInvalidRequest = errors.New("invalid workflow dispatch request")
)

// AdmissionRequest contains every identity that previously came from an HTTP
// context. The caller owns the transaction and may perform entrypoint-specific
// attribution checks between PrepareTx and PersistTx.
type AdmissionRequest struct {
	WorkspaceID     string
	WorkflowID      string
	WorkflowVersion *int
	SourceRef       string
	TriggerType     string
	RunID           string
}

// PersistRequest contains the immutable queue and delivery facts for one run.
type PersistRequest struct {
	ProjectID            string
	TaskID               string
	TaskSource           string
	ContextKey           string
	Payload              json.RawMessage
	InputRevisionID      string
	DeliveryContract     json.RawMessage
	SkipDeliveryContract bool
}

// Receipt is the stable result of a successful caller-owned transaction.
type Receipt struct {
	RunID           string
	WorkflowID      string
	WorkflowVersion int
	TaskID          string
	ProjectID       string
	Snapshot        snapshot.TeamRunSnapshot
}

// PreparedRun is an admitted but not yet persisted workflow snapshot. Its
// fields remain private so callers cannot replace frozen workflow identity.
type PreparedRun struct {
	snapshot snapshot.TeamRunSnapshot
}

// Snapshot returns a copy for entrypoint-specific attribution checks.
func (prepared PreparedRun) Snapshot() snapshot.TeamRunSnapshot {
	return prepared.snapshot
}

// Service joins existing workflow admission, snapshot, delivery and queue
// stores without owning their transaction lifecycle.
type Service struct {
	Workflow     *workflow.Store
	Snapshots    *snapshot.Store
	Tasks        *taskqueue.Store
	Deliverables *deliverable.Store
}

// PrepareTx validates a published workflow and freezes its run identity in
// memory. No database row is written before the caller finishes attribution.
func (service *Service) PrepareTx(
	ctx context.Context,
	tx pgx.Tx,
	request AdmissionRequest,
) (PreparedRun, error) {
	if service == nil || service.Workflow == nil {
		return PreparedRun{}, fmt.Errorf("%w: dependencies are incomplete", ErrUnavailable)
	}
	if request.WorkspaceID == "" || request.WorkflowID == "" || request.SourceRef == "" || request.RunID == "" {
		return PreparedRun{}, fmt.Errorf("%w: identity is incomplete", ErrInvalidRequest)
	}
	admitted, err := service.Workflow.AdmitWorkflowManualRunTx(ctx, tx, workflow.WorkflowManualRunAdmissionRequest{
		WorkspaceID:     request.WorkspaceID,
		WorkflowID:      request.WorkflowID,
		WorkflowVersion: request.WorkflowVersion,
		SourceRef:       request.SourceRef,
		TriggerType:     request.TriggerType,
	})
	if err != nil {
		return PreparedRun{}, err
	}
	admitted.RunID = request.RunID
	expectedTrigger := request.TriggerType
	if expectedTrigger == "" {
		expectedTrigger = "manual"
	}
	if err := ValidateSnapshot(admitted, request.WorkspaceID, request.WorkflowID, request.SourceRef, expectedTrigger); err != nil {
		return PreparedRun{}, err
	}
	return PreparedRun{snapshot: admitted}, nil
}

// Lead returns the frozen lead identity from the admitted publication.
func (service *Service) Lead(ctx context.Context, admitted snapshot.TeamRunSnapshot) (string, int, error) {
	if service == nil || service.Workflow == nil {
		return "", 0, ErrUnavailable
	}
	artifact, err := service.Workflow.GetArtifact(ctx, admitted.WorkspaceID, admitted.WorkflowID, admitted.WorkflowVersion)
	if err != nil {
		return "", 0, fmt.Errorf("read admitted workflow artifact: %w", err)
	}
	payload, err := frozen.DecodeArtifactEnvelopeV1(frozen.ArtifactEnvelopeV1{
		WorkspaceID:               artifact.WorkspaceID,
		WorkflowID:                artifact.WorkflowID,
		WorkflowVersion:           artifact.WorkflowVersion,
		ArtifactSchemaVersion:     artifact.ArtifactSchemaVersion,
		CanonicalizationAlgorithm: artifact.CanonicalizationAlgorithm,
		CanonicalizationVersion:   artifact.CanonicalizationVersion,
		HashAlgorithm:             artifact.HashAlgorithm,
		ContentHash:               artifact.ContentHash,
		Payload:                   artifact.Payload,
	})
	if err != nil {
		return "", 0, fmt.Errorf("decode admitted workflow artifact: %w", err)
	}
	if payload.Team.WorkspaceID != admitted.WorkspaceID || payload.Team.TeamID != admitted.TeamID || payload.Team.LeadAgentID == "" {
		return "", 0, errors.New("admitted workflow artifact has mismatched lead identity")
	}
	if payload.Team.LeadAgentVersion > 0 {
		return payload.Team.LeadAgentID, int(payload.Team.LeadAgentVersion), nil
	}
	for _, bundle := range payload.Bundles {
		if bundle.Agent.AgentID == payload.Team.LeadAgentID && bundle.Agent.AgentVersion > 0 {
			return payload.Team.LeadAgentID, int(bundle.Agent.AgentVersion), nil
		}
	}
	return "", 0, errors.New("admitted workflow artifact has no frozen lead bundle")
}

// PersistTx creates the immutable run snapshot, freezes its delivery contract,
// and enqueues exactly one existing team_workflow task. It never commits tx.
func (service *Service) PersistTx(
	ctx context.Context,
	tx pgx.Tx,
	prepared PreparedRun,
	request PersistRequest,
) (Receipt, error) {
	if service == nil || service.Snapshots == nil || service.Tasks == nil ||
		(!request.SkipDeliveryContract && service.Deliverables == nil) {
		return Receipt{}, ErrUnavailable
	}
	if request.TaskID == "" || request.TaskSource == "" || len(request.Payload) == 0 {
		return Receipt{}, fmt.Errorf("%w: persistence facts are incomplete", ErrInvalidRequest)
	}
	admitted := prepared.snapshot
	admitted.ProjectID = request.ProjectID
	created, err := service.Snapshots.CreateTx(ctx, tx, admitted)
	if err != nil {
		return Receipt{}, err
	}
	if !request.SkipDeliveryContract {
		if err := service.freezeDeliveryContractTx(ctx, tx, created, request); err != nil {
			return Receipt{}, fmt.Errorf("freeze workflow delivery contract: %w", err)
		}
	}
	task := &taskqueue.Task{
		ID:                    request.TaskID,
		WorkspaceID:           created.WorkspaceID,
		ProjectID:             created.ProjectID,
		IdentityKind:          taskqueue.IdentityTeamWorkflow,
		IdentitySchemaVersion: 2,
		WorkflowID:            created.WorkflowID,
		WorkflowVersion:       created.WorkflowVersion,
		RunSnapshotID:         created.RunID,
		Source:                request.TaskSource,
		Kind:                  "team_workflow",
		ContextKey:            request.ContextKey,
		Payload:               append(json.RawMessage(nil), request.Payload...),
	}
	if err := service.Tasks.EnqueueTx(ctx, tx, task); err != nil {
		return Receipt{}, fmt.Errorf("enqueue workflow task: %w", err)
	}
	return Receipt{
		RunID: created.RunID, WorkflowID: created.WorkflowID,
		WorkflowVersion: created.WorkflowVersion, TaskID: task.ID,
		ProjectID: created.ProjectID, Snapshot: *created,
	}, nil
}

// ValidateSnapshot checks that the workflow store returned the requested
// frozen identity and trigger attribution.
func ValidateSnapshot(admitted snapshot.TeamRunSnapshot, workspaceID, workflowID, sourceRef, expectedTrigger string) error {
	if admitted.RunID == "" || admitted.WorkspaceID != workspaceID || admitted.TeamID == "" ||
		admitted.SnapshotSchemaVersion != 2 || admitted.Mode != "fixed_workflow" ||
		admitted.WorkflowID != workflowID || admitted.WorkflowVersion < 1 ||
		admitted.ArtifactWorkflowID != workflowID || admitted.ArtifactWorkflowVersion != admitted.WorkflowVersion {
		return errors.New("workflow admission returned a mismatched fixed workflow identity")
	}
	var trigger struct {
		SchemaVersion int    `json:"schema_version"`
		Type          string `json:"type"`
		SourceRef     string `json:"source_ref"`
	}
	decoder := json.NewDecoder(bytes.NewReader(admitted.TriggerSourceV2))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&trigger); err != nil {
		return fmt.Errorf("decode workflow trigger source: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("decode workflow trigger source: trailing JSON value")
	}
	if trigger.SchemaVersion != 1 || trigger.Type != expectedTrigger || trigger.SourceRef != sourceRef {
		return errors.New("workflow admission returned a mismatched trigger")
	}
	return nil
}
