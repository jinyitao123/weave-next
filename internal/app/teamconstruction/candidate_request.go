package teamconstruction

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"strings"
	"time"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/publication"
)

// CandidateTarget supplies the product usage-source association without making
// the execution kernel interpret build rounds or evaluation roles.
type CandidateTarget struct {
	ExpectedAssetVersion string `json:"expected_asset_version,omitempty"`
	BuildRunID           string `json:"build_run_id"`
	RoundNo              int    `json:"round_no"`
	SourceRole           string `json:"source_role"`
}

// CandidateRequestRecord holds only the fixed request and the kernel admission
// reference. All queued/running/paused/unknown/cancelled facts remain in taskqueue.
type CandidateRequestRecord struct {
	Subject execution.Subject               `json:"subject"`
	Digest  string                          `json:"digest"`
	Target  CandidateTarget                 `json:"target"`
	Request publication.CandidateRunRequest `json:"request"`
	Receipt *publication.AdmissionReceipt   `json:"receipt,omitempty"`
}

// CandidateRequests atomically reserves an immutable workspace/request ID and
// attaches its one admission receipt. A receipt write also records the build's
// usage-source association in product storage. No method writes a kernel task.
type CandidateRequests interface {
	ReserveCandidate(context.Context, CandidateRequestRecord) (CandidateRequestRecord, error)
	RecordAdmission(context.Context, CandidateRequestRecord) (CandidateRequestRecord, error)
}

type CandidateAdmitter interface {
	AdmitCandidate(context.Context, publication.CandidateRunRequest) (publication.AdmissionReceipt, error)
}

type CandidateAdmissionFlow struct {
	Requests CandidateRequests
	Kernel   CandidateAdmitter
}

func (f CandidateAdmissionFlow) Admit(ctx context.Context, target CandidateTarget, request publication.CandidateRunRequest) (CandidateRequestRecord, error) {
	digest, err := candidateRequestDigest(ctx, target, request)
	if err != nil {
		return CandidateRequestRecord{}, err
	}
	if f.Requests == nil || f.Kernel == nil {
		return CandidateRequestRecord{}, errors.New("candidate admission unavailable")
	}
	subject, _ := execution.RequireSubject(ctx, request.Candidate.WorkspaceID)
	request.Candidate.Payload = append([]byte(nil), request.Candidate.Payload...)
	request.Input = append([]byte(nil), request.Input...)
	if request.DeadlineAt != nil {
		value := *request.DeadlineAt
		request.DeadlineAt = &value
	}
	record, err := f.Requests.ReserveCandidate(ctx, CandidateRequestRecord{Subject: subject, Digest: digest, Target: target, Request: request})
	if err != nil {
		return CandidateRequestRecord{}, err
	}
	if err = record.Verify(ctx, target, request); err != nil {
		return CandidateRequestRecord{}, err
	}
	// The stable admission request rechecks current authorization on recovery;
	// its idempotent kernel receipt always names the original task.
	receipt, err := f.Kernel.AdmitCandidate(ctx, record.Request)
	if err != nil {
		return record, err
	}
	if err = receipt.Verify(ctx, request); err != nil {
		return record, err
	}
	if record.Receipt != nil {
		if *record.Receipt != receipt {
			return record, publication.ErrRequestConflict
		}
		return record, nil
	}
	record.Receipt = &receipt
	record, err = f.Requests.RecordAdmission(ctx, record)
	if err != nil {
		return CandidateRequestRecord{}, err
	}
	if err = record.Verify(ctx, target, request); err != nil {
		return CandidateRequestRecord{}, err
	}
	if record.Receipt == nil {
		return CandidateRequestRecord{}, publication.ErrInvalidReceipt
	}
	return record, nil
}

func (r CandidateRequestRecord) Verify(ctx context.Context, target CandidateTarget, request publication.CandidateRunRequest) error {
	digest, err := candidateRequestDigest(ctx, target, request)
	if err != nil {
		return err
	}
	subject, _ := execution.RequireSubject(ctx, request.Candidate.WorkspaceID)
	if r.Subject != subject {
		return execution.ErrSubjectMismatch
	}
	storedDigest, err := candidateRequestDigest(ctx, r.Target, r.Request)
	if err != nil {
		return err
	}
	if r.Digest != digest || storedDigest != digest {
		return publication.ErrRequestConflict
	}
	if r.Receipt != nil {
		return r.Receipt.Verify(ctx, request)
	}
	return nil
}

func candidateRequestDigest(ctx context.Context, target CandidateTarget, request publication.CandidateRunRequest) (string, error) {
	if target.RoundNo < 0 {
		return "", publication.ErrInvalidRequest
	}
	for _, value := range []string{target.BuildRunID, target.SourceRole} {
		if value == "" || len(value) > 512 || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\x00\r\n") {
			return "", publication.ErrInvalidRequest
		}
	}
	kernelDigest, err := request.Fingerprint(ctx)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(struct {
		Target       CandidateTarget `json:"target"`
		KernelDigest string          `json:"kernel_digest"`
	}{target, kernelDigest})
	if err != nil {
		return "", err
	}
	canonical, err := frozen.CanonicalizeJSON(data)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(append([]byte("weave.product-candidate/v1\x00"), canonical...))
	return hex.EncodeToString(hash[:]), nil
}

// restoreProductCandidate combines immutable kernel facts with the original
// product CAS token; a kernel candidate creation time cannot replace that token.
func restoreProductCandidate(record CandidateRequestRecord) (*workflow.PublicationCandidate, error) {
	candidate, err := workflow.CandidateFromEnvelope(record.Request.Candidate)
	if err != nil {
		return nil, err
	}
	candidate.ExpectedUpdatedAt, err = time.Parse(time.RFC3339Nano, record.Target.ExpectedAssetVersion)
	if err != nil {
		return nil, publication.ErrInvalidRequest
	}
	return candidate, nil
}
