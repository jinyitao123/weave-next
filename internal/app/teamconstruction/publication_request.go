package teamconstruction

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/publication"
)

type PublicationRequestState string

const (
	PublicationPending          PublicationRequestState = "pending"
	PublicationRevisionObtained PublicationRequestState = "revision_obtained"
	PublicationActivated        PublicationRequestState = "activated"
)

var ErrPublicationTransition = errors.New("publication request transition is invalid")

// PublicationTarget is product-owned activation intent. It never travels to
// the kernel. ExpectedAssetVersion fences activation against a later user edit.
type PublicationTarget struct {
	BuildRunID           string `json:"build_run_id"`
	TeamID               string `json:"team_id"`
	ExpectedAssetVersion string `json:"expected_asset_version"`
}

type PublicationCommand struct {
	Target  PublicationTarget          `json:"target"`
	Request publication.PublishRequest `json:"request"`
}

// PublicationRequestRecord is a durable product-to-kernel association, not an
// execution job. It has no worker, lease, retry counter or terminal task state.
type PublicationRequestRecord struct {
	Subject execution.Subject           `json:"subject"`
	Digest  string                      `json:"digest"`
	Command PublicationCommand          `json:"command"`
	State   PublicationRequestState     `json:"state"`
	Receipt *publication.PublishReceipt `json:"receipt,omitempty"`
}

// PublicationRequests must persist the supplied immutable record before any
// kernel call. Its uniqueness key is workspace/request ID, with subject and
// digest checked on every read/write. RecordRevision is a compare-and-set
// transition and rejects a changed receipt. Activate atomically checks current
// product authorization and ExpectedAssetVersion, activates the target revision,
// and records activated. It must never write kernel publication/execution tables.
type PublicationRequests interface {
	ReservePublication(context.Context, PublicationRequestRecord) (PublicationRequestRecord, error)
	RecordRevision(context.Context, PublicationRequestRecord) (PublicationRequestRecord, error)
	ActivatePublication(context.Context, PublicationRequestRecord) (PublicationRequestRecord, error)
}

type Publisher interface {
	Publish(context.Context, publication.PublishRequest) (publication.PublishReceipt, error)
}

// PublicationFlow makes one delivery attempt. Its caller retries using the
// existing build operation checkpoint; it owns no worker or polling loop. A
// failure leaves the last persisted phase intact. An unknown kernel response is
// retried with the same request, never a newly generated candidate.
type PublicationFlow struct {
	Requests PublicationRequests
	Kernel   Publisher
}

func (f PublicationFlow) Publish(ctx context.Context, command PublicationCommand) (PublicationRequestRecord, error) {
	record, err := NewPublicationRequest(ctx, command)
	if err != nil {
		return PublicationRequestRecord{}, err
	}
	if f.Requests == nil || f.Kernel == nil {
		return PublicationRequestRecord{}, errors.New("publication flow unavailable")
	}
	record, err = f.Requests.ReservePublication(ctx, record)
	if err != nil {
		return PublicationRequestRecord{}, err
	}
	if err = record.Verify(ctx, command); err != nil {
		return PublicationRequestRecord{}, err
	}
	if record.State == PublicationPending {
		receipt, callErr := f.Kernel.Publish(ctx, record.Command.Request)
		if callErr != nil {
			return record, callErr
		}
		next, transitionErr := record.AcceptRevision(ctx, receipt)
		if transitionErr != nil {
			return record, transitionErr
		}
		record, err = f.Requests.RecordRevision(ctx, next)
		if err != nil {
			return PublicationRequestRecord{}, err
		}
		if err = record.Verify(ctx, command); err != nil {
			return PublicationRequestRecord{}, err
		}
		if record.State == PublicationPending {
			return PublicationRequestRecord{}, ErrPublicationTransition
		}
	}
	if record.State == PublicationRevisionObtained {
		record, err = f.Requests.ActivatePublication(ctx, record)
		if err != nil {
			return PublicationRequestRecord{}, err
		}
		if err = record.Verify(ctx, command); err != nil {
			return PublicationRequestRecord{}, err
		}
		if record.State != PublicationActivated {
			return PublicationRequestRecord{}, ErrPublicationTransition
		}
	}
	return record, nil
}

func NewPublicationRequest(ctx context.Context, command PublicationCommand) (PublicationRequestRecord, error) {
	if _, err := command.Request.Fingerprint(ctx); err != nil {
		return PublicationRequestRecord{}, err
	}
	if !validPublicationTarget(command.Target) {
		return PublicationRequestRecord{}, publication.ErrInvalidRequest
	}
	subject, _ := execution.RequireSubject(ctx, command.Request.Candidate.WorkspaceID)
	payload, err := frozen.DecodeArtifactEnvelopeV1(command.Request.Candidate)
	if err != nil {
		return PublicationRequestRecord{}, err
	}
	if payload.Team.TeamID != command.Target.TeamID {
		return PublicationRequestRecord{}, publication.ErrInvalidRequest
	}
	digest, err := commandDigest(subject, command)
	if err != nil {
		return PublicationRequestRecord{}, err
	}
	// Copy the fixed envelope; later mutation of caller-owned bytes must not
	// replace the durable candidate selected for this request.
	command.Request.Candidate.Payload = append(json.RawMessage(nil), command.Request.Candidate.Payload...)
	return PublicationRequestRecord{Subject: subject, Digest: digest, Command: command, State: PublicationPending}, nil
}

func (r PublicationRequestRecord) Verify(ctx context.Context, command PublicationCommand) error {
	expected, err := NewPublicationRequest(ctx, command)
	if err != nil {
		return err
	}
	if r.Subject != expected.Subject {
		return execution.ErrSubjectMismatch
	}
	actual, err := NewPublicationRequest(ctx, r.Command)
	if err != nil {
		return err
	}
	if r.Digest != expected.Digest || actual.Digest != expected.Digest {
		return publication.ErrRequestConflict
	}
	switch r.State {
	case PublicationPending:
		if r.Receipt != nil {
			return ErrPublicationTransition
		}
	case PublicationRevisionObtained, PublicationActivated:
		if r.Receipt == nil {
			return ErrPublicationTransition
		}
		return r.Receipt.Verify(ctx, command.Request)
	default:
		return ErrPublicationTransition
	}
	return nil
}

func (r PublicationRequestRecord) AcceptRevision(ctx context.Context, receipt publication.PublishReceipt) (PublicationRequestRecord, error) {
	if err := r.Verify(ctx, r.Command); err != nil {
		return PublicationRequestRecord{}, err
	}
	if err := receipt.Verify(ctx, r.Command.Request); err != nil {
		return PublicationRequestRecord{}, err
	}
	if r.Receipt != nil {
		if *r.Receipt != receipt {
			return PublicationRequestRecord{}, publication.ErrRequestConflict
		}
		return r, nil
	}
	r.Receipt = &receipt
	r.State = PublicationRevisionObtained
	return r, nil
}

// Activated is used by the product store only inside its own asset activation
// transaction. Calling this reducer alone does not activate or persist an asset.
func (r PublicationRequestRecord) Activated(ctx context.Context) (PublicationRequestRecord, error) {
	if err := r.Verify(ctx, r.Command); err != nil {
		return PublicationRequestRecord{}, err
	}
	if r.State == PublicationPending {
		return PublicationRequestRecord{}, ErrPublicationTransition
	}
	r.State = PublicationActivated
	return r, nil
}

func validPublicationTarget(target PublicationTarget) bool {
	for _, value := range []string{target.BuildRunID, target.TeamID, target.ExpectedAssetVersion} {
		if value == "" || len(value) > 512 || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\x00\r\n") {
			return false
		}
	}
	return true
}

func commandDigest(subject execution.Subject, command PublicationCommand) (string, error) {
	kernelDigest, err := command.Request.Fingerprint(execution.WithSubject(context.Background(), subject))
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(struct {
		Target       PublicationTarget `json:"target"`
		KernelDigest string            `json:"kernel_digest"`
	}{command.Target, kernelDigest})
	if err != nil {
		return "", err
	}
	canonical, err := frozen.CanonicalizeJSON(data)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(append([]byte("weave.product-publication/v1\x00"), canonical...))
	return hex.EncodeToString(hash[:]), nil
}
