package loomruntime

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const expectedRunRecordSchemaV1 = 1

// ErrExpectedRunConflict identifies an immutable run identity claimed with
// different attribution facts.
var ErrExpectedRunConflict = errors.New("expected run identity conflict")

var (
	ErrA4FreshAdmissionUnsupported = errors.New("A4 fresh admission is unsupported")
	ErrOrphanAttemptLease          = errors.New("orphan attempt lease")
)

type FreshExpectedRunAdmission struct {
	Record       ExpectedRunRecordV1
	GraphName    string
	RunStartedAt string
	AttemptID    uuid.UUID
	LeaseTTL     time.Duration
}

type FreshExpectedRunAdmitter interface {
	AdmitFresh(context.Context, FreshExpectedRunAdmission) (RunAttemptLease, error)
	A4AdmissionGuaranteed() bool
}

type FreshExpectedRunAdmissionStage string

const (
	FreshAdmissionValidate      FreshExpectedRunAdmissionStage = "validate"
	FreshAdmissionBegin         FreshExpectedRunAdmissionStage = "begin"
	FreshAdmissionLockRun       FreshExpectedRunAdmissionStage = "lock_run"
	FreshAdmissionLockClaim     FreshExpectedRunAdmissionStage = "lock_claim"
	FreshAdmissionLockIndex     FreshExpectedRunAdmissionStage = "lock_index"
	FreshAdmissionLockReceipt   FreshExpectedRunAdmissionStage = "lock_receipt"
	FreshAdmissionReadClaim     FreshExpectedRunAdmissionStage = "read_claim"
	FreshAdmissionReadIndex     FreshExpectedRunAdmissionStage = "read_index"
	FreshAdmissionReadReceipt   FreshExpectedRunAdmissionStage = "read_receipt"
	FreshAdmissionWriteClaim    FreshExpectedRunAdmissionStage = "write_claim"
	FreshAdmissionWriteIndex    FreshExpectedRunAdmissionStage = "write_index"
	FreshAdmissionWriteLease    FreshExpectedRunAdmissionStage = "write_lease"
	FreshAdmissionWriteReceipt  FreshExpectedRunAdmissionStage = "write_receipt"
	FreshAdmissionVerify        FreshExpectedRunAdmissionStage = "verify"
	FreshAdmissionVerifyReceipt FreshExpectedRunAdmissionStage = "verify_receipt"
	FreshAdmissionCommit        FreshExpectedRunAdmissionStage = "commit"
)

type FreshExpectedRunAdmissionError struct {
	Stage       FreshExpectedRunAdmissionStage
	WorkspaceID string
	RunID       string
	Err         error
}

func (err *FreshExpectedRunAdmissionError) Error() string {
	if err == nil {
		return "fresh expected run admission failed"
	}
	if err.Err == nil {
		return fmt.Sprintf(
			"fresh expected run admission failed at %s for workspace %q run %q",
			err.Stage,
			err.WorkspaceID,
			err.RunID,
		)
	}
	return fmt.Sprintf(
		"fresh expected run admission failed at %s for workspace %q run %q: %v",
		err.Stage,
		err.WorkspaceID,
		err.RunID,
		err.Err,
	)
}

func (err *FreshExpectedRunAdmissionError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Err
}

// ExpectedRunPersistenceError identifies a run whose expected identity could
// not be durably registered at the runtime boundary.
type ExpectedRunPersistenceError struct {
	Err error
}

func (err *ExpectedRunPersistenceError) Error() string {
	if err == nil || err.Err == nil {
		return "expected run persistence failed"
	}
	return "expected run persistence failed: " + err.Err.Error()
}

func (err *ExpectedRunPersistenceError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Err
}

// ExpectedRunRecordV1 is one immutable runtime admission claim.
type ExpectedRunRecordV1 struct {
	SchemaVersion          int                      `json:"schema_version"`
	WorkspaceID            string                   `json:"workspace_id"`
	RunID                  string                   `json:"run_id"`
	Agent                  string                   `json:"agent"`
	AttributionScope       TerminalAttributionScope `json:"attribution_scope"`
	TeamID                 *string                  `json:"team_id"`
	WorkflowID             *string                  `json:"workflow_id"`
	WorkflowVersion        *int                     `json:"workflow_version"`
	RunSnapshotID          *string                  `json:"run_snapshot_id"`
	ParentRunID            *string                  `json:"parent_run_id"`
	ParentSeq              *int64                   `json:"parent_seq"`
	AggregationParentRunID *string                  `json:"aggregation_parent_run_id"`
	TaskGroupID            *string                  `json:"task_group_id"`
	RegisteredAt           time.Time                `json:"-"`
}

// ExpectedRunRegistry durably claims and enumerates admitted execution IDs.
type ExpectedRunRegistry interface {
	Register(context.Context, ExpectedRunRecordV1) error
	Get(context.Context, string, string) (ExpectedRunRecordV1, bool, error)
	ListBySnapshot(context.Context, string, string) ([]ExpectedRunRecordV1, error)
}

// ExpectedRunDomainRegistry extends the registry with PostgreSQL-backed domain
// enumeration without widening the runtime-facing ExpectedRunRegistry ABI.
type ExpectedRunDomainRegistry interface {
	ExpectedRunRegistry
	ListByTeam(context.Context, string, string) ([]ExpectedRunRecordV1, error)
	ListByWorkflow(context.Context, string, string, int) ([]ExpectedRunRecordV1, error)
}

type expectedRunRegistry struct {
	store       TerminalRecordStore
	txStore     expectedRunAdmissionTxStore
	insertStore expectedRunAdmissionInsertStore
}

type expectedRunAdmissionTxStore interface {
	BeginTx(context.Context) (pgx.Tx, error)
	LockValueTx(context.Context, pgx.Tx, string, string) error
	ReadValueTx(context.Context, pgx.Tx, string, string) ([]byte, bool, error)
	PutValueTx(context.Context, pgx.Tx, string, string, []byte) error
}

type expectedRunAdmissionInsertStore interface {
	InsertValueTx(context.Context, pgx.Tx, string, string, []byte) (bool, error)
}

// NewExpectedRunRegistry constructs a registry over the shared durable record
// store. Registry writes remain independent from terminal sink writes.
func NewExpectedRunRegistry(store TerminalRecordStore) (ExpectedRunRegistry, error) {
	if store == nil || isNilTerminalRecordStore(store) {
		return nil, fmt.Errorf("expected run record store is required")
	}
	txStore, _ := store.(expectedRunAdmissionTxStore)
	insertStore, _ := store.(expectedRunAdmissionInsertStore)
	return &expectedRunRegistry{
		store:       store,
		txStore:     txStore,
		insertStore: insertStore,
	}, nil
}

func expectedRunStoreFromTerminalSink(sink TerminalSink) TerminalRecordStore {
	switch sink := sink.(type) {
	case *monotonicTerminalSink:
		return sink.store
	case *lineageTerminalSink:
		return sink.store
	default:
		return nil
	}
}

func expectedRunRecordFromAttribution(
	runID string,
	agent string,
	attribution TerminalAttribution,
	registeredAt time.Time,
) ExpectedRunRecordV1 {
	input := attribution.inputCopy()
	return ExpectedRunRecordV1{
		SchemaVersion:          expectedRunRecordSchemaV1,
		WorkspaceID:            input.WorkspaceID,
		RunID:                  runID,
		Agent:                  agent,
		AttributionScope:       input.Scope,
		TeamID:                 input.TeamID,
		WorkflowID:             input.WorkflowID,
		WorkflowVersion:        input.WorkflowVersion,
		RunSnapshotID:          input.RunSnapshotID,
		ParentRunID:            input.ParentRunID,
		ParentSeq:              input.ParentSeq,
		AggregationParentRunID: input.AggregationParentRunID,
		TaskGroupID:            input.TaskGroupID,
		RegisteredAt:           registeredAt,
	}
}

func (registry *expectedRunRegistry) Register(
	ctx context.Context,
	candidate ExpectedRunRecordV1,
) error {
	candidateBytes, err := encodeExpectedRunRecordV1(candidate)
	if err != nil {
		return fmt.Errorf("validate expected run candidate: %w", err)
	}
	namespace := expectedRunNamespace(candidate.WorkspaceID)
	claimKey := expectedRunClaimKey(candidate.RunID)
	indexKey := expectedRunDomainKey(candidate)
	if registry != nil && registry.txStore != nil {
		return registry.registerPostgres(
			ctx,
			candidate,
			candidateBytes,
			namespace,
			claimKey,
			indexKey,
		)
	}

	seed := candidateBytes
	indexBytes, indexPresent, err := registry.store.ReadValue(ctx, namespace, indexKey)
	if err != nil {
		return fmt.Errorf("read expected run domain index: %w", err)
	}
	if indexPresent {
		indexed, err := inspectExpectedRunPhysicalRecord(
			indexBytes,
			candidate.WorkspaceID,
			candidate.RunID,
			indexKey,
			false,
		)
		if err != nil {
			return fmt.Errorf("inspect expected run domain index: %w", err)
		}
		if err := compareExpectedRunIdentity(indexed, candidate); err != nil {
			return fmt.Errorf("preflight expected run domain index: %w", err)
		}
		seed = indexBytes
	}

	if err := registry.store.MutateValue(
		ctx,
		namespace,
		claimKey,
		func(current []byte, present bool) ([]byte, error) {
			if !present {
				return bytes.Clone(seed), nil
			}
			claimed, err := inspectExpectedRunPhysicalRecord(
				current,
				candidate.WorkspaceID,
				candidate.RunID,
				claimKey,
				true,
			)
			if err != nil {
				return nil, err
			}
			if err := compareExpectedRunIdentity(claimed, candidate); err != nil {
				return nil, err
			}
			return bytes.Clone(current), nil
		},
	); err != nil {
		return fmt.Errorf("claim expected run identity: %w", err)
	}

	claimBytes, claimPresent, err := registry.store.ReadValue(ctx, namespace, claimKey)
	if err != nil {
		return fmt.Errorf("read canonical expected run claim: %w", err)
	}
	if !claimPresent {
		return fmt.Errorf("canonical expected run claim disappeared after mutation")
	}
	claim, err := inspectExpectedRunPhysicalRecord(
		claimBytes,
		candidate.WorkspaceID,
		candidate.RunID,
		claimKey,
		true,
	)
	if err != nil {
		return fmt.Errorf("inspect canonical expected run claim: %w", err)
	}
	if err := compareExpectedRunIdentity(claim, candidate); err != nil {
		return fmt.Errorf("verify canonical expected run claim: %w", err)
	}

	if err := registry.store.MutateValue(
		ctx,
		namespace,
		indexKey,
		func(current []byte, present bool) ([]byte, error) {
			if !present {
				return bytes.Clone(claimBytes), nil
			}
			indexed, err := inspectExpectedRunPhysicalRecord(
				current,
				candidate.WorkspaceID,
				candidate.RunID,
				indexKey,
				false,
			)
			if err != nil {
				return nil, err
			}
			if err := compareExpectedRunIdentity(indexed, claim); err != nil {
				return nil, err
			}
			return bytes.Clone(claimBytes), nil
		},
	); err != nil {
		return fmt.Errorf("index expected run identity: %w", err)
	}
	return nil
}

func (registry *expectedRunRegistry) registerPostgres(
	ctx context.Context,
	candidate ExpectedRunRecordV1,
	candidateBytes []byte,
	namespace string,
	claimKey string,
	indexKey string,
) error {
	receiptNamespace := a4AdmissionReceiptNamespace(candidate.WorkspaceID)
	receiptKey := a4AdmissionReceiptKey(candidate.RunID)
	tx, err := registry.txStore.BeginTx(ctx)
	if err != nil {
		return fmt.Errorf("begin expected run registration: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, lock := range []struct {
		namespace string
		key       string
		label     string
	}{
		{namespace: namespace, key: claimKey, label: "claim"},
		{namespace: namespace, key: indexKey, label: "domain index"},
		{namespace: receiptNamespace, key: receiptKey, label: "admission receipt"},
	} {
		if err := registry.txStore.LockValueTx(
			ctx,
			tx,
			lock.namespace,
			lock.key,
		); err != nil {
			return fmt.Errorf("lock expected run %s: %w", lock.label, err)
		}
	}

	claimBytes, claimPresent, err := registry.txStore.ReadValueTx(
		ctx,
		tx,
		namespace,
		claimKey,
	)
	if err != nil {
		return fmt.Errorf("read expected run claim: %w", err)
	}
	indexBytes, indexPresent, err := registry.txStore.ReadValueTx(
		ctx,
		tx,
		namespace,
		indexKey,
	)
	if err != nil {
		return fmt.Errorf("read expected run domain index: %w", err)
	}
	_, receiptPresent, err := registry.txStore.ReadValueTx(
		ctx,
		tx,
		receiptNamespace,
		receiptKey,
	)
	if err != nil {
		return fmt.Errorf("read expected run admission receipt: %w", err)
	}

	canonical := candidate
	canonicalBytes := candidateBytes
	if claimPresent {
		canonical, err = inspectExpectedRunPhysicalRecord(
			claimBytes,
			candidate.WorkspaceID,
			candidate.RunID,
			claimKey,
			true,
		)
		if err != nil {
			return fmt.Errorf("inspect expected run claim: %w", err)
		}
		if err := compareExpectedRunIdentity(canonical, candidate); err != nil {
			return fmt.Errorf("preflight expected run claim: %w", err)
		}
		canonicalBytes = claimBytes
	}
	if indexPresent {
		indexed, inspectErr := inspectExpectedRunPhysicalRecord(
			indexBytes,
			candidate.WorkspaceID,
			candidate.RunID,
			indexKey,
			false,
		)
		if inspectErr != nil {
			return fmt.Errorf("inspect expected run domain index: %w", inspectErr)
		}
		if err := compareExpectedRunIdentity(indexed, candidate); err != nil {
			return fmt.Errorf("preflight expected run domain index: %w", err)
		}
		if claimPresent {
			if err := compareExpectedRunIdentity(indexed, canonical); err != nil {
				return fmt.Errorf("verify expected run claim/index identity: %w", err)
			}
		} else {
			canonical = indexed
			canonicalBytes = indexBytes
		}
	}

	if receiptPresent {
		return fmt.Errorf(
			"register certified expected run: %w",
			ErrA4AdmissionReceiptConflict,
		)
	}

	if !claimPresent {
		if err := registry.txStore.PutValueTx(
			ctx,
			tx,
			namespace,
			claimKey,
			canonicalBytes,
		); err != nil {
			return fmt.Errorf("claim expected run identity: %w", err)
		}
	}
	if !indexPresent || !bytes.Equal(indexBytes, canonicalBytes) {
		if err := registry.txStore.PutValueTx(
			ctx,
			tx,
			namespace,
			indexKey,
			canonicalBytes,
		); err != nil {
			return fmt.Errorf("index expected run identity: %w", err)
		}
	}
	if err := ensureExpectedRunDomainIndexTx(ctx, tx, canonical); err != nil {
		return fmt.Errorf("write expected run durable domain index: %w", err)
	}

	verifiedClaim, verifiedClaimPresent, err := registry.txStore.ReadValueTx(
		ctx,
		tx,
		namespace,
		claimKey,
	)
	if err != nil {
		return fmt.Errorf("verify expected run claim: %w", err)
	}
	verifiedIndex, verifiedIndexPresent, err := registry.txStore.ReadValueTx(
		ctx,
		tx,
		namespace,
		indexKey,
	)
	if err != nil {
		return fmt.Errorf("verify expected run domain index: %w", err)
	}
	_, verifiedReceiptPresent, err := registry.txStore.ReadValueTx(
		ctx,
		tx,
		receiptNamespace,
		receiptKey,
	)
	if err != nil {
		return fmt.Errorf("verify expected run admission receipt absence: %w", err)
	}
	if !verifiedClaimPresent ||
		!verifiedIndexPresent ||
		!bytes.Equal(verifiedClaim, canonicalBytes) ||
		!bytes.Equal(verifiedIndex, canonicalBytes) {
		return fmt.Errorf("verify expected run registration bytes")
	}
	if verifiedReceiptPresent {
		return fmt.Errorf(
			"register certified expected run: %w",
			ErrA4AdmissionReceiptConflict,
		)
	}
	if _, err := inspectExpectedRunPhysicalRecord(
		verifiedClaim,
		candidate.WorkspaceID,
		candidate.RunID,
		claimKey,
		true,
	); err != nil {
		return fmt.Errorf("inspect verified expected run claim: %w", err)
	}
	if _, err := inspectExpectedRunPhysicalRecord(
		verifiedIndex,
		candidate.WorkspaceID,
		candidate.RunID,
		indexKey,
		false,
	); err != nil {
		return fmt.Errorf("inspect verified expected run domain index: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit expected run registration: %w", err)
	}
	return nil
}

func (registry *expectedRunRegistry) A4AdmissionGuaranteed() bool {
	return registry != nil && registry.txStore != nil && registry.insertStore != nil
}

func (registry *expectedRunRegistry) AdmitFresh(
	ctx context.Context,
	request FreshExpectedRunAdmission,
) (RunAttemptLease, error) {
	workspaceID := request.Record.WorkspaceID
	runID := request.Record.RunID
	fail := func(stage FreshExpectedRunAdmissionStage, err error) (RunAttemptLease, error) {
		return RunAttemptLease{}, &FreshExpectedRunAdmissionError{
			Stage:       stage,
			WorkspaceID: workspaceID,
			RunID:       runID,
			Err:         err,
		}
	}

	candidateBytes, err := encodeExpectedRunRecordV1(request.Record)
	if err != nil {
		return fail(FreshAdmissionValidate, fmt.Errorf("validate expected run candidate: %w", err))
	}
	if request.GraphName == "" {
		return fail(FreshAdmissionValidate, fmt.Errorf("graph_name must be non-empty"))
	}
	if err := canonicalRunStartedAt(request.RunStartedAt); err != nil {
		return fail(FreshAdmissionValidate, err)
	}
	if request.AttemptID == uuid.Nil {
		return fail(FreshAdmissionValidate, fmt.Errorf("attempt_id must be non-zero"))
	}
	if request.LeaseTTL <= 0 {
		return fail(FreshAdmissionValidate, fmt.Errorf("lease_ttl must be positive"))
	}
	if registry == nil || registry.txStore == nil || registry.insertStore == nil {
		return fail(FreshAdmissionValidate, ErrA4FreshAdmissionUnsupported)
	}

	namespace := expectedRunNamespace(workspaceID)
	claimKey := expectedRunClaimKey(runID)
	indexKey := expectedRunDomainKey(request.Record)
	receiptNamespace := a4AdmissionReceiptNamespace(workspaceID)
	receiptKey := a4AdmissionReceiptKey(runID)
	tx, err := registry.txStore.BeginTx(ctx)
	if err != nil {
		return fail(FreshAdmissionBegin, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	leaseStore := NewPGTerminalStateStore()
	if err := leaseStore.LockTerminalRun(ctx, tx, workspaceID, runID); err != nil {
		return fail(FreshAdmissionLockRun, err)
	}
	if err := registry.txStore.LockValueTx(ctx, tx, namespace, claimKey); err != nil {
		return fail(FreshAdmissionLockClaim, err)
	}
	if err := registry.txStore.LockValueTx(ctx, tx, namespace, indexKey); err != nil {
		return fail(FreshAdmissionLockIndex, err)
	}
	if err := registry.txStore.LockValueTx(
		ctx,
		tx,
		receiptNamespace,
		receiptKey,
	); err != nil {
		return fail(FreshAdmissionLockReceipt, err)
	}

	claimBytes, claimPresent, err := registry.txStore.ReadValueTx(
		ctx,
		tx,
		namespace,
		claimKey,
	)
	if err != nil {
		return fail(FreshAdmissionReadClaim, err)
	}
	indexBytes, indexPresent, err := registry.txStore.ReadValueTx(
		ctx,
		tx,
		namespace,
		indexKey,
	)
	if err != nil {
		return fail(FreshAdmissionReadIndex, err)
	}
	receiptBytes, receiptPresent, err := registry.txStore.ReadValueTx(
		ctx,
		tx,
		receiptNamespace,
		receiptKey,
	)
	if err != nil {
		return fail(FreshAdmissionReadReceipt, err)
	}

	canonicalBytes := candidateBytes
	writeClaim := false
	writeIndex := false
	var canonical ExpectedRunRecordV1
	switch {
	case claimPresent:
		canonical, err = inspectExpectedRunPhysicalRecord(
			claimBytes,
			workspaceID,
			runID,
			claimKey,
			true,
		)
		if err != nil {
			return fail(FreshAdmissionReadClaim, err)
		}
		if err := compareExpectedRunIdentity(canonical, request.Record); err != nil {
			return fail(FreshAdmissionReadClaim, err)
		}
		canonicalBytes = claimBytes
		if !indexPresent {
			writeIndex = true
			break
		}
		indexed, inspectErr := inspectExpectedRunPhysicalRecord(
			indexBytes,
			workspaceID,
			runID,
			indexKey,
			false,
		)
		if inspectErr != nil {
			if receiptPresent {
				return fail(
					FreshAdmissionReadReceipt,
					a4AdmissionReceiptConflict(
						"certified domain index is invalid: %v",
						inspectErr,
					),
				)
			}
			return fail(FreshAdmissionReadIndex, inspectErr)
		}
		if identityErr := compareExpectedRunIdentity(indexed, canonical); identityErr != nil {
			if receiptPresent {
				return fail(
					FreshAdmissionReadReceipt,
					a4AdmissionReceiptConflict(
						"certified claim/index identity mismatch: %v",
						identityErr,
					),
				)
			}
			return fail(FreshAdmissionReadIndex, identityErr)
		}
		if bytes.Equal(indexBytes, canonicalBytes) {
			break
		}
		canonicalEncoding, encodeErr := encodeExpectedRunRecordV1(canonical)
		if encodeErr != nil {
			return fail(FreshAdmissionReadClaim, encodeErr)
		}
		indexEncoding, encodeErr := encodeExpectedRunRecordV1(indexed)
		if encodeErr != nil {
			return fail(FreshAdmissionReadIndex, encodeErr)
		}
		if indexed.RegisteredAt.Equal(canonical.RegisteredAt) ||
			!bytes.Equal(claimBytes, canonicalEncoding) ||
			!bytes.Equal(indexBytes, indexEncoding) {
			return fail(
				FreshAdmissionReadIndex,
				fmt.Errorf("expected run claim/index byte mismatch is not registered_at drift"),
			)
		}
		writeIndex = true
	case indexPresent:
		indexed, inspectErr := inspectExpectedRunPhysicalRecord(
			indexBytes,
			workspaceID,
			runID,
			indexKey,
			false,
		)
		if inspectErr != nil {
			return fail(FreshAdmissionReadIndex, inspectErr)
		}
		if identityErr := compareExpectedRunIdentity(indexed, request.Record); identityErr != nil {
			return fail(FreshAdmissionReadIndex, identityErr)
		}
		canonical = indexed
		canonicalBytes = indexBytes
		writeClaim = true
	default:
		canonical = request.Record
		writeClaim = true
		writeIndex = true
	}

	existingLease, leaseWasPresent, err := leaseStore.ReadAttemptLeaseForUpdate(
		ctx,
		tx,
		workspaceID,
		runID,
	)
	if err != nil {
		if receiptPresent {
			return fail(
				FreshAdmissionReadReceipt,
				a4AdmissionReceiptConflict("read certified initial lease: %v", err),
			)
		}
		return fail(FreshAdmissionWriteLease, err)
	}
	if receiptPresent {
		if !claimPresent || !indexPresent || !leaseWasPresent {
			return fail(
				FreshAdmissionReadReceipt,
				a4AdmissionReceiptConflict(
					"certified claim/index/initial lease must all be present",
				),
			)
		}
		if !bytes.Equal(indexBytes, canonicalBytes) {
			return fail(
				FreshAdmissionReadReceipt,
				a4AdmissionReceiptConflict(
					"certified claim/index bytes are not identical",
				),
			)
		}
		storedReceipt, decodeErr := decodeA4AdmissionReceiptV1(receiptBytes)
		if decodeErr != nil {
			return fail(
				FreshAdmissionReadReceipt,
				a4AdmissionReceiptConflict("decode stored receipt: %v", decodeErr),
			)
		}
		if bindingErr := validateA4AdmissionReceiptBinding(
			storedReceipt,
			receiptNamespace,
			receiptKey,
			canonical,
			canonicalBytes,
			indexKey,
			existingLease,
		); bindingErr != nil {
			return fail(FreshAdmissionReadReceipt, bindingErr)
		}
		canonicalReceipt, encodeErr := encodeA4AdmissionReceiptV1(storedReceipt)
		if encodeErr != nil {
			return fail(
				FreshAdmissionReadReceipt,
				a4AdmissionReceiptConflict("encode stored receipt: %v", encodeErr),
			)
		}
		if !bytes.Equal(receiptBytes, canonicalReceipt) {
			return fail(
				FreshAdmissionReadReceipt,
				a4AdmissionReceiptConflict("stored receipt bytes are not canonical"),
			)
		}
	}
	if !claimPresent && leaseWasPresent {
		return fail(FreshAdmissionWriteLease, ErrOrphanAttemptLease)
	}

	if writeClaim {
		if err := registry.txStore.PutValueTx(
			ctx,
			tx,
			namespace,
			claimKey,
			canonicalBytes,
		); err != nil {
			return fail(FreshAdmissionWriteClaim, err)
		}
	}
	if writeIndex {
		if err := registry.txStore.PutValueTx(
			ctx,
			tx,
			namespace,
			indexKey,
			canonicalBytes,
		); err != nil {
			return fail(FreshAdmissionWriteIndex, err)
		}
	}
	if err := ensureExpectedRunDomainIndexTx(ctx, tx, canonical); err != nil {
		return fail(FreshAdmissionWriteIndex, err)
	}

	lease, err := leaseStore.CreateInitialAttemptLease(
		ctx,
		tx,
		workspaceID,
		runID,
		request.AttemptID,
		request.GraphName,
		request.RunStartedAt,
		request.LeaseTTL,
	)
	if err != nil {
		return fail(FreshAdmissionWriteLease, err)
	}

	desiredReceipt, err := newA4AdmissionReceiptV1(
		canonical,
		canonicalBytes,
		indexKey,
		lease,
	)
	if err != nil {
		return fail(FreshAdmissionWriteReceipt, err)
	}
	desiredReceiptBytes, err := encodeA4AdmissionReceiptV1(desiredReceipt)
	if err != nil {
		return fail(FreshAdmissionWriteReceipt, err)
	}
	if receiptPresent {
		if !bytes.Equal(receiptBytes, desiredReceiptBytes) {
			return fail(
				FreshAdmissionReadReceipt,
				a4AdmissionReceiptConflict(
					"stored receipt does not match requested certified admission",
				),
			)
		}
	} else {
		inserted, insertErr := registry.insertStore.InsertValueTx(
			ctx,
			tx,
			receiptNamespace,
			receiptKey,
			desiredReceiptBytes,
		)
		if insertErr != nil {
			return fail(FreshAdmissionWriteReceipt, insertErr)
		}
		if !inserted {
			insertedBytes, insertedPresent, readErr := registry.txStore.ReadValueTx(
				ctx,
				tx,
				receiptNamespace,
				receiptKey,
			)
			if readErr != nil {
				return fail(FreshAdmissionWriteReceipt, readErr)
			}
			if !insertedPresent || !bytes.Equal(insertedBytes, desiredReceiptBytes) {
				return fail(
					FreshAdmissionWriteReceipt,
					a4AdmissionReceiptConflict(
						"lost receipt insert did not preserve identical bytes",
					),
				)
			}
		}
	}

	verifiedClaim, verifiedClaimPresent, err := registry.txStore.ReadValueTx(
		ctx,
		tx,
		namespace,
		claimKey,
	)
	if err != nil {
		return fail(FreshAdmissionVerifyReceipt, err)
	}
	verifiedIndex, verifiedIndexPresent, err := registry.txStore.ReadValueTx(
		ctx,
		tx,
		namespace,
		indexKey,
	)
	if err != nil {
		return fail(FreshAdmissionVerifyReceipt, err)
	}
	verifiedReceipt, verifiedReceiptPresent, err := registry.txStore.ReadValueTx(
		ctx,
		tx,
		receiptNamespace,
		receiptKey,
	)
	if err != nil {
		return fail(FreshAdmissionVerifyReceipt, err)
	}
	if !verifiedClaimPresent || !verifiedIndexPresent || !verifiedReceiptPresent ||
		!bytes.Equal(verifiedClaim, canonicalBytes) ||
		!bytes.Equal(verifiedIndex, canonicalBytes) ||
		!bytes.Equal(verifiedReceipt, desiredReceiptBytes) {
		return fail(
			FreshAdmissionVerifyReceipt,
			a4AdmissionReceiptConflict(
				"expected run claim/index/receipt verification mismatch",
			),
		)
	}
	if _, err := inspectExpectedRunPhysicalRecord(
		verifiedClaim,
		workspaceID,
		runID,
		claimKey,
		true,
	); err != nil {
		return fail(FreshAdmissionVerifyReceipt, err)
	}
	if _, err := inspectExpectedRunPhysicalRecord(
		verifiedIndex,
		workspaceID,
		runID,
		indexKey,
		false,
	); err != nil {
		return fail(FreshAdmissionVerifyReceipt, err)
	}
	verifiedLease, verifiedLeasePresent, err := leaseStore.ReadAttemptLeaseForUpdate(
		ctx,
		tx,
		workspaceID,
		runID,
	)
	if err != nil {
		return fail(FreshAdmissionVerifyReceipt, err)
	}
	if !verifiedLeasePresent || !reflect.DeepEqual(lease, verifiedLease) {
		return fail(
			FreshAdmissionVerifyReceipt,
			a4AdmissionReceiptConflict("attempt lease verification mismatch"),
		)
	}
	decodedVerifiedReceipt, err := decodeA4AdmissionReceiptV1(verifiedReceipt)
	if err != nil {
		return fail(
			FreshAdmissionVerifyReceipt,
			a4AdmissionReceiptConflict("decode verified receipt: %v", err),
		)
	}
	if err := validateA4AdmissionReceiptBinding(
		decodedVerifiedReceipt,
		receiptNamespace,
		receiptKey,
		canonical,
		canonicalBytes,
		indexKey,
		verifiedLease,
	); err != nil {
		return fail(FreshAdmissionVerifyReceipt, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fail(FreshAdmissionCommit, err)
	}
	return lease, nil
}

func (registry *expectedRunRegistry) Get(
	ctx context.Context,
	workspaceID string,
	runID string,
) (ExpectedRunRecordV1, bool, error) {
	if workspaceID == "" || runID == "" {
		return ExpectedRunRecordV1{}, false, fmt.Errorf("expected run workspace_id and run_id are required")
	}
	key := expectedRunClaimKey(runID)
	data, present, err := registry.store.ReadValue(ctx, expectedRunNamespace(workspaceID), key)
	if err != nil {
		return ExpectedRunRecordV1{}, false, fmt.Errorf("read expected run claim: %w", err)
	}
	if !present {
		return ExpectedRunRecordV1{}, false, nil
	}
	record, err := inspectExpectedRunPhysicalRecord(data, workspaceID, runID, key, true)
	if err != nil {
		return ExpectedRunRecordV1{}, false, fmt.Errorf("inspect expected run claim: %w", err)
	}
	return record, true, nil
}

type expectedRunDomainIndexRow struct {
	WorkspaceID     string
	RunID           string
	TeamID          *string
	WorkflowID      *string
	WorkflowVersion *int64
	RunSnapshotID   *string
}

func ensureExpectedRunDomainIndexTx(
	ctx context.Context,
	tx pgx.Tx,
	record ExpectedRunRecordV1,
) error {
	if _, err := tx.Exec(ctx, `INSERT INTO weave_expected_run_domain_index (
		workspace_id, run_id, team_id, workflow_id, workflow_version,
		run_snapshot_id, created_at
	) VALUES ($1, $2, $3, $4, $5, $6, statement_timestamp())
	ON CONFLICT (workspace_id, run_id) DO NOTHING`,
		record.WorkspaceID,
		record.RunID,
		record.TeamID,
		record.WorkflowID,
		record.WorkflowVersion,
		record.RunSnapshotID,
	); err != nil {
		return fmt.Errorf("insert expected run domain row: %w", err)
	}

	stored, err := scanExpectedRunDomainIndexRow(tx.QueryRow(ctx, `SELECT
		workspace_id, run_id, team_id, workflow_id, workflow_version, run_snapshot_id
	FROM weave_expected_run_domain_index
	WHERE workspace_id=$1 AND run_id=$2
	FOR UPDATE`, record.WorkspaceID, record.RunID))
	if err != nil {
		return fmt.Errorf("read expected run domain row: %w", err)
	}
	if !expectedRunDomainIndexMatchesRecord(stored, record) {
		return fmt.Errorf(
			"%w for workspace %q run %q: durable domain index mismatch",
			ErrExpectedRunConflict,
			record.WorkspaceID,
			record.RunID,
		)
	}
	return nil
}

func scanExpectedRunDomainIndexRow(row pgx.Row) (expectedRunDomainIndexRow, error) {
	var record expectedRunDomainIndexRow
	err := row.Scan(
		&record.WorkspaceID,
		&record.RunID,
		&record.TeamID,
		&record.WorkflowID,
		&record.WorkflowVersion,
		&record.RunSnapshotID,
	)
	return record, err
}

func expectedRunDomainIndexMatchesRecord(
	indexed expectedRunDomainIndexRow,
	record ExpectedRunRecordV1,
) bool {
	want := expectedRunDomainIndexRow{
		WorkspaceID:   record.WorkspaceID,
		RunID:         record.RunID,
		TeamID:        record.TeamID,
		WorkflowID:    record.WorkflowID,
		RunSnapshotID: record.RunSnapshotID,
	}
	if record.WorkflowVersion != nil {
		version := int64(*record.WorkflowVersion)
		want.WorkflowVersion = &version
	}
	return reflect.DeepEqual(indexed, want)
}

func (registry *expectedRunRegistry) ListByTeam(
	ctx context.Context,
	workspaceID string,
	teamID string,
) ([]ExpectedRunRecordV1, error) {
	if workspaceID == "" || teamID == "" {
		return nil, fmt.Errorf("expected run workspace_id and team_id are required")
	}
	return registry.listByDurableDomainIndex(
		ctx,
		"team",
		`SELECT workspace_id, run_id, team_id, workflow_id, workflow_version, run_snapshot_id
		 FROM weave_expected_run_domain_index
		 WHERE workspace_id=$1 AND team_id=$2
		 ORDER BY run_id`,
		workspaceID,
		teamID,
	)
}

func (registry *expectedRunRegistry) ListByWorkflow(
	ctx context.Context,
	workspaceID string,
	workflowID string,
	version int,
) ([]ExpectedRunRecordV1, error) {
	if workspaceID == "" || workflowID == "" || version < 1 {
		return nil, fmt.Errorf(
			"expected run workspace_id, workflow_id, and positive workflow_version are required",
		)
	}
	return registry.listByDurableDomainIndex(
		ctx,
		"workflow",
		`SELECT workspace_id, run_id, team_id, workflow_id, workflow_version, run_snapshot_id
		 FROM weave_expected_run_domain_index
		 WHERE workspace_id=$1 AND workflow_id=$2 AND workflow_version=$3
		 ORDER BY run_id`,
		workspaceID,
		workflowID,
		version,
	)
}

func (registry *expectedRunRegistry) listByDurableDomainIndex(
	ctx context.Context,
	domain string,
	query string,
	args ...any,
) ([]ExpectedRunRecordV1, error) {
	if registry == nil || registry.txStore == nil {
		return nil, fmt.Errorf("list expected runs by %s requires postgres registry store", domain)
	}
	tx, err := registry.txStore.BeginTx(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin expected run %s listing: %w", domain, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query expected run %s domain index: %w", domain, err)
	}
	indexed := make([]expectedRunDomainIndexRow, 0)
	for rows.Next() {
		var row expectedRunDomainIndexRow
		if err := rows.Scan(
			&row.WorkspaceID,
			&row.RunID,
			&row.TeamID,
			&row.WorkflowID,
			&row.WorkflowVersion,
			&row.RunSnapshotID,
		); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan expected run %s domain index: %w", domain, err)
		}
		indexed = append(indexed, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read expected run %s domain index: %w", domain, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit expected run %s listing: %w", domain, err)
	}

	records := make([]ExpectedRunRecordV1, 0, len(indexed))
	for _, row := range indexed {
		record, present, err := registry.Get(ctx, row.WorkspaceID, row.RunID)
		if err != nil {
			return nil, fmt.Errorf(
				"read authoritative expected run claim for %s index %q: %w",
				domain,
				row.RunID,
				err,
			)
		}
		if !present {
			return nil, fmt.Errorf(
				"expected run %s domain index %q has no authoritative claim",
				domain,
				row.RunID,
			)
		}
		if !expectedRunDomainIndexMatchesRecord(row, record) {
			return nil, fmt.Errorf(
				"expected run %s domain index mismatch for run %q",
				domain,
				row.RunID,
			)
		}
		records = append(records, record)
	}
	return records, nil
}

func (registry *expectedRunRegistry) ListBySnapshot(
	ctx context.Context,
	workspaceID string,
	runSnapshotID string,
) ([]ExpectedRunRecordV1, error) {
	if workspaceID == "" || runSnapshotID == "" {
		return nil, fmt.Errorf("expected run workspace_id and run_snapshot_id are required")
	}
	namespace := expectedRunNamespace(workspaceID)
	prefix := expectedRunSnapshotPrefix(runSnapshotID)
	keys, err := registry.store.ListKeys(ctx, namespace)
	if err != nil {
		return nil, fmt.Errorf("list expected run domain keys: %w", err)
	}
	records := make([]ExpectedRunRecordV1, 0)
	seen := make(map[string]struct{})
	for _, key := range keys {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		data, present, err := registry.store.ReadValue(ctx, namespace, key)
		if err != nil {
			return nil, fmt.Errorf("read expected run domain index %q: %w", key, err)
		}
		if !present {
			return nil, fmt.Errorf("expected run domain index %q disappeared during listing", key)
		}
		record, err := decodeExpectedRunRecordV1(data)
		if err != nil {
			return nil, fmt.Errorf("decode expected run domain index %q: %w", key, err)
		}
		if record.WorkspaceID != workspaceID || record.RunSnapshotID == nil ||
			*record.RunSnapshotID != runSnapshotID || expectedRunDomainKey(record) != key {
			return nil, fmt.Errorf("expected run domain index %q physical identity mismatch", key)
		}
		if _, duplicate := seen[record.RunID]; duplicate {
			return nil, fmt.Errorf("duplicate expected run %q in snapshot domain", record.RunID)
		}
		claimKey := expectedRunClaimKey(record.RunID)
		claimBytes, claimPresent, err := registry.store.ReadValue(ctx, namespace, claimKey)
		if err != nil {
			return nil, fmt.Errorf("read expected run claim for index %q: %w", key, err)
		}
		if !claimPresent {
			return nil, fmt.Errorf("expected run domain index %q has no authoritative claim", key)
		}
		claim, err := inspectExpectedRunPhysicalRecord(
			claimBytes,
			workspaceID,
			record.RunID,
			claimKey,
			true,
		)
		if err != nil {
			return nil, fmt.Errorf("inspect expected run claim for index %q: %w", key, err)
		}
		if err := compareExpectedRunIdentity(record, claim); err != nil || !bytes.Equal(data, claimBytes) {
			return nil, fmt.Errorf("expected run claim/index mismatch for run %q", record.RunID)
		}
		seen[record.RunID] = struct{}{}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].RunID < records[j].RunID })
	return records, nil
}

func expectedRunNamespace(workspaceID string) string {
	return "runreg:" + workspaceID
}

func expectedRunClaimKey(runID string) string {
	return "v1/id/" + expectedRunEncodeKeyComponent(runID)
}

func expectedRunSnapshotPrefix(runSnapshotID string) string {
	return "v1/snapshot/" + expectedRunEncodeKeyComponent(runSnapshotID) + "/run/"
}

func expectedRunDomainKey(record ExpectedRunRecordV1) string {
	if record.RunSnapshotID != nil {
		return expectedRunSnapshotPrefix(*record.RunSnapshotID) + expectedRunEncodeKeyComponent(record.RunID)
	}
	return "v1/legacy_unattributed/run/" + expectedRunEncodeKeyComponent(record.RunID)
}

func expectedRunEncodeKeyComponent(value string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func inspectExpectedRunPhysicalRecord(
	data []byte,
	workspaceID string,
	runID string,
	key string,
	claim bool,
) (ExpectedRunRecordV1, error) {
	record, err := decodeExpectedRunRecordV1(data)
	if err != nil {
		return ExpectedRunRecordV1{}, err
	}
	if record.WorkspaceID != workspaceID || record.RunID != runID {
		return ExpectedRunRecordV1{}, fmt.Errorf("expected run physical workspace/run identity mismatch")
	}
	wantKey := expectedRunDomainKey(record)
	if claim {
		wantKey = expectedRunClaimKey(record.RunID)
	}
	if key != wantKey {
		return ExpectedRunRecordV1{}, fmt.Errorf("expected run physical key mismatch: got %q want %q", key, wantKey)
	}
	return record, nil
}

func compareExpectedRunIdentity(left, right ExpectedRunRecordV1) error {
	left.RegisteredAt = time.Time{}
	right.RegisteredAt = time.Time{}
	if !reflect.DeepEqual(left, right) {
		return fmt.Errorf("%w for workspace %q run %q", ErrExpectedRunConflict, right.WorkspaceID, right.RunID)
	}
	return nil
}

type expectedRunRecordWireV1 struct {
	SchemaVersion          int                      `json:"schema_version"`
	WorkspaceID            string                   `json:"workspace_id"`
	RunID                  string                   `json:"run_id"`
	Agent                  string                   `json:"agent"`
	AttributionScope       TerminalAttributionScope `json:"attribution_scope"`
	TeamID                 *string                  `json:"team_id"`
	WorkflowID             *string                  `json:"workflow_id"`
	WorkflowVersion        *int                     `json:"workflow_version"`
	RunSnapshotID          *string                  `json:"run_snapshot_id"`
	ParentRunID            *string                  `json:"parent_run_id"`
	ParentSeq              *int64                   `json:"parent_seq"`
	AggregationParentRunID *string                  `json:"aggregation_parent_run_id"`
	TaskGroupID            *string                  `json:"task_group_id"`
	RegisteredAt           string                   `json:"registered_at"`
}

func encodeExpectedRunRecordV1(record ExpectedRunRecordV1) ([]byte, error) {
	if err := validateExpectedRunRecordV1(record); err != nil {
		return nil, err
	}
	wire := expectedRunRecordWireV1{
		SchemaVersion: record.SchemaVersion, WorkspaceID: record.WorkspaceID,
		RunID: record.RunID, Agent: record.Agent, AttributionScope: record.AttributionScope,
		TeamID: record.TeamID, WorkflowID: record.WorkflowID, WorkflowVersion: record.WorkflowVersion,
		RunSnapshotID: record.RunSnapshotID, ParentRunID: record.ParentRunID,
		ParentSeq: record.ParentSeq, AggregationParentRunID: record.AggregationParentRunID,
		TaskGroupID: record.TaskGroupID, RegisteredAt: record.RegisteredAt.Format(time.RFC3339Nano),
	}
	data, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("marshal expected run record: %w", err)
	}
	return data, nil
}

func decodeExpectedRunRecordV1(data []byte) (ExpectedRunRecordV1, error) {
	fields, err := decodeExpectedRunObject(data)
	if err != nil {
		return ExpectedRunRecordV1{}, err
	}
	var wire expectedRunRecordWireV1
	if wire.SchemaVersion, err = decodeExpectedRunInt(fields["schema_version"]); err != nil {
		return ExpectedRunRecordV1{}, fmt.Errorf("decode schema_version: %w", err)
	}
	if wire.WorkspaceID, err = decodeExpectedRunString(fields["workspace_id"]); err != nil {
		return ExpectedRunRecordV1{}, fmt.Errorf("decode workspace_id: %w", err)
	}
	if wire.RunID, err = decodeExpectedRunString(fields["run_id"]); err != nil {
		return ExpectedRunRecordV1{}, fmt.Errorf("decode run_id: %w", err)
	}
	if wire.Agent, err = decodeExpectedRunString(fields["agent"]); err != nil {
		return ExpectedRunRecordV1{}, fmt.Errorf("decode agent: %w", err)
	}
	scope, err := decodeExpectedRunString(fields["attribution_scope"])
	if err != nil {
		return ExpectedRunRecordV1{}, fmt.Errorf("decode attribution_scope: %w", err)
	}
	wire.AttributionScope = TerminalAttributionScope(scope)
	if wire.TeamID, err = decodeExpectedRunOptionalString(fields["team_id"]); err != nil {
		return ExpectedRunRecordV1{}, fmt.Errorf("decode team_id: %w", err)
	}
	if wire.WorkflowID, err = decodeExpectedRunOptionalString(fields["workflow_id"]); err != nil {
		return ExpectedRunRecordV1{}, fmt.Errorf("decode workflow_id: %w", err)
	}
	if wire.WorkflowVersion, err = decodeExpectedRunOptionalInt(fields["workflow_version"]); err != nil {
		return ExpectedRunRecordV1{}, fmt.Errorf("decode workflow_version: %w", err)
	}
	if wire.RunSnapshotID, err = decodeExpectedRunOptionalString(fields["run_snapshot_id"]); err != nil {
		return ExpectedRunRecordV1{}, fmt.Errorf("decode run_snapshot_id: %w", err)
	}
	if wire.ParentRunID, err = decodeExpectedRunOptionalString(fields["parent_run_id"]); err != nil {
		return ExpectedRunRecordV1{}, fmt.Errorf("decode parent_run_id: %w", err)
	}
	if wire.ParentSeq, err = decodeExpectedRunOptionalInt64(fields["parent_seq"]); err != nil {
		return ExpectedRunRecordV1{}, fmt.Errorf("decode parent_seq: %w", err)
	}
	if wire.AggregationParentRunID, err = decodeExpectedRunOptionalString(fields["aggregation_parent_run_id"]); err != nil {
		return ExpectedRunRecordV1{}, fmt.Errorf("decode aggregation_parent_run_id: %w", err)
	}
	if wire.TaskGroupID, err = decodeExpectedRunOptionalString(fields["task_group_id"]); err != nil {
		return ExpectedRunRecordV1{}, fmt.Errorf("decode task_group_id: %w", err)
	}
	if wire.RegisteredAt, err = decodeExpectedRunString(fields["registered_at"]); err != nil {
		return ExpectedRunRecordV1{}, fmt.Errorf("decode registered_at: %w", err)
	}
	registeredAt, err := time.Parse(time.RFC3339Nano, wire.RegisteredAt)
	if err != nil || registeredAt.Location() != time.UTC || registeredAt.Format(time.RFC3339Nano) != wire.RegisteredAt {
		return ExpectedRunRecordV1{}, fmt.Errorf("decode registered_at: non-canonical UTC RFC3339Nano timestamp")
	}
	record := ExpectedRunRecordV1{
		SchemaVersion: wire.SchemaVersion, WorkspaceID: wire.WorkspaceID,
		RunID: wire.RunID, Agent: wire.Agent, AttributionScope: wire.AttributionScope,
		TeamID: wire.TeamID, WorkflowID: wire.WorkflowID, WorkflowVersion: wire.WorkflowVersion,
		RunSnapshotID: wire.RunSnapshotID, ParentRunID: wire.ParentRunID, ParentSeq: wire.ParentSeq,
		AggregationParentRunID: wire.AggregationParentRunID, TaskGroupID: wire.TaskGroupID,
		RegisteredAt: registeredAt,
	}
	if err := validateExpectedRunRecordV1(record); err != nil {
		return ExpectedRunRecordV1{}, fmt.Errorf("validate expected run record: %w", err)
	}
	return record, nil
}

func decodeExpectedRunObject(data []byte) (map[string]json.RawMessage, error) {
	allowed := map[string]struct{}{
		"schema_version": {}, "workspace_id": {}, "run_id": {}, "agent": {},
		"attribution_scope": {}, "team_id": {}, "workflow_id": {}, "workflow_version": {},
		"run_snapshot_id": {}, "parent_run_id": {}, "parent_seq": {},
		"aggregation_parent_run_id": {}, "task_group_id": {}, "registered_at": {},
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("expected run record must be one JSON object")
	}
	fields := make(map[string]json.RawMessage, len(allowed))
	for decoder.More() {
		rawName, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("decode expected run field name: %w", err)
		}
		name, ok := rawName.(string)
		if !ok {
			return nil, fmt.Errorf("expected run field name is not a string")
		}
		if _, ok := allowed[name]; !ok {
			return nil, fmt.Errorf("unknown expected run field %q", name)
		}
		if _, duplicate := fields[name]; duplicate {
			return nil, fmt.Errorf("duplicate expected run field %q", name)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode expected run field %q: %w", name, err)
		}
		fields[name] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, fmt.Errorf("close expected run object: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("expected run record has trailing JSON content")
	}
	if len(fields) != len(allowed) {
		missing := make([]string, 0)
		for name := range allowed {
			if _, ok := fields[name]; !ok {
				missing = append(missing, name)
			}
		}
		sort.Strings(missing)
		return nil, fmt.Errorf("expected run record missing fields: %s", strings.Join(missing, ", "))
	}
	return fields, nil
}

func decodeExpectedRunString(raw json.RawMessage) (string, error) {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", err
	}
	return value, nil
}

func decodeExpectedRunOptionalString(raw json.RawMessage) (*string, error) {
	if bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	value, err := decodeExpectedRunString(raw)
	if err != nil {
		return nil, err
	}
	return &value, nil
}

func decodeExpectedRunInt(raw json.RawMessage) (int, error) {
	parsed, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("expected exact integer: %w", err)
	}
	value := int(parsed)
	if int64(value) != parsed {
		return 0, fmt.Errorf("integer overflows int")
	}
	return value, nil
}

func decodeExpectedRunOptionalInt(raw json.RawMessage) (*int, error) {
	if bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	value, err := decodeExpectedRunInt(raw)
	if err != nil {
		return nil, err
	}
	return &value, nil
}

func decodeExpectedRunOptionalInt64(raw json.RawMessage) (*int64, error) {
	if bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	value, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("expected exact integer: %w", err)
	}
	return &value, nil
}

func validateExpectedRunRecordV1(record ExpectedRunRecordV1) error {
	if record.SchemaVersion != expectedRunRecordSchemaV1 {
		return fmt.Errorf("expected run schema_version must be %d", expectedRunRecordSchemaV1)
	}
	if record.WorkspaceID == "" || record.RunID == "" || record.Agent == "" {
		return fmt.Errorf("expected run workspace_id, run_id, and agent are required")
	}
	if record.RegisteredAt.IsZero() || record.RegisteredAt.Location() != time.UTC {
		return fmt.Errorf("expected run registered_at must be non-zero UTC")
	}
	for name, value := range map[string]*string{
		"team_id": record.TeamID, "workflow_id": record.WorkflowID,
		"run_snapshot_id": record.RunSnapshotID, "parent_run_id": record.ParentRunID,
		"aggregation_parent_run_id": record.AggregationParentRunID, "task_group_id": record.TaskGroupID,
	} {
		if value != nil && *value == "" {
			return fmt.Errorf("expected run %s must be non-empty when present", name)
		}
	}
	if (record.WorkflowID == nil) != (record.WorkflowVersion == nil) {
		return fmt.Errorf("expected run workflow_id and workflow_version must be paired")
	}
	if record.WorkflowVersion != nil && *record.WorkflowVersion < 1 {
		return fmt.Errorf("expected run workflow_version must be >= 1")
	}
	if (record.ParentRunID == nil) != (record.ParentSeq == nil) {
		return fmt.Errorf("expected run parent_run_id and parent_seq must be paired")
	}
	if record.ParentSeq != nil && *record.ParentSeq < 0 {
		return fmt.Errorf("expected run parent_seq must be non-negative")
	}
	if record.ParentRunID != nil && record.RunID == *record.ParentRunID {
		return fmt.Errorf("expected run run_id must differ from parent_run_id")
	}
	if record.AggregationParentRunID != nil &&
		(record.ParentRunID == nil || *record.AggregationParentRunID != *record.ParentRunID) {
		return fmt.Errorf("expected run aggregation_parent_run_id must equal parent_run_id")
	}

	switch record.AttributionScope {
	case TerminalAttributionFixedWorkflow:
		if record.TeamID == nil || record.WorkflowID == nil ||
			record.WorkflowVersion == nil || record.RunSnapshotID == nil {
			return fmt.Errorf("fixed_workflow expected run requires team, workflow pair, and snapshot")
		}
	case TerminalAttributionTeamFreeCollab:
		if record.TeamID == nil || record.RunSnapshotID == nil {
			return fmt.Errorf("team_free_collab expected run requires team and snapshot")
		}
		if record.WorkflowID != nil || record.WorkflowVersion != nil {
			return fmt.Errorf("team_free_collab expected run requires no workflow pair")
		}
	case TerminalAttributionLegacyUnattributed:
		if record.TeamID != nil || record.WorkflowID != nil || record.WorkflowVersion != nil ||
			record.RunSnapshotID != nil || record.TaskGroupID != nil {
			return fmt.Errorf("legacy_unattributed expected run cannot carry team associations")
		}
	default:
		return fmt.Errorf("unknown expected run attribution_scope %q", record.AttributionScope)
	}

	if record.RunSnapshotID != nil {
		if record.AggregationParentRunID == nil && record.RunID != *record.RunSnapshotID {
			return fmt.Errorf("team aggregation root run_id must equal run_snapshot_id")
		}
		if record.AggregationParentRunID != nil && record.RunID == *record.RunSnapshotID {
			return fmt.Errorf("team child run_id must differ from run_snapshot_id")
		}
	}
	return nil
}
