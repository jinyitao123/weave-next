// Package publication defines the storage-free boundary for freezing a
// definition and admitting a candidate through the platform execution kernel.
package publication

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

const ContractVersion = "weave.publication/v1"

var (
	ErrInvalidRequest  = errors.New("invalid publication request")
	ErrRequestConflict = errors.New("publication request identity conflict")
	ErrInvalidReceipt  = errors.New("publication receipt does not match request")
)

// Service owns publication and admission transactions. Implementations must
// persist the request identity and result in the same transaction as their
// frozen revision or snapshot/task facts. Repeating an unchanged request must
// return the same receipt; changing its subject or content must fail. Neither
// method activates a product asset, runs an execution loop, nor borrows a
// caller's transaction. Contract validation alone does not authorize execution:
// the kernel must also validate graph/dependencies and enforce current grants.
type Service interface {
	Publish(context.Context, PublishRequest) (PublishReceipt, error)
	AdmitCandidate(context.Context, CandidateRunRequest) (AdmissionReceipt, error)
}

// PublishRequest carries a fixed, resolved candidate. The actor is obtained
// only from authenticated context, never from the candidate or request body.
type PublishRequest struct {
	Version   string                    `json:"version"`
	RequestID string                    `json:"request_id"`
	Candidate frozen.ArtifactEnvelopeV1 `json:"candidate"`
}

type RevisionRef struct {
	WorkspaceID     string `json:"workspace_id"`
	WorkflowID      string `json:"workflow_id"`
	WorkflowVersion int    `json:"workflow_version"`
	ContentHash     string `json:"content_hash"`
}

type PublishReceipt struct {
	Version       string            `json:"version"`
	RequestID     string            `json:"request_id"`
	RequestDigest string            `json:"request_digest"`
	Subject       execution.Subject `json:"subject"`
	Revision      RevisionRef       `json:"revision"`
}

// CandidateRunRequest does not carry a task, lease, or state transition. The
// kernel creates the snapshot and task atomically. Correlation fields are
// immutable tracing facts, not authorization or instructions to the kernel.
// DeadlineAt is absolute and cannot be renewed by replaying this request.
type CandidateRunRequest struct {
	Version      string                    `json:"version"`
	RequestID    string                    `json:"request_id"`
	Candidate    frozen.ArtifactEnvelopeV1 `json:"candidate"`
	Input        json.RawMessage           `json:"input"`
	InputVersion string                    `json:"input_version"`
	SourceRef    string                    `json:"source_ref"`
	Purpose      string                    `json:"purpose"`
	ParentTaskID string                    `json:"parent_task_id,omitempty"`
	DeadlineAt   *time.Time                `json:"deadline_at,omitempty"`
}

type AdmissionReceipt struct {
	Version       string            `json:"version"`
	RequestID     string            `json:"request_id"`
	RequestDigest string            `json:"request_digest"`
	Subject       execution.Subject `json:"subject"`
	Revision      RevisionRef       `json:"revision"`
	TaskID        string            `json:"task_id"`
	RunID         string            `json:"run_id"`
	RunSnapshotID string            `json:"run_snapshot_id"`
}

func (r PublishRequest) Fingerprint(ctx context.Context) (string, error) {
	subject, err := validateCandidate(ctx, r.Version, r.RequestID, r.Candidate)
	if err != nil {
		return "", err
	}
	// The verified content hash already binds the normalized payload. Cosmetic
	// JSON formatting or unordered frozen sets must not change request identity.
	r.Candidate.Payload = nil
	return fingerprint(subject, r)
}

func (r CandidateRunRequest) Fingerprint(ctx context.Context) (string, error) {
	subject, err := validateCandidate(ctx, r.Version, r.RequestID, r.Candidate)
	if err != nil {
		return "", err
	}
	if !validID(r.InputVersion) || !validID(r.SourceRef) || !validID(r.Purpose) ||
		len(r.Input) == 0 || len(r.Input) > 8*1024*1024 ||
		(r.ParentTaskID != "" && !validID(r.ParentTaskID)) ||
		(r.DeadlineAt != nil && r.DeadlineAt.IsZero()) {
		return "", ErrInvalidRequest
	}
	if _, err := frozen.CanonicalizeJSON(r.Input); err != nil {
		return "", ErrInvalidRequest
	}
	r.Candidate.Payload = nil
	if r.DeadlineAt != nil {
		deadline := r.DeadlineAt.UTC()
		r.DeadlineAt = &deadline
	}
	return fingerprint(subject, r)
}

func (r PublishReceipt) Verify(ctx context.Context, request PublishRequest) error {
	digest, err := request.Fingerprint(ctx)
	if err != nil {
		return err
	}
	subject, _ := execution.RequireSubject(ctx, request.Candidate.WorkspaceID)
	if r.Version != ContractVersion || r.RequestID != request.RequestID || r.RequestDigest != digest ||
		r.Subject != subject || r.Revision != CandidateRevision(request.Candidate) {
		return ErrInvalidReceipt
	}
	return nil
}

func (r AdmissionReceipt) Verify(ctx context.Context, request CandidateRunRequest) error {
	digest, err := request.Fingerprint(ctx)
	if err != nil {
		return err
	}
	subject, _ := execution.RequireSubject(ctx, request.Candidate.WorkspaceID)
	if r.Version != ContractVersion || r.RequestID != request.RequestID || r.RequestDigest != digest ||
		r.Subject != subject || r.Revision != CandidateRevision(request.Candidate) ||
		!validID(r.TaskID) || !validID(r.RunID) || !validID(r.RunSnapshotID) {
		return ErrInvalidReceipt
	}
	return nil
}

func CandidateRevision(candidate frozen.ArtifactEnvelopeV1) RevisionRef {
	return RevisionRef{WorkspaceID: candidate.WorkspaceID, WorkflowID: candidate.WorkflowID,
		WorkflowVersion: candidate.WorkflowVersion, ContentHash: candidate.ContentHash}
}

func validateCandidate(ctx context.Context, version, requestID string, candidate frozen.ArtifactEnvelopeV1) (execution.Subject, error) {
	if version != ContractVersion || !validID(requestID) || len(candidate.Payload) > 16*1024*1024 {
		return execution.Subject{}, ErrInvalidRequest
	}
	subject, err := execution.RequireSubject(ctx, candidate.WorkspaceID)
	if err != nil {
		return execution.Subject{}, err
	}
	payload, err := frozen.DecodeArtifactEnvelopeV1(candidate)
	if err != nil {
		return execution.Subject{}, err
	}
	if payload.Team.WorkspaceID != subject.WorkspaceID || !validID(payload.Team.TeamID) {
		return execution.Subject{}, ErrInvalidRequest
	}
	return subject, nil
}

func fingerprint(subject execution.Subject, request any) (string, error) {
	data, err := json.Marshal(struct {
		Subject execution.Subject `json:"subject"`
		Request any               `json:"request"`
	}{subject, request})
	if err != nil {
		return "", ErrInvalidRequest
	}
	canonical, err := frozen.CanonicalizeJSON(data)
	if err != nil {
		return "", ErrInvalidRequest
	}
	hash := sha256.Sum256(append([]byte(ContractVersion+"\x00"), canonical...))
	return hex.EncodeToString(hash[:]), nil
}

func validID(value string) bool {
	return value != "" && len(value) <= 512 && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\x00\r\n")
}
