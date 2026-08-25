package loomruntime

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	ErrA4AttemptLeaseLifecycleUnsupported = errors.New(
		"A4 attempt lease lifecycle is unsupported",
	)
	ErrResumeAttemptConflict     = errors.New("resume attempt lease conflict")
	ErrAttemptLeaseOwnerConflict = errors.New(
		"attempt lease owner conflict",
	)
	ErrRunAlreadyTerminal = errors.New(
		"run already has a final terminal marker",
	)
)

type AttemptLeaseOwner struct {
	WorkspaceID  string
	RunID        string
	Generation   int64
	AttemptID    uuid.UUID
	GraphName    string
	RunStartedAt string
}

type ResumeAttemptAdmission struct {
	WorkspaceID string
	RunID       string
	GraphName   string
	AttemptID   uuid.UUID
	LeaseTTL    time.Duration
}

type AttemptLeaseLifecycle interface {
	A4AttemptLeaseLifecycleGuaranteed() bool
	AdmitResume(
		context.Context,
		ResumeAttemptAdmission,
	) (RunAttemptLease, error)
	MarkAttemptYielded(
		context.Context,
		AttemptLeaseOwner,
	) (RunAttemptLease, error)
}

type AttemptLeaseLifecycleOperation string
type AttemptLeaseLifecycleStage string
type AttemptLeaseHeartbeatHookStage string

const (
	AttemptLeaseAdmitResume               AttemptLeaseLifecycleOperation = "admit_resume"
	AttemptLeaseMarkAttemptYielded        AttemptLeaseLifecycleOperation = "mark_attempt_yielded"
	AttemptLeaseLifecycleValidate         AttemptLeaseLifecycleStage     = "validate"
	AttemptLeaseLifecycleBegin            AttemptLeaseLifecycleStage     = "begin"
	AttemptLeaseLifecycleLockRun          AttemptLeaseLifecycleStage     = "lock_run"
	AttemptLeaseLifecycleLockClaim        AttemptLeaseLifecycleStage     = "lock_claim"
	AttemptLeaseLifecycleReadClaim        AttemptLeaseLifecycleStage     = "read_claim"
	AttemptLeaseLifecycleReadMarker       AttemptLeaseLifecycleStage     = "read_marker"
	AttemptLeaseLifecycleReadLease        AttemptLeaseLifecycleStage     = "read_lease"
	AttemptLeaseLifecycleWriteLease       AttemptLeaseLifecycleStage     = "write_lease"
	AttemptLeaseLifecycleVerify           AttemptLeaseLifecycleStage     = "verify"
	AttemptLeaseLifecycleCommit           AttemptLeaseLifecycleStage     = "commit"
	AttemptLeaseHeartbeatHookLocked       AttemptLeaseHeartbeatHookStage = "heartbeat_locked"
	AttemptLeaseHeartbeatHookCommitBefore AttemptLeaseHeartbeatHookStage = "heartbeat_commit_before"
	AttemptLeaseHeartbeatHookCommitAfter  AttemptLeaseHeartbeatHookStage = "heartbeat_commit_after"
)

type attemptLeaseHeartbeatHookStore interface {
	RunAttemptLeaseHeartbeatHook(
		context.Context,
		AttemptLeaseHeartbeatHookStage,
	) error
}

type AttemptLeaseLifecycleError struct {
	Operation   AttemptLeaseLifecycleOperation
	Stage       AttemptLeaseLifecycleStage
	WorkspaceID string
	RunID       string
	Err         error
}

func (err *AttemptLeaseLifecycleError) Error() string {
	if err == nil {
		return "attempt lease lifecycle failed"
	}
	if err.Err == nil {
		return fmt.Sprintf(
			"attempt lease lifecycle %s failed at %s for workspace %q run %q",
			err.Operation,
			err.Stage,
			err.WorkspaceID,
			err.RunID,
		)
	}
	return fmt.Sprintf(
		"attempt lease lifecycle %s failed at %s for workspace %q run %q: %v",
		err.Operation,
		err.Stage,
		err.WorkspaceID,
		err.RunID,
		err.Err,
	)
}

type nestedResumeAttemptAdmitter interface {
	admitResumeExpected(
		context.Context,
		ResumeAttemptAdmission,
		ExpectedRunRecordV1,
	) (RunAttemptLease, error)
}

func (err *AttemptLeaseLifecycleError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Err
}

func validateAttemptLeaseOwner(owner AttemptLeaseOwner) error {
	if owner.WorkspaceID == "" || owner.RunID == "" {
		return fmt.Errorf("physical key must be non-empty")
	}
	if owner.Generation < 1 {
		return fmt.Errorf("generation must be >= 1")
	}
	if owner.AttemptID == uuid.Nil {
		return fmt.Errorf("attempt_id must be non-zero")
	}
	if owner.GraphName == "" {
		return fmt.Errorf("graph_name must be non-empty")
	}
	return canonicalRunStartedAt(owner.RunStartedAt)
}

func (registry *expectedRunRegistry) A4AttemptLeaseLifecycleGuaranteed() bool {
	return registry != nil && registry.txStore != nil
}

func (registry *expectedRunRegistry) A4AttemptLeaseHeartbeatGuaranteed() bool {
	return registry != nil && registry.txStore != nil
}

func (registry *expectedRunRegistry) RenewAttempt(
	ctx context.Context,
	owner AttemptLeaseOwner,
	ttl time.Duration,
) (AttemptLeaseRenewal, error) {
	fail := func(
		stage AttemptLeaseHeartbeatStage,
		err error,
	) (AttemptLeaseRenewal, error) {
		return AttemptLeaseRenewal{}, &AttemptLeaseHeartbeatError{
			Stage:       stage,
			WorkspaceID: owner.WorkspaceID,
			RunID:       owner.RunID,
			Generation:  owner.Generation,
			AttemptID:   owner.AttemptID,
			Err:         err,
		}
	}
	if err := validateAttemptLeaseOwner(owner); err != nil {
		return fail(AttemptLeaseHeartbeatValidate, err)
	}
	if ttl != 120*time.Second {
		return fail(
			AttemptLeaseHeartbeatValidate,
			fmt.Errorf("lease_ttl must equal 120 seconds"),
		)
	}
	if registry == nil || registry.txStore == nil {
		return fail(
			AttemptLeaseHeartbeatValidate,
			ErrA4AttemptLeaseHeartbeatUnsupported,
		)
	}
	tx, err := registry.txStore.BeginTx(ctx)
	if err != nil {
		return fail(AttemptLeaseHeartbeatBegin, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	stateStore := NewPGTerminalStateStore()
	current, present, err := stateStore.ReadAttemptLeaseForUpdate(
		ctx,
		tx,
		owner.WorkspaceID,
		owner.RunID,
	)
	if err != nil {
		return fail(AttemptLeaseHeartbeatRenew, err)
	}
	if !present ||
		current.AttemptGeneration != owner.Generation ||
		current.AttemptID != owner.AttemptID ||
		current.GraphName != owner.GraphName ||
		current.RunStartedAt != owner.RunStartedAt ||
		current.State != AttemptLeaseActive {
		return fail(
			AttemptLeaseHeartbeatOwnerLost,
			ErrAttemptLeaseHeartbeatOwnerLost,
		)
	}
	if err := registry.runAttemptLeaseHeartbeatHook(
		ctx,
		AttemptLeaseHeartbeatHookLocked,
	); err != nil {
		return fail(AttemptLeaseHeartbeatRenew, err)
	}
	heartbeatAt, leaseExpiresAt, renewed, err := stateStore.RenewAttemptLease(
		ctx,
		tx,
		owner.WorkspaceID,
		owner.RunID,
		owner.Generation,
		owner.AttemptID,
		ttl,
	)
	if err != nil {
		return fail(AttemptLeaseHeartbeatRenew, err)
	}
	if !renewed {
		return fail(
			AttemptLeaseHeartbeatOwnerLost,
			ErrAttemptLeaseHeartbeatOwnerLost,
		)
	}
	if err := registry.runAttemptLeaseHeartbeatHook(
		ctx,
		AttemptLeaseHeartbeatHookCommitBefore,
	); err != nil {
		return fail(AttemptLeaseHeartbeatCommit, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fail(AttemptLeaseHeartbeatCommit, err)
	}
	if err := registry.runAttemptLeaseHeartbeatHook(
		ctx,
		AttemptLeaseHeartbeatHookCommitAfter,
	); err != nil {
		return fail(AttemptLeaseHeartbeatCommit, err)
	}
	return AttemptLeaseRenewal{
		HeartbeatAt:    heartbeatAt,
		LeaseExpiresAt: leaseExpiresAt,
	}, nil
}

func (registry *expectedRunRegistry) runAttemptLeaseHeartbeatHook(
	ctx context.Context,
	stage AttemptLeaseHeartbeatHookStage,
) error {
	if registry == nil {
		return nil
	}
	hook, ok := registry.txStore.(attemptLeaseHeartbeatHookStore)
	if !ok {
		return nil
	}
	return hook.RunAttemptLeaseHeartbeatHook(ctx, stage)
}

func (registry *expectedRunRegistry) AdmitResume(
	ctx context.Context,
	request ResumeAttemptAdmission,
) (RunAttemptLease, error) {
	return registry.admitResume(ctx, request, nil)
}

func (registry *expectedRunRegistry) admitResumeExpected(
	ctx context.Context,
	request ResumeAttemptAdmission,
	expected ExpectedRunRecordV1,
) (RunAttemptLease, error) {
	return registry.admitResume(ctx, request, &expected)
}

func (registry *expectedRunRegistry) admitResume(
	ctx context.Context,
	request ResumeAttemptAdmission,
	expected *ExpectedRunRecordV1,
) (RunAttemptLease, error) {
	fail := func(stage AttemptLeaseLifecycleStage, err error) (RunAttemptLease, error) {
		return RunAttemptLease{}, &AttemptLeaseLifecycleError{
			Operation:   AttemptLeaseAdmitResume,
			Stage:       stage,
			WorkspaceID: request.WorkspaceID,
			RunID:       request.RunID,
			Err:         err,
		}
	}
	if request.WorkspaceID == "" || request.RunID == "" {
		return fail(AttemptLeaseLifecycleValidate, fmt.Errorf("physical key must be non-empty"))
	}
	if request.GraphName == "" {
		return fail(AttemptLeaseLifecycleValidate, fmt.Errorf("graph_name must be non-empty"))
	}
	if request.AttemptID == uuid.Nil {
		return fail(AttemptLeaseLifecycleValidate, fmt.Errorf("attempt_id must be non-zero"))
	}
	if request.LeaseTTL != 120*time.Second {
		return fail(
			AttemptLeaseLifecycleValidate,
			fmt.Errorf("lease_ttl must equal 120 seconds"),
		)
	}
	if registry == nil || registry.txStore == nil {
		return fail(AttemptLeaseLifecycleValidate, ErrA4AttemptLeaseLifecycleUnsupported)
	}

	namespace := expectedRunNamespace(request.WorkspaceID)
	claimKey := expectedRunClaimKey(request.RunID)
	tx, err := registry.txStore.BeginTx(ctx)
	if err != nil {
		return fail(AttemptLeaseLifecycleBegin, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	leaseStore := NewPGTerminalStateStore()
	if err := leaseStore.LockTerminalRun(
		ctx,
		tx,
		request.WorkspaceID,
		request.RunID,
	); err != nil {
		return fail(AttemptLeaseLifecycleLockRun, err)
	}
	if err := registry.txStore.LockValueTx(
		ctx,
		tx,
		namespace,
		claimKey,
	); err != nil {
		return fail(AttemptLeaseLifecycleLockClaim, err)
	}
	claimBytes, claimPresent, err := registry.txStore.ReadValueTx(
		ctx,
		tx,
		namespace,
		claimKey,
	)
	if err != nil {
		return fail(AttemptLeaseLifecycleReadClaim, err)
	}
	marker, markerPresent, err := leaseStore.ReadTerminalMarkerForUpdate(
		ctx,
		tx,
		request.WorkspaceID,
		request.RunID,
	)
	if err != nil {
		return fail(AttemptLeaseLifecycleReadMarker, err)
	}
	if err := lockAttemptLeaseRow(
		ctx,
		tx,
		request.WorkspaceID,
		request.RunID,
	); err != nil {
		return fail(AttemptLeaseLifecycleReadLease, err)
	}
	if markerPresent && marker.Phase == TerminalMarkerPhaseFinal {
		return fail(AttemptLeaseLifecycleReadMarker, ErrRunAlreadyTerminal)
	}
	current, leasePresent, err := leaseStore.ReadAttemptLeaseForUpdate(
		ctx,
		tx,
		request.WorkspaceID,
		request.RunID,
	)
	if err != nil {
		return fail(AttemptLeaseLifecycleReadLease, err)
	}

	if !claimPresent {
		if leasePresent {
			return fail(AttemptLeaseLifecycleReadClaim, ErrOrphanAttemptLease)
		}
		return fail(AttemptLeaseLifecycleReadClaim, ErrResumeAttemptConflict)
	}
	if !leasePresent {
		return fail(AttemptLeaseLifecycleReadLease, ErrResumeAttemptConflict)
	}
	claim, err := inspectExpectedRunPhysicalRecord(
		claimBytes,
		request.WorkspaceID,
		request.RunID,
		claimKey,
		true,
	)
	if err != nil {
		return fail(AttemptLeaseLifecycleReadClaim, err)
	}
	if expected != nil {
		if err := compareExpectedRunIdentity(*expected, claim); err != nil {
			return fail(
				AttemptLeaseLifecycleVerify,
				fmt.Errorf("%w: nested child identity mismatch: %v", ErrResumeAttemptConflict, err),
			)
		}
	}
	if err := verifyResumeAttemptState(request, claim, current, marker, markerPresent); err != nil {
		return fail(AttemptLeaseLifecycleVerify, err)
	}

	next, updated, err := leaseStore.ActivateResumeAttemptLease(
		ctx,
		tx,
		attemptLeaseOwner(current),
		request.AttemptID,
		request.LeaseTTL,
	)
	if err != nil {
		return fail(AttemptLeaseLifecycleWriteLease, err)
	}
	if !updated {
		return fail(AttemptLeaseLifecycleWriteLease, ErrResumeAttemptConflict)
	}
	verified, verifiedPresent, err := leaseStore.ReadAttemptLeaseForUpdate(
		ctx,
		tx,
		request.WorkspaceID,
		request.RunID,
	)
	if err != nil {
		return fail(AttemptLeaseLifecycleVerify, err)
	}
	if !verifiedPresent || !reflect.DeepEqual(next, verified) {
		return fail(
			AttemptLeaseLifecycleVerify,
			fmt.Errorf("attempt lease verification mismatch"),
		)
	}
	if err := tx.Commit(ctx); err != nil {
		return fail(AttemptLeaseLifecycleCommit, err)
	}
	return next, nil
}

func lockAttemptLeaseRow(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID string,
	runID string,
) error {
	var locked int
	err := tx.QueryRow(
		ctx,
		`SELECT 1 FROM weave_run_attempt_leases
		 WHERE workspace_id=$1 AND run_id=$2
		 FOR UPDATE`,
		workspaceID,
		runID,
	).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

func (registry *expectedRunRegistry) MarkAttemptYielded(
	ctx context.Context,
	owner AttemptLeaseOwner,
) (RunAttemptLease, error) {
	fail := func(stage AttemptLeaseLifecycleStage, err error) (RunAttemptLease, error) {
		return RunAttemptLease{}, &AttemptLeaseLifecycleError{
			Operation:   AttemptLeaseMarkAttemptYielded,
			Stage:       stage,
			WorkspaceID: owner.WorkspaceID,
			RunID:       owner.RunID,
			Err:         err,
		}
	}
	if err := validateAttemptLeaseOwner(owner); err != nil {
		return fail(AttemptLeaseLifecycleValidate, err)
	}
	if registry == nil || registry.txStore == nil {
		return fail(AttemptLeaseLifecycleValidate, ErrA4AttemptLeaseLifecycleUnsupported)
	}
	tx, err := registry.txStore.BeginTx(ctx)
	if err != nil {
		return fail(AttemptLeaseLifecycleBegin, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	leaseStore := NewPGTerminalStateStore()
	if err := leaseStore.LockTerminalRun(
		ctx,
		tx,
		owner.WorkspaceID,
		owner.RunID,
	); err != nil {
		return fail(AttemptLeaseLifecycleLockRun, err)
	}
	current, present, err := leaseStore.ReadAttemptLeaseForUpdate(
		ctx,
		tx,
		owner.WorkspaceID,
		owner.RunID,
	)
	if err != nil {
		return fail(AttemptLeaseLifecycleReadLease, err)
	}
	if !present || current.State != AttemptLeaseActive ||
		!attemptLeaseOwnerEqual(current, owner) {
		return fail(AttemptLeaseLifecycleReadLease, ErrAttemptLeaseOwnerConflict)
	}
	yielded, updated, err := leaseStore.MarkAttemptLeaseYielded(ctx, tx, owner)
	if err != nil {
		return fail(AttemptLeaseLifecycleWriteLease, err)
	}
	if !updated {
		return fail(AttemptLeaseLifecycleWriteLease, ErrAttemptLeaseOwnerConflict)
	}
	verified, verifiedPresent, err := leaseStore.ReadAttemptLeaseForUpdate(
		ctx,
		tx,
		owner.WorkspaceID,
		owner.RunID,
	)
	if err != nil {
		return fail(AttemptLeaseLifecycleVerify, err)
	}
	if !verifiedPresent || !reflect.DeepEqual(yielded, verified) {
		return fail(
			AttemptLeaseLifecycleVerify,
			fmt.Errorf("attempt lease verification mismatch"),
		)
	}
	if err := tx.Commit(ctx); err != nil {
		return fail(AttemptLeaseLifecycleCommit, err)
	}
	return yielded, nil
}

func verifyResumeAttemptState(
	request ResumeAttemptAdmission,
	claim ExpectedRunRecordV1,
	lease RunAttemptLease,
	marker TerminalMarkerV1,
	markerPresent bool,
) error {
	if lease.WorkspaceID != request.WorkspaceID ||
		lease.RunID != request.RunID ||
		lease.GraphName != request.GraphName ||
		lease.State != AttemptLeaseYielded ||
		lease.AttemptGeneration == math.MaxInt64 ||
		request.AttemptID == lease.AttemptID {
		return ErrResumeAttemptConflict
	}
	if !markerPresent {
		return nil
	}
	if markerClass(marker) != "yielded" ||
		marker.AttemptGeneration != lease.AttemptGeneration ||
		marker.AttemptID != lease.AttemptID ||
		marker.RunStartedAt != lease.RunStartedAt ||
		(marker.CheckpointGraph != nil && *marker.CheckpointGraph != lease.GraphName) {
		return ErrResumeAttemptConflict
	}
	markerClaim := expectedRunRecordFromTerminalMarker(marker)
	if err := compareExpectedRunIdentity(markerClaim, claim); err != nil {
		return fmt.Errorf("%w: %v", ErrResumeAttemptConflict, err)
	}
	return nil
}

func expectedRunRecordFromTerminalMarker(marker TerminalMarkerV1) ExpectedRunRecordV1 {
	var workflowVersion *int
	if marker.WorkflowVersion != nil {
		value := int(*marker.WorkflowVersion)
		workflowVersion = &value
	}
	return ExpectedRunRecordV1{
		SchemaVersion:          int(marker.SchemaVersion),
		WorkspaceID:            marker.WorkspaceID,
		RunID:                  marker.RunID,
		Agent:                  marker.Agent,
		AttributionScope:       marker.AttributionScope,
		TeamID:                 marker.TeamID,
		WorkflowID:             marker.WorkflowID,
		WorkflowVersion:        workflowVersion,
		RunSnapshotID:          marker.RunSnapshotID,
		ParentRunID:            marker.ParentRunID,
		ParentSeq:              marker.ParentSeq,
		AggregationParentRunID: marker.AggregationParentRunID,
		TaskGroupID:            marker.TaskGroupID,
	}
}

func attemptLeaseOwner(lease RunAttemptLease) AttemptLeaseOwner {
	return AttemptLeaseOwner{
		WorkspaceID:  lease.WorkspaceID,
		RunID:        lease.RunID,
		Generation:   lease.AttemptGeneration,
		AttemptID:    lease.AttemptID,
		GraphName:    lease.GraphName,
		RunStartedAt: lease.RunStartedAt,
	}
}

func attemptLeaseOwnerEqual(lease RunAttemptLease, owner AttemptLeaseOwner) bool {
	return lease.WorkspaceID == owner.WorkspaceID &&
		lease.RunID == owner.RunID &&
		lease.AttemptGeneration == owner.Generation &&
		lease.AttemptID == owner.AttemptID &&
		lease.GraphName == owner.GraphName &&
		lease.RunStartedAt == owner.RunStartedAt
}
