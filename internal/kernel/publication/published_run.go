package publication

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/base/execution"
)

// PublishedService admits an exact, already frozen revision. Each request owns
// one receipt and one execution identity even when its commit result is unknown.
type PublishedService interface {
	AdmitPublished(context.Context, PublishedRunRequest) (AdmissionReceipt, error)
}

var ErrAdmissionClosed = errors.New("published admission request is closed")

// ClosePublished fences a not-yet-accepted request, or returns its existing
// receipt. A nil receipt proves a durable closure; it never means "not found".
type PublishedReconciler interface {
	ClosePublished(context.Context, PublishedRunRequest) (*AdmissionReceipt, error)
}

type PublishedTrigger struct {
	Type          string `json:"type"`
	SourceRef     string `json:"source_ref"`
	OccurrenceKey string `json:"occurrence_key,omitempty"`
}

// PublishedRunRequest contains immutable execution facts, not a caller-built
// snapshot, task state, permission grant or product transaction. Explicit IDs
// preserve server-owned dispatch correlations; they confer no authorization.
type PublishedRunRequest struct {
	Version          string                        `json:"version"`
	RequestID        string                        `json:"request_id"`
	Revision         RevisionRef                   `json:"revision"`
	RunID            string                        `json:"run_id"`
	TaskID           string                        `json:"task_id"`
	Input            json.RawMessage               `json:"input"`
	InputVersion     string                        `json:"input_version"`
	ProjectID        string                        `json:"project_id,omitempty"`
	ContextKey       string                        `json:"context_key,omitempty"`
	Trigger          PublishedTrigger              `json:"trigger"`
	ParentTaskID     string                        `json:"parent_task_id,omitempty"`
	DeadlineAt       *time.Time                    `json:"deadline_at,omitempty"`
	DeliveryContract *deliverable.DeliveryContract `json:"delivery_contract,omitempty"`
}

func (r PublishedRunRequest) Fingerprint(ctx context.Context) (string, error) {
	subject, err := execution.RequireSubject(ctx, r.Revision.WorkspaceID)
	if err != nil {
		return "", err
	}
	if r.Version != ContractVersion || !validID(r.RequestID) || !validID(r.RunID) || !validID(r.TaskID) ||
		!validID(r.Revision.WorkflowID) || r.Revision.WorkflowVersion < 1 || len(r.Revision.ContentHash) != 64 ||
		!validID(r.InputVersion) || !validID(r.Trigger.SourceRef) || len(r.Input) == 0 || len(r.Input) > 8*1024*1024 {
		return "", ErrInvalidRequest
	}
	for _, value := range []string{r.ProjectID, r.ContextKey, r.ParentTaskID} {
		if value != "" && !validID(value) {
			return "", ErrInvalidRequest
		}
	}
	switch r.Trigger.Type {
	case "manual", "conversation_explicit":
		if r.Trigger.OccurrenceKey != "" {
			return "", ErrInvalidRequest
		}
	case "schedule":
		if !validID(r.Trigger.OccurrenceKey) {
			return "", ErrInvalidRequest
		}
	default:
		return "", ErrInvalidRequest
	}
	if _, err := json.Marshal(r); err != nil || !json.Valid(r.Input) {
		return "", ErrInvalidRequest
	}
	if r.DeadlineAt != nil {
		if r.DeadlineAt.IsZero() {
			return "", ErrInvalidRequest
		}
		deadline := r.DeadlineAt.UTC()
		r.DeadlineAt = &deadline
	}
	if r.DeliveryContract != nil {
		if err := deliverable.ValidateDeliveryContract(r.DeliveryContract); err != nil {
			return "", err
		}
	}
	return fingerprint(subject, r)
}

func (r AdmissionReceipt) VerifyPublished(ctx context.Context, request PublishedRunRequest) error {
	digest, err := request.Fingerprint(ctx)
	if err != nil {
		return err
	}
	subject, _ := execution.RequireSubject(ctx, request.Revision.WorkspaceID)
	if r.Version != ContractVersion || r.RequestID != request.RequestID || r.RequestDigest != digest ||
		r.Subject != subject || r.Revision != request.Revision || r.TaskID != request.TaskID ||
		r.RunID != request.RunID || r.RunSnapshotID != request.RunID {
		return ErrInvalidReceipt
	}
	return nil
}
