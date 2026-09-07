package loomruntime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrA4NormalTerminalUnsupported = errors.New(
	"A4 normal terminal coordinator is unsupported",
)

type NormalTerminalCommit struct {
	Candidate TerminalEntryV3
	Owner     AttemptLeaseOwner
	// BeforeLock binds a member result and its parent fence to this same
	// terminal transaction. It runs before the member terminal locks.
	BeforeLock func(context.Context, pgx.Tx) error
}

type NormalTerminalCoordinator interface {
	A4NormalTerminalGuaranteed() bool
	CommitNormalTerminal(context.Context, NormalTerminalCommit) error
}

type NormalTerminalStage string

const (
	NormalTerminalStageRunLocked               NormalTerminalStage = "run_locked"
	NormalTerminalStageRegistryRead            NormalTerminalStage = "registry_read"
	NormalTerminalStageTerminalFactsLocked     NormalTerminalStage = "terminal_facts_locked"
	NormalTerminalStageAuditCompared           NormalTerminalStage = "audit_compared"
	NormalTerminalStageAuditWritten            NormalTerminalStage = "audit_written"
	NormalTerminalStageMarkerWritten           NormalTerminalStage = "marker_written"
	NormalTerminalStageLeaseCloseBefore        NormalTerminalStage = "lease_close_before"
	NormalTerminalStageTransactionCommitBefore NormalTerminalStage = "transaction_commit_before"
	NormalTerminalStageTransactionCommitAfter  NormalTerminalStage = "transaction_commit_after"
)

type normalTerminalStageHookStore interface {
	NormalTerminalStageHook(context.Context, NormalTerminalStage) error
}

type TerminalMarkerPersistenceStage string

const (
	TerminalMarkerPersistenceValidate     TerminalMarkerPersistenceStage = "validate"
	TerminalMarkerPersistenceBegin        TerminalMarkerPersistenceStage = "begin"
	TerminalMarkerPersistenceLockRun      TerminalMarkerPersistenceStage = "lock_run"
	TerminalMarkerPersistenceLockClaim    TerminalMarkerPersistenceStage = "lock_claim"
	TerminalMarkerPersistenceReadClaim    TerminalMarkerPersistenceStage = "read_claim"
	TerminalMarkerPersistenceReadMarker   TerminalMarkerPersistenceStage = "read_marker"
	TerminalMarkerPersistenceReadLease    TerminalMarkerPersistenceStage = "read_lease"
	TerminalMarkerPersistenceLockAudit    TerminalMarkerPersistenceStage = "lock_audit"
	TerminalMarkerPersistenceReadAudit    TerminalMarkerPersistenceStage = "read_audit"
	TerminalMarkerPersistenceCompareAudit TerminalMarkerPersistenceStage = "compare_audit"
	TerminalMarkerPersistenceWriteAudit   TerminalMarkerPersistenceStage = "write_audit"
	TerminalMarkerPersistenceWriteMarker  TerminalMarkerPersistenceStage = "write_marker"
	TerminalMarkerPersistenceWriteLease   TerminalMarkerPersistenceStage = "write_lease"
	TerminalMarkerPersistenceVerify       TerminalMarkerPersistenceStage = "verify"
	TerminalMarkerPersistenceCommit       TerminalMarkerPersistenceStage = "commit"
)

type TerminalMarkerPersistenceError struct {
	Stage       TerminalMarkerPersistenceStage
	WorkspaceID string
	RunID       string
	AttemptID   uuid.UUID
	Err         error
}

func (err *TerminalMarkerPersistenceError) Error() string {
	if err == nil {
		return "terminal marker persistence failed"
	}
	if err.Err == nil {
		return fmt.Sprintf(
			"terminal marker persistence failed at %s for workspace %q run %q attempt %q",
			err.Stage,
			err.WorkspaceID,
			err.RunID,
			err.AttemptID,
		)
	}
	return fmt.Sprintf(
		"terminal marker persistence failed at %s for workspace %q run %q attempt %q: %v",
		err.Stage,
		err.WorkspaceID,
		err.RunID,
		err.AttemptID,
		err.Err,
	)
}

func (err *TerminalMarkerPersistenceError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Err
}

type normalTerminalCommitPlan struct {
	Claim       ExpectedRunRecordV1
	Marker      TerminalMarkerV1
	LeaseTarget AttemptLeaseState
}

func commitNormalTerminalPGWithOutcome(
	ctx context.Context,
	store expectedRunAdmissionTxStore,
	commit NormalTerminalCommit,
	advanced *bool,
) error {
	if advanced != nil {
		*advanced = false
	}
	plan, err := prepareNormalTerminalCommit(commit)
	if err != nil {
		return err
	}
	mutation, err := prepareTerminalMutation(commit.Candidate)
	if err != nil {
		return normalTerminalPersistenceFailure(
			TerminalMarkerPersistenceValidate,
			commit,
			err,
		)
	}
	fail := func(stage TerminalMarkerPersistenceStage, err error) error {
		return normalTerminalPersistenceFailure(stage, commit, err)
	}
	if store == nil {
		return fail(
			TerminalMarkerPersistenceValidate,
			ErrA4NormalTerminalUnsupported,
		)
	}

	tx, err := store.BeginTx(ctx)
	if err != nil {
		return fail(TerminalMarkerPersistenceBegin, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if commit.BeforeLock != nil {
		if err := commit.BeforeLock(ctx, tx); err != nil {
			return fail(TerminalMarkerPersistenceLockRun, err)
		}
	}

	stateStore := NewPGTerminalStateStore()
	if err := stateStore.LockTerminalRun(
		ctx,
		tx,
		commit.Owner.WorkspaceID,
		commit.Owner.RunID,
	); err != nil {
		return fail(TerminalMarkerPersistenceLockRun, err)
	}
	if err := runNormalTerminalStageHook(
		ctx,
		store,
		NormalTerminalStageRunLocked,
	); err != nil {
		return fail(TerminalMarkerPersistenceLockRun, err)
	}
	claimNamespace := expectedRunNamespace(commit.Owner.WorkspaceID)
	claimKey := expectedRunClaimKey(commit.Owner.RunID)
	if err := store.LockValueTx(
		ctx,
		tx,
		claimNamespace,
		claimKey,
	); err != nil {
		return fail(TerminalMarkerPersistenceLockClaim, err)
	}
	claimBytes, claimPresent, err := store.ReadValueTx(
		ctx,
		tx,
		claimNamespace,
		claimKey,
	)
	if err != nil {
		return fail(TerminalMarkerPersistenceReadClaim, err)
	}
	if !claimPresent {
		return fail(
			TerminalMarkerPersistenceReadClaim,
			fmt.Errorf("expected run claim is missing"),
		)
	}
	claim, err := inspectExpectedRunPhysicalRecord(
		claimBytes,
		commit.Owner.WorkspaceID,
		commit.Owner.RunID,
		claimKey,
		true,
	)
	if err != nil {
		return fail(TerminalMarkerPersistenceReadClaim, err)
	}
	if err := validateNormalTerminalClaim(commit, claim); err != nil {
		return err
	}
	if err := runNormalTerminalStageHook(
		ctx,
		store,
		NormalTerminalStageRegistryRead,
	); err != nil {
		return fail(TerminalMarkerPersistenceReadClaim, err)
	}

	currentMarker, markerPresent, err := stateStore.ReadTerminalMarkerForUpdate(
		ctx,
		tx,
		commit.Owner.WorkspaceID,
		commit.Owner.RunID,
	)
	if err != nil {
		return fail(TerminalMarkerPersistenceReadMarker, err)
	}
	currentLease, leasePresent, err := stateStore.ReadAttemptLeaseForUpdate(
		ctx,
		tx,
		commit.Owner.WorkspaceID,
		commit.Owner.RunID,
	)
	if err != nil {
		return fail(TerminalMarkerPersistenceReadLease, err)
	}
	if !leasePresent {
		return fail(
			TerminalMarkerPersistenceReadLease,
			fmt.Errorf("attempt lease is missing"),
		)
	}
	var markerPtr *TerminalMarkerV1
	if markerPresent {
		markerPtr = &currentMarker
	}
	leaseTarget, err := resolveNormalTerminalLeaseTarget(
		commit,
		currentLease,
		markerPtr,
	)
	if err != nil {
		return err
	}

	if err := store.LockValueTx(
		ctx,
		tx,
		mutation.namespace,
		mutation.key,
	); err != nil {
		return fail(TerminalMarkerPersistenceLockAudit, err)
	}
	if err := runNormalTerminalStageHook(
		ctx,
		store,
		NormalTerminalStageTerminalFactsLocked,
	); err != nil {
		return fail(TerminalMarkerPersistenceLockAudit, err)
	}
	currentAudit, auditPresent, err := store.ReadValueTx(
		ctx,
		tx,
		mutation.namespace,
		mutation.key,
	)
	if err != nil {
		return fail(TerminalMarkerPersistenceReadAudit, err)
	}
	nextAudit, err := mutation.applyPreservingCurrentLineage(
		currentAudit,
		auditPresent,
	)
	if err != nil {
		return fail(TerminalMarkerPersistenceCompareAudit, err)
	}
	auditChanged := !auditPresent || !bytes.Equal(currentAudit, nextAudit)
	if err := runNormalTerminalStageHook(
		ctx,
		store,
		NormalTerminalStageAuditCompared,
	); err != nil {
		return fail(TerminalMarkerPersistenceCompareAudit, err)
	}
	if auditChanged {
		if err := store.PutValueTx(
			ctx,
			tx,
			mutation.namespace,
			mutation.key,
			nextAudit,
		); err != nil {
			return fail(TerminalMarkerPersistenceWriteAudit, err)
		}
	}
	if err := runNormalTerminalStageHook(
		ctx,
		store,
		NormalTerminalStageAuditWritten,
	); err != nil {
		return fail(TerminalMarkerPersistenceWriteAudit, err)
	}

	markerCandidate := plan.Marker
	if markerPresent {
		markerCandidate.CreatedAt = currentMarker.CreatedAt
		markerCandidate.UpdatedAt = currentMarker.UpdatedAt
		if currentMarker.TerminalAt.Equal(
			markerCandidate.TerminalAt.Truncate(time.Microsecond),
		) {
			markerCandidate.TerminalAt = currentMarker.TerminalAt
		}
		if markerLineageBusinessEqual(currentMarker, markerCandidate) {
			markerCandidate.LineageState = currentMarker.LineageState
			markerCandidate.LastErrorCode = currentMarker.LastErrorCode
		}
	}
	writtenMarker, err := stateStore.ApplyTerminalMarkerTransition(
		ctx,
		tx,
		markerCandidate,
	)
	if err != nil {
		return fail(TerminalMarkerPersistenceWriteMarker, err)
	}
	if err := runNormalTerminalStageHook(
		ctx,
		store,
		NormalTerminalStageMarkerWritten,
	); err != nil {
		return fail(TerminalMarkerPersistenceWriteMarker, err)
	}

	writtenLease := currentLease
	if currentLease.State != leaseTarget {
		if err := runNormalTerminalStageHook(
			ctx,
			store,
			NormalTerminalStageLeaseCloseBefore,
		); err != nil {
			return fail(TerminalMarkerPersistenceWriteLease, err)
		}
		var updated bool
		writtenLease, updated, err = stateStore.TransitionNormalAttemptLease(
			ctx,
			tx,
			commit.Owner,
			currentLease.State,
			leaseTarget,
		)
		if err != nil {
			return fail(TerminalMarkerPersistenceWriteLease, err)
		}
		if !updated {
			return fail(
				TerminalMarkerPersistenceWriteLease,
				ErrAttemptLeaseOwnerConflict,
			)
		}
	}

	verifiedAudit, verifiedAuditPresent, err := store.ReadValueTx(
		ctx,
		tx,
		mutation.namespace,
		mutation.key,
	)
	if err != nil {
		return fail(TerminalMarkerPersistenceVerify, err)
	}
	verifiedMarker, verifiedMarkerPresent, err := stateStore.ReadTerminalMarkerForUpdate(
		ctx,
		tx,
		commit.Owner.WorkspaceID,
		commit.Owner.RunID,
	)
	if err != nil {
		return fail(TerminalMarkerPersistenceVerify, err)
	}
	verifiedLease, verifiedLeasePresent, err := stateStore.ReadAttemptLeaseForUpdate(
		ctx,
		tx,
		commit.Owner.WorkspaceID,
		commit.Owner.RunID,
	)
	if err != nil {
		return fail(TerminalMarkerPersistenceVerify, err)
	}
	if !verifiedAuditPresent || !bytes.Equal(verifiedAudit, nextAudit) ||
		!verifiedMarkerPresent || !reflect.DeepEqual(verifiedMarker, writtenMarker) ||
		!verifiedLeasePresent || !reflect.DeepEqual(verifiedLease, writtenLease) {
		return fail(
			TerminalMarkerPersistenceVerify,
			fmt.Errorf("normal terminal durable verification mismatch"),
		)
	}

	if err := runNormalTerminalStageHook(
		ctx,
		store,
		NormalTerminalStageTransactionCommitBefore,
	); err != nil {
		return fail(TerminalMarkerPersistenceCommit, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fail(TerminalMarkerPersistenceCommit, err)
	}
	if advanced != nil {
		markerChanged := !markerPresent ||
			!reflect.DeepEqual(currentMarker, writtenMarker)
		leaseChanged := !reflect.DeepEqual(currentLease, writtenLease)
		*advanced = auditChanged || markerChanged || leaseChanged
	}
	if err := runNormalTerminalStageHook(
		ctx,
		store,
		NormalTerminalStageTransactionCommitAfter,
	); err != nil {
		return fail(TerminalMarkerPersistenceCommit, err)
	}
	return nil
}

func runNormalTerminalStageHook(
	ctx context.Context,
	store expectedRunAdmissionTxStore,
	stage NormalTerminalStage,
) error {
	hooks, ok := store.(normalTerminalStageHookStore)
	if !ok {
		return nil
	}
	if err := hooks.NormalTerminalStageHook(ctx, stage); err != nil {
		return fmt.Errorf("normal terminal stage %s: %w", stage, err)
	}
	return nil
}

func prepareNormalTerminalCommit(
	commit NormalTerminalCommit,
) (normalTerminalCommitPlan, error) {
	fail := func(err error) (normalTerminalCommitPlan, error) {
		return normalTerminalCommitPlan{}, normalTerminalPersistenceFailure(
			TerminalMarkerPersistenceValidate,
			commit,
			err,
		)
	}

	if err := validateAttemptLeaseOwner(commit.Owner); err != nil {
		return fail(fmt.Errorf("validate attempt owner: %w", err))
	}
	if err := ValidateTerminalV3(commit.Candidate); err != nil {
		return fail(fmt.Errorf("validate terminal candidate: %w", err))
	}
	if commit.Candidate.Tenant != commit.Owner.WorkspaceID ||
		commit.Candidate.RunID != commit.Owner.RunID {
		return fail(fmt.Errorf(
			"candidate physical identity does not match attempt owner",
		))
	}
	if commit.Candidate.StartedAt != commit.Owner.RunStartedAt {
		return fail(fmt.Errorf(
			"candidate started_at does not match attempt owner",
		))
	}

	terminalAt, err := parseCanonicalNormalTerminalTime(
		"candidate ended_at",
		commit.Candidate.EndedAt,
	)
	if err != nil {
		return fail(err)
	}
	if commit.Candidate.WorkflowVersion != nil &&
		*commit.Candidate.WorkflowVersion > math.MaxInt32 {
		return fail(fmt.Errorf("candidate workflow_version exceeds int32"))
	}

	phase, status, leaseTarget, err := normalTerminalOutcome(
		commit.Candidate.Status,
	)
	if err != nil {
		return fail(err)
	}
	auditSchemaVersion := int16(3)
	markerWorkflowVersion := normalTerminalInt32Ptr(
		commit.Candidate.WorkflowVersion,
	)
	marker := TerminalMarkerV1{
		WorkspaceID:            commit.Candidate.Tenant,
		RunID:                  commit.Candidate.RunID,
		SchemaVersion:          1,
		AttemptGeneration:      commit.Owner.Generation,
		AttemptID:              commit.Owner.AttemptID,
		Agent:                  commit.Candidate.Agent,
		AttributionScope:       commit.Candidate.AttributionScope,
		TeamID:                 normalTerminalStringPtr(commit.Candidate.TeamID),
		WorkflowID:             normalTerminalStringPtr(commit.Candidate.WorkflowID),
		WorkflowVersion:        markerWorkflowVersion,
		RunSnapshotID:          normalTerminalStringPtr(commit.Candidate.RunSnapshotID),
		ConversationID:         normalTerminalStringPtr(commit.Candidate.ConversationID),
		ParentRunID:            normalTerminalStringPtr(commit.Candidate.ParentRunID),
		ParentSeq:              normalTerminalInt64Ptr(commit.Candidate.ParentSeq),
		AggregationParentRunID: normalTerminalStringPtr(commit.Candidate.AggregationParentRunID),
		TaskGroupID:            normalTerminalStringPtr(commit.Candidate.TaskGroupID),
		RunStartedAt:           commit.Candidate.StartedAt,
		Phase:                  phase,
		Status:                 status,
		StopReason:             commit.Candidate.StopReason,
		Source:                 TerminalMarkerSourceNormal,
		TerminalAt:             terminalAt,
		EvidenceKind:           TerminalMarkerEvidenceRunResult,
		UsageInputTokens:       int64(commit.Candidate.SelfExclusive.InputTokens),
		UsageOutputTokens:      int64(commit.Candidate.SelfExclusive.OutputTokens),
		UsageCostUSD:           commit.Candidate.SelfExclusive.CostUSD,
		UsageToolCalls:         int64(commit.Candidate.SelfExclusive.ToolCalls),
		AuditState:             TerminalMarkerAuditMaterialized,
		AuditSchemaVersion:     &auditSchemaVersion,
		LineageState:           TerminalMarkerLineagePending,
		CreatedAt:              terminalAt,
		UpdatedAt:              terminalAt,
	}
	if err := ValidateTerminalMarkerV1(marker); err != nil {
		return fail(fmt.Errorf("validate derived terminal marker: %w", err))
	}

	return normalTerminalCommitPlan{
		Claim:       expectedRunRecordFromNormalTerminal(commit.Candidate),
		Marker:      marker,
		LeaseTarget: leaseTarget,
	}, nil
}

func validateNormalTerminalClaim(
	commit NormalTerminalCommit,
	claim ExpectedRunRecordV1,
) error {
	plan, err := prepareNormalTerminalCommit(commit)
	if err != nil {
		return err
	}
	if err := validateExpectedRunRecordV1(claim); err != nil {
		return normalTerminalPersistenceFailure(
			TerminalMarkerPersistenceValidate,
			commit,
			fmt.Errorf("validate expected run claim: %w", err),
		)
	}
	if err := compareExpectedRunIdentity(plan.Claim, claim); err != nil {
		return normalTerminalPersistenceFailure(
			TerminalMarkerPersistenceValidate,
			commit,
			fmt.Errorf("terminal candidate does not match expected run claim: %w", err),
		)
	}
	return nil
}

func resolveNormalTerminalLeaseTarget(
	commit NormalTerminalCommit,
	lease RunAttemptLease,
	currentMarker *TerminalMarkerV1,
) (AttemptLeaseState, error) {
	plan, err := prepareNormalTerminalCommit(commit)
	if err != nil {
		return "", err
	}
	fail := func(err error) (AttemptLeaseState, error) {
		return "", normalTerminalPersistenceFailure(
			TerminalMarkerPersistenceValidate,
			commit,
			err,
		)
	}

	if err := ValidateRunAttemptLease(lease); err != nil {
		return fail(fmt.Errorf("validate current attempt lease: %w", err))
	}
	if !attemptLeaseOwnerEqual(lease, commit.Owner) {
		return fail(fmt.Errorf("current attempt lease owner mismatch"))
	}
	if currentMarker != nil {
		if err := ValidateTerminalMarkerV1(*currentMarker); err != nil {
			return fail(fmt.Errorf("validate current terminal marker: %w", err))
		}
	}

	if lease.State == AttemptLeaseReconciled &&
		isExactReconciledNormalUpgrade(currentMarker, plan.Marker) {
		return AttemptLeaseClosed, nil
	}

	if lease.State == AttemptLeaseReconciling &&
		(plan.Marker.Status == TerminalMarkerStatusSuccess ||
			(plan.Marker.Status == TerminalMarkerStatusFailed &&
				plan.Marker.StopReason != "interrupted")) {
		if currentMarker != nil {
			if err := ValidateTerminalMarkerTransition(
				currentMarker,
				plan.Marker,
			); err != nil {
				return fail(fmt.Errorf(
					"reconciling attempt terminal marker transition: %w",
					err,
				))
			}
		}
		return AttemptLeaseClosed, nil
	}

	if lease.State == AttemptLeaseActive {
		if currentMarker == nil {
			return plan.LeaseTarget, nil
		}
		if markerClass(*currentMarker) != "yielded" {
			return fail(fmt.Errorf(
				"active attempt requires no marker or a previous yielded marker",
			))
		}
		if err := ValidateTerminalMarkerTransition(
			currentMarker,
			plan.Marker,
		); err != nil {
			return fail(fmt.Errorf(
				"active attempt terminal marker transition: %w",
				err,
			))
		}
		return plan.LeaseTarget, nil
	}

	if lease.State == plan.LeaseTarget &&
		currentMarker != nil &&
		normalTerminalMarkersEquivalent(*currentMarker, plan.Marker) {
		return plan.LeaseTarget, nil
	}

	return fail(fmt.Errorf(
		"attempt lease state %q is not approved for normal terminal target %q",
		lease.State,
		plan.LeaseTarget,
	))
}

func expectedRunRecordFromNormalTerminal(
	candidate TerminalEntryV3,
) ExpectedRunRecordV1 {
	return ExpectedRunRecordV1{
		SchemaVersion:          expectedRunRecordSchemaV1,
		WorkspaceID:            candidate.Tenant,
		RunID:                  candidate.RunID,
		Agent:                  candidate.Agent,
		AttributionScope:       candidate.AttributionScope,
		TeamID:                 normalTerminalStringPtr(candidate.TeamID),
		WorkflowID:             normalTerminalStringPtr(candidate.WorkflowID),
		WorkflowVersion:        normalTerminalIntPtr(candidate.WorkflowVersion),
		RunSnapshotID:          normalTerminalStringPtr(candidate.RunSnapshotID),
		ParentRunID:            normalTerminalStringPtr(candidate.ParentRunID),
		ParentSeq:              normalTerminalInt64Ptr(candidate.ParentSeq),
		AggregationParentRunID: normalTerminalStringPtr(candidate.AggregationParentRunID),
		TaskGroupID:            normalTerminalStringPtr(candidate.TaskGroupID),
	}
}

func normalTerminalOutcome(
	status string,
) (
	TerminalMarkerPhase,
	TerminalMarkerStatus,
	AttemptLeaseState,
	error,
) {
	switch status {
	case "yielded":
		return TerminalMarkerPhaseYielded,
			TerminalMarkerStatusYielded,
			AttemptLeaseYielded,
			nil
	case "success":
		return TerminalMarkerPhaseFinal,
			TerminalMarkerStatusSuccess,
			AttemptLeaseClosed,
			nil
	case "failed":
		return TerminalMarkerPhaseFinal,
			TerminalMarkerStatusFailed,
			AttemptLeaseClosed,
			nil
	default:
		return "", "", "", fmt.Errorf(
			"unknown normal terminal candidate status %q",
			status,
		)
	}
}

func isExactReconciledNormalUpgrade(
	current *TerminalMarkerV1,
	candidate TerminalMarkerV1,
) bool {
	if current == nil || markerClass(*current) != "reconciled_interrupted" {
		return false
	}
	if current.AttemptGeneration != candidate.AttemptGeneration ||
		current.AttemptID != candidate.AttemptID {
		return false
	}
	if err := ValidateTerminalMarkerTransition(current, candidate); err != nil {
		return false
	}
	return true
}

func normalTerminalMarkersEquivalent(
	left TerminalMarkerV1,
	right TerminalMarkerV1,
) bool {
	if math.Float64bits(left.UsageCostUSD) !=
		math.Float64bits(right.UsageCostUSD) {
		return false
	}
	left.CreatedAt = time.Time{}
	left.UpdatedAt = time.Time{}
	right.CreatedAt = time.Time{}
	right.UpdatedAt = time.Time{}
	right.LineageState = left.LineageState
	right.LastErrorCode = left.LastErrorCode
	left.TerminalAt = left.TerminalAt.UTC().Truncate(time.Microsecond)
	right.TerminalAt = right.TerminalAt.UTC().Truncate(time.Microsecond)
	return reflect.DeepEqual(left, right)
}

func parseCanonicalNormalTerminalTime(field, raw string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil || parsed.Format(time.RFC3339Nano) != raw {
		return time.Time{}, fmt.Errorf("%s must be canonical RFC3339Nano", field)
	}
	return parsed, nil
}

func normalTerminalPersistenceFailure(
	stage TerminalMarkerPersistenceStage,
	commit NormalTerminalCommit,
	err error,
) error {
	return &TerminalMarkerPersistenceError{
		Stage:       stage,
		WorkspaceID: commit.Owner.WorkspaceID,
		RunID:       commit.Owner.RunID,
		AttemptID:   commit.Owner.AttemptID,
		Err:         err,
	}
}

func normalTerminalStringPtr(value *string) *string {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func normalTerminalIntPtr(value *int) *int {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func normalTerminalInt32Ptr(value *int) *int32 {
	if value == nil {
		return nil
	}
	copied := int32(*value)
	return &copied
}

func normalTerminalInt64Ptr(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}
