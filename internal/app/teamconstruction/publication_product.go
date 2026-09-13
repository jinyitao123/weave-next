package teamconstruction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/publication"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

// ProductPublication coordinates product activation with the kernel's durable
// receipt. Its pool owns only product state; kernel calls never receive its tx.
// Keep this adapter in weave-server when the modules are extracted.
type PublicationAuthorizer func(context.Context, PublicationCommand) error

type ProductPublication struct {
	authorize PublicationAuthorizer
	requests  *pgPublicationRequests
	kernel    publication.Service
}

func NewProductPublication(pool *pgxpool.Pool, kernel publication.Service, authorize PublicationAuthorizer) *ProductPublication {
	p := &ProductPublication{requests: &pgPublicationRequests{pool: pool}, kernel: kernel, authorize: authorize}
	p.requests.activate = func(ctx context.Context, tx pgx.Tx, record PublicationRequestRecord) error {
		if p.authorize == nil {
			return errors.New("product publication authorization unavailable")
		}
		if err := p.authorize(ctx, record.Command); err != nil {
			return err
		}
		return activateWorkflowRevisionTx(ctx, tx, record)
	}
	return p
}

func (p *ProductPublication) Publish(ctx context.Context, command PublicationCommand) (PublicationRequestRecord, error) {
	if p == nil || p.requests == nil || p.requests.pool == nil {
		return PublicationRequestRecord{}, errors.New("product publication unavailable")
	}
	if p.authorize == nil {
		return PublicationRequestRecord{}, errors.New("product publication authorization unavailable")
	}
	if err := p.authorize(ctx, command); err != nil {
		return PublicationRequestRecord{}, err
	}
	return (PublicationFlow{Requests: p.requests, Kernel: p.kernel}).Publish(ctx, command)
}

// PublicationCommandForCandidate fixes the evaluated envelope and product CAS
// token before the first kernel call. The caller supplies the durable operation
// ID; retries must reuse it and the persisted command, never mint a new version.
func PublicationCommandForCandidate(requestID string, candidate *workflow.PublicationCandidate, target PublicationTarget) (PublicationCommand, error) {
	envelope, err := workflow.CandidateEnvelope(candidate)
	if err != nil {
		return PublicationCommand{}, err
	}
	if target.TeamID == "" {
		target.TeamID = candidate.Payload.Team.TeamID
	}
	if target.ExpectedAssetVersion == "" {
		target.ExpectedAssetVersion = candidate.ExpectedUpdatedAt.UTC().Format(time.RFC3339Nano)
	}
	return PublicationCommand{Target: target, Request: publication.PublishRequest{Version: publication.ContractVersion, RequestID: requestID, Candidate: envelope}}, nil
}

// activateWorkflowRevisionTx changes only the mutable product catalog. The
// kernel receipt is already verified by the request store before this hook runs.
func activateWorkflowRevisionTx(ctx context.Context, tx pgx.Tx, record PublicationRequestRecord) error {
	subject, err := execution.RequireSubject(ctx, record.Subject.WorkspaceID)
	if err != nil {
		return err
	}
	if subject != record.Subject {
		return execution.ErrSubjectMismatch
	}
	expected, err := time.Parse(time.RFC3339Nano, record.Command.Target.ExpectedAssetVersion)
	if err != nil {
		return publication.ErrInvalidRequest
	}
	ref := record.Receipt.Revision
	var workflowStatus, versionStatus, teamID string
	var publishedVersion *int
	var workflowUpdatedAt, versionUpdatedAt time.Time
	err = tx.QueryRow(ctx, `SELECT w.status,w.updated_at,w.team_id,w.published_version,v.status,v.updated_at
 FROM weave_team_workflows w JOIN weave_team_workflow_versions v
 ON v.workspace_id=w.workspace_id AND v.workflow_id=w.id
 WHERE w.workspace_id=$1 AND w.id=$2 AND v.version=$3 FOR UPDATE OF w,v`, ref.WorkspaceID, ref.WorkflowID, ref.WorkflowVersion).Scan(&workflowStatus, &workflowUpdatedAt, &teamID, &publishedVersion, &versionStatus, &versionUpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return workflow.ErrNotFound
	}
	if err != nil {
		return err
	}
	if workflowStatus == workflow.WorkflowStatusArchived {
		return workflow.ErrArchived
	}
	if teamID != record.Command.Target.TeamID {
		return publication.ErrRequestConflict
	}
	if record.Command.Target.ReuseActiveRevision && versionStatus == "published" && publishedVersion != nil && *publishedVersion == ref.WorkflowVersion {
		return nil
	}
	if versionStatus != "draft" || !versionUpdatedAt.Equal(expected) {
		return workflow.ErrVersionConflict
	}
	// Product access was rechecked by the adapter for this activation. The CAS
	// also prevents activating a definition edited since the recorded decision.
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, value := range []time.Time{workflowUpdatedAt, versionUpdatedAt} {
		if !now.After(value) {
			now = value.Add(time.Microsecond)
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE weave_team_workflow_versions SET status='published',published_at=$4,updated_at=$4 WHERE workspace_id=$1 AND workflow_id=$2 AND version=$3`, ref.WorkspaceID, ref.WorkflowID, ref.WorkflowVersion, now); err != nil {
		return fmt.Errorf("activate product workflow version: %w", err)
	}
	if _, err = tx.Exec(ctx, `UPDATE weave_team_workflows SET published_version=$3,updated_at=$4 WHERE workspace_id=$1 AND id=$2`, ref.WorkspaceID, ref.WorkflowID, ref.WorkflowVersion, now); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE weave_teams SET default_workflow_id=$3,updated_at=GREATEST(updated_at,$4) WHERE workspace_id=$1 AND id=$2 AND default_workflow_id IS NULL`, ref.WorkspaceID, teamID, ref.WorkflowID, now)
	return err
}

// ReserveTx records immutable delivery intent alongside product materialization
// (agent settings or a restored draft). It never calls the kernel. A caller must
// commit this product transaction before calling Publish with the fixed command.
func (p *ProductPublication) ReserveTx(ctx context.Context, tx pgx.Tx, command PublicationCommand) (PublicationRequestRecord, error) {
	if p == nil || p.requests == nil || tx == nil || p.authorize == nil {
		return PublicationRequestRecord{}, errors.New("product publication unavailable")
	}
	record, err := NewPublicationRequest(ctx, command)
	if err != nil {
		return record, err
	}
	if err = p.authorize(ctx, command); err != nil {
		return record, err
	}
	subject, _ := json.Marshal(record.Subject)
	encoded, _ := json.Marshal(record.Command)
	_, err = tx.Exec(ctx, `INSERT INTO weave_team_publication_requests(workspace_id,request_id,actor_subject,request_digest,command,state) VALUES($1,$2,$3::jsonb,$4,$5::jsonb,'pending') ON CONFLICT(workspace_id,request_id) DO NOTHING`, record.Subject.WorkspaceID, command.Request.RequestID, string(subject), record.Digest, string(encoded))
	if err != nil {
		return record, err
	}
	return loadPublicationRequest(ctx, tx, record)
}

func (p *ProductPublication) Lookup(ctx context.Context, workspaceID, requestID string) (PublicationRequestRecord, bool, error) {
	if p == nil || p.requests == nil || p.requests.pool == nil {
		return PublicationRequestRecord{}, false, errors.New("product publication unavailable")
	}
	return p.requests.findPublication(ctx, workspaceID, requestID)
}

// SetActivationEffect installs a server-owned catalog/build effect in the same
// product transaction as activation and its receipt. It cannot write kernel data.
func (p *ProductPublication) SetActivationEffect(effect func(context.Context, pgx.Tx, PublicationRequestRecord) error) {
	base := p.requests.activate
	p.requests.activate = func(ctx context.Context, tx pgx.Tx, record PublicationRequestRecord) error {
		if err := base(ctx, tx, record); err != nil {
			return err
		}
		if effect != nil {
			return effect(ctx, tx, record)
		}
		return nil
	}
}

// SetCandidateAssociation installs the product usage-source association. The
// callback and stored kernel receipt commit together before execution starts.
func (p *ProductPublication) SetCandidateAssociation(effect func(context.Context, pgx.Tx, CandidateRequestRecord) error) {
	p.requests.associateUsage = effect
}

func (p *ProductPublication) AdmitCandidate(ctx context.Context, target CandidateTarget, request publication.CandidateRunRequest) (CandidateRequestRecord, error) {
	if p == nil || p.requests == nil || p.kernel == nil || p.authorize == nil {
		return CandidateRequestRecord{}, errors.New("product candidate admission unavailable")
	}
	payload, err := frozen.DecodeArtifactEnvelopeV1(request.Candidate)
	if err != nil {
		return CandidateRequestRecord{}, err
	}
	if err = p.authorize(ctx, PublicationCommand{Target: PublicationTarget{TeamID: payload.Team.TeamID, BuildRunID: target.BuildRunID}, Request: publication.PublishRequest{Version: publication.ContractVersion, RequestID: request.RequestID, Candidate: request.Candidate}}); err != nil {
		return CandidateRequestRecord{}, err
	}
	return (CandidateAdmissionFlow{Requests: p.requests, Kernel: p.kernel}).Admit(ctx, target, request)
}

func (p *ProductPublication) LookupCandidate(ctx context.Context, workspaceID, requestID string) (CandidateRequestRecord, bool, error) {
	if p == nil || p.requests == nil || p.requests.pool == nil {
		return CandidateRequestRecord{}, false, errors.New("product candidate admission unavailable")
	}
	return p.requests.findCandidate(ctx, workspaceID, requestID)
}
