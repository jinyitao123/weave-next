// Package workflowadmission stores product delivery intent around the kernel's
// durable admission receipt. It has no execution queue, lease or retry policy.
package workflowadmission

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/publication"
)

type Target struct {
	TeamID          string          `json:"team_id"`
	InputRevisionID string          `json:"input_revision_id,omitempty"`
	ChatRequestID   string          `json:"chat_request_id,omitempty"`
	ConversationID  string          `json:"conversation_id,omitempty"`
	ScheduleID      string          `json:"schedule_id,omitempty"`
	OccurrenceKey   string          `json:"occurrence_key,omitempty"`
	ScheduledFor    time.Time       `json:"scheduled_for,omitempty"`
	Schedule        json.RawMessage `json:"schedule,omitempty"`
}

type Record struct {
	Subject    execution.Subject
	Digest     string
	Request    publication.PublishedRunRequest
	Target     Target
	Receipt    *publication.AdmissionReceipt
	Associated bool
}

type Association func(context.Context, pgx.Tx, Record) error

type Store struct {
	pool   *pgxpool.Pool
	kernel publication.PublishedService
}

func New(pool *pgxpool.Pool, kernel publication.PublishedService) *Store {
	return &Store{pool: pool, kernel: kernel}
}

// ReserveTx commits only product facts with the caller's input/occurrence locks.
// Every retry must load this exact request before calling the kernel again.
func (s *Store) ReserveTx(ctx context.Context, tx pgx.Tx, request publication.PublishedRunRequest, target Target) (Record, error) {
	if s == nil || s.pool == nil || tx == nil {
		return Record{}, errors.New("product admission unavailable")
	}
	subject, err := execution.RequireSubject(ctx, request.Revision.WorkspaceID)
	if err != nil {
		return Record{}, err
	}
	expected := Record{Subject: subject, Request: request, Target: target}
	expected.Digest, err = recordDigest(ctx, request, target)
	if err != nil {
		return Record{}, err
	}
	actor, _ := json.Marshal(subject)
	req, _ := json.Marshal(request)
	targetJSON, _ := json.Marshal(target)
	_, err = tx.Exec(ctx, `INSERT INTO weave_workflow_admission_requests(workspace_id,request_id,actor_subject,request_digest,request,target) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(workspace_id,request_id) DO NOTHING`, subject.WorkspaceID, request.RequestID, string(actor), expected.Digest, string(req), string(targetJSON))
	if err != nil {
		return Record{}, err
	}
	actual, err := readRecord(ctx, tx, subject.WorkspaceID, request.RequestID, true)
	if err != nil {
		return Record{}, err
	}
	if actual.Digest != expected.Digest {
		return Record{}, publication.ErrRequestConflict
	}
	return actual, nil
}

func (s *Store) Get(ctx context.Context, workspaceID, requestID string) (Record, error) {
	if s == nil || s.pool == nil {
		return Record{}, errors.New("product admission unavailable")
	}
	return readRecord(ctx, s.pool, workspaceID, requestID, false)
}

// Admit always rechecks the same kernel request, including retained receipts.
// The association transaction opens only after the kernel call has returned.
func (s *Store) Admit(ctx context.Context, workspaceID, requestID string, associate Association) (Record, error) {
	if s == nil || s.pool == nil || s.kernel == nil {
		return Record{}, errors.New("product admission unavailable")
	}
	record, err := s.Get(ctx, workspaceID, requestID)
	if err != nil {
		return Record{}, err
	}
	receipt, err := s.kernel.AdmitPublished(ctx, record.Request)
	if err != nil {
		return Record{}, err
	}
	return s.associate(ctx, record, receipt, associate)
}

// Reconcile closes a request that the kernel has not accepted, or associates
// its already committed receipt. It cannot create an execution as a side effect.
func (s *Store) Reconcile(ctx context.Context, workspaceID, requestID string, associate Association) (Record, bool, error) {
	record, err := s.Get(ctx, workspaceID, requestID)
	if err != nil {
		return Record{}, false, err
	}
	kernel, ok := s.kernel.(publication.PublishedReconciler)
	if !ok {
		return Record{}, false, errors.New("published admission reconciliation unavailable")
	}
	receipt, err := kernel.ClosePublished(ctx, record.Request)
	if err != nil {
		return Record{}, false, err
	}
	if receipt == nil {
		return record, true, nil
	}
	// Close proved this request was already accepted. Refresh current access
	// through that same request before writing any product association.
	refreshed, err := s.kernel.AdmitPublished(ctx, record.Request)
	if err != nil {
		return Record{}, false, err
	}
	if refreshed != *receipt {
		return Record{}, false, publication.ErrInvalidReceipt
	}
	stored, err := s.associate(ctx, record, refreshed, associate)
	return stored, false, err
}

func (s *Store) associate(ctx context.Context, record Record, receipt publication.AdmissionReceipt, associate Association) (Record, error) {
	workspaceID, requestID := record.Request.Revision.WorkspaceID, record.Request.RequestID
	var err error
	if err = receipt.VerifyPublished(ctx, record.Request); err != nil {
		return Record{}, err
	}
	if record.Receipt != nil && *record.Receipt != receipt {
		return Record{}, publication.ErrInvalidReceipt
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Record{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	stored, err := readRecord(ctx, tx, workspaceID, requestID, true)
	if err != nil {
		return Record{}, err
	}
	if stored.Digest != record.Digest || (stored.Receipt != nil && *stored.Receipt != receipt) {
		return Record{}, publication.ErrRequestConflict
	}
	stored.Receipt = &receipt
	if !stored.Associated {
		if associate == nil {
			return Record{}, errors.New("product admission association unavailable")
		}
		if err = associate(ctx, tx, stored); err != nil {
			return Record{}, err
		}
		encoded, _ := json.Marshal(receipt)
		if _, err = tx.Exec(ctx, `UPDATE weave_workflow_admission_requests SET receipt=$3,associated=true WHERE workspace_id=$1 AND request_id=$2`, workspaceID, requestID, string(encoded)); err != nil {
			return Record{}, err
		}
		stored.Associated = true
	}
	if err = tx.Commit(ctx); err != nil {
		return Record{}, err
	}
	return stored, nil
}

type recordQuery interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func readRecord(ctx context.Context, q recordQuery, workspaceID, requestID string, lock bool) (Record, error) {
	subject, err := execution.RequireSubject(ctx, workspaceID)
	if err != nil {
		return Record{}, err
	}
	query := `SELECT actor_subject,request_digest,request,target,receipt,associated FROM weave_workflow_admission_requests WHERE workspace_id=$1 AND request_id=$2`
	if lock {
		query += " FOR UPDATE"
	}
	var record Record
	var actor, request, target, receipt []byte
	if err = q.QueryRow(ctx, query, workspaceID, requestID).Scan(&actor, &record.Digest, &request, &target, &receipt, &record.Associated); err != nil {
		return Record{}, err
	}
	if err = json.Unmarshal(actor, &record.Subject); err != nil {
		return Record{}, err
	}
	if record.Subject != subject {
		return Record{}, execution.ErrSubjectMismatch
	}
	if err = json.Unmarshal(request, &record.Request); err != nil {
		return Record{}, err
	}
	if err = json.Unmarshal(target, &record.Target); err != nil {
		return Record{}, err
	}
	if len(receipt) > 0 {
		if err = json.Unmarshal(receipt, &record.Receipt); err != nil {
			return Record{}, err
		}
	}
	digest, err := recordDigest(ctx, record.Request, record.Target)
	if err != nil {
		return Record{}, err
	}
	if digest != record.Digest || record.Request.RequestID != requestID || record.Request.Revision.WorkspaceID != workspaceID {
		return Record{}, publication.ErrRequestConflict
	}
	if record.Receipt != nil {
		if err = record.Receipt.VerifyPublished(ctx, record.Request); err != nil {
			return Record{}, err
		}
	}
	return record, nil
}

func recordDigest(ctx context.Context, request publication.PublishedRunRequest, target Target) (string, error) {
	kernelDigest, err := request.Fingerprint(ctx)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(struct {
		KernelDigest string `json:"kernel_digest"`
		Target       Target `json:"target"`
	}{kernelDigest, target})
	if err != nil {
		return "", err
	}
	data, err = frozen.CanonicalizeJSON(data)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(append([]byte("weave.product-admission/v1\x00"), data...))
	return hex.EncodeToString(digest[:]), nil
}
