package loomruntime

import (
	"context"
	"errors"
	"fmt"
	"reflect"
)

var ErrA4TerminalLineageRepairUnsupported = errors.New(
	"terminal lineage repair requires PostgreSQL transaction support",
)

const (
	terminalLineageMissingCode           = "terminal_lineage_missing"
	terminalLineageCorruptCode           = "terminal_lineage_corrupt"
	terminalLineageAssociationDefectCode = "terminal_lineage_association_defect"
	terminalLineageDefectCode            = "terminal_lineage_defect"
	terminalLineagePreAttributionCode    = "terminal_lineage_pre_attribution"
	terminalLineageRepairFailedCode      = "terminal_lineage_repair_failed"
)

func terminalLineageFailureCodeStable(code *string) bool {
	if code == nil {
		return false
	}
	switch *code {
	case terminalLineageMissingCode,
		terminalLineageCorruptCode,
		terminalLineageAssociationDefectCode,
		terminalLineageDefectCode,
		terminalLineagePreAttributionCode,
		terminalLineageRepairFailedCode:
		return true
	default:
		return false
	}
}

type TerminalLineageRepairStage string

const (
	TerminalLineageRepairSelectBegin    TerminalLineageRepairStage = "select_begin"
	TerminalLineageRepairSelectRead     TerminalLineageRepairStage = "select_read"
	TerminalLineageRepairSelectCommit   TerminalLineageRepairStage = "select_commit"
	TerminalLineageRepairRebuild        TerminalLineageRepairStage = "rebuild"
	TerminalLineageRepairFinalizeBegin  TerminalLineageRepairStage = "finalize_begin"
	TerminalLineageRepairFinalizeRead   TerminalLineageRepairStage = "finalize_read"
	TerminalLineageRepairFinalizeWrite  TerminalLineageRepairStage = "finalize_write"
	TerminalLineageRepairFinalizeCommit TerminalLineageRepairStage = "finalize_commit"
)

type TerminalLineageRepairError struct {
	Stage       TerminalLineageRepairStage
	WorkspaceID string
	RunID       string
	Code        string
	Retryable   bool
	cause       error
}

func (err *TerminalLineageRepairError) Error() string {
	return fmt.Sprintf(
		"terminal lineage repair: stage=%s workspace_id=%q run_id=%q code=%s retryable=%t",
		err.Stage,
		err.WorkspaceID,
		err.RunID,
		err.Code,
		err.Retryable,
	)
}

func (err *TerminalLineageRepairError) Unwrap() error { return err.cause }

type terminalLineageRepairer struct {
	records TerminalRecordStore
	txStore expectedRunAdmissionTxStore
}

func repairTerminalLineageRun(
	ctx context.Context,
	store TerminalRecordStore,
	workspaceID string,
	runID string,
) (TerminalLineageRebuildReport, bool, error) {
	var report TerminalLineageRebuildReport
	txStore, ok := store.(expectedRunAdmissionTxStore)
	if store == nil || isNilTerminalRecordStore(store) || !ok {
		return report, false, ErrA4TerminalLineageRepairUnsupported
	}
	markerIdentity := TerminalMarkerV1{
		WorkspaceID: workspaceID,
		RunID:       runID,
	}
	tx, err := txStore.BeginTx(ctx)
	if err != nil {
		return report, false, newTerminalLineageRepairError(
			TerminalLineageRepairSelectBegin,
			markerIdentity,
			terminalLineageRepairFailedCode,
			true,
			err,
		)
	}
	finished := false
	defer func() {
		if !finished {
			_ = tx.Rollback(ctx)
		}
	}()

	marker, present, err := NewPGTerminalStateStore().
		ReadTerminalMarkerForUpdate(ctx, tx, workspaceID, runID)
	if err != nil {
		return report, false, newTerminalLineageRepairError(
			TerminalLineageRepairSelectRead,
			markerIdentity,
			terminalLineageRepairFailedCode,
			true,
			err,
		)
	}
	if !present || !terminalMarkerLineageRepairEligible(marker) {
		_ = tx.Rollback(ctx)
		finished = true
		return report, false, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return report, false, newTerminalLineageRepairError(
			TerminalLineageRepairSelectCommit,
			marker,
			terminalLineageRepairFailedCode,
			true,
			err,
		)
	}
	finished = true

	repairer := &terminalLineageRepairer{
		records: store,
		txStore: txStore,
	}
	return repairTerminalLineageSnapshot(ctx, repairer, marker)
}

func terminalMarkerLineageRepairEligible(marker TerminalMarkerV1) bool {
	if marker.AuditState != TerminalMarkerAuditMaterialized {
		return false
	}
	switch marker.LineageState {
	case TerminalMarkerLineagePending:
		return true
	case TerminalMarkerLineageFailed:
		if marker.LastErrorCode == nil {
			return false
		}
		return *marker.LastErrorCode == terminalLineageMissingCode ||
			*marker.LastErrorCode == terminalLineageRepairFailedCode
	default:
		return false
	}
}

func repairTerminalLineageSnapshot(
	ctx context.Context,
	repairer *terminalLineageRepairer,
	snapshot TerminalMarkerV1,
) (TerminalLineageRebuildReport, bool, error) {
	sink := &monotonicTerminalSink{store: repairer.records}
	report, rebuildErr := RebuildTerminalLineage(
		ctx,
		repairer.records,
		snapshot.WorkspaceID,
		snapshot.RunID,
		sink.putCandidate,
	)

	candidate := snapshot
	if rebuildErr == nil {
		candidate.LineageState = TerminalMarkerLineageComplete
		candidate.LastErrorCode = nil
	} else {
		code, _ := terminalLineageRepairCode(rebuildErr)
		candidate.LineageState = TerminalMarkerLineageFailed
		candidate.LastErrorCode = &code
	}

	tx, err := repairer.txStore.BeginTx(ctx)
	if err != nil {
		return report, false, newTerminalLineageRepairError(
			TerminalLineageRepairFinalizeBegin,
			snapshot,
			terminalLineageRepairFailedCode,
			true,
			err,
		)
	}
	finished := false
	defer func() {
		if !finished {
			_ = tx.Rollback(ctx)
		}
	}()

	current, present, err := NewPGTerminalStateStore().
		ReadTerminalMarkerForUpdate(
			ctx,
			tx,
			snapshot.WorkspaceID,
			snapshot.RunID,
		)
	if err != nil {
		return report, false, newTerminalLineageRepairError(
			TerminalLineageRepairFinalizeRead,
			snapshot,
			terminalLineageRepairFailedCode,
			true,
			err,
		)
	}
	if !present || !reflect.DeepEqual(current, snapshot) {
		_ = tx.Rollback(ctx)
		finished = true
		return report, false, nil
	}
	if _, err := NewPGTerminalStateStore().
		ApplyTerminalMarkerTransition(ctx, tx, candidate); err != nil {
		return report, false, newTerminalLineageRepairError(
			TerminalLineageRepairFinalizeWrite,
			snapshot,
			terminalLineageRepairFailedCode,
			true,
			err,
		)
	}
	if err := tx.Commit(ctx); err != nil {
		return report, false, newTerminalLineageRepairError(
			TerminalLineageRepairFinalizeCommit,
			snapshot,
			terminalLineageRepairFailedCode,
			true,
			err,
		)
	}
	finished = true

	if rebuildErr != nil {
		code, retryable := terminalLineageRepairCode(rebuildErr)
		return report, true, newTerminalLineageRepairError(
			TerminalLineageRepairRebuild,
			snapshot,
			code,
			retryable,
			rebuildErr,
		)
	}
	return report, true, nil
}

func terminalLineageRepairCode(err error) (string, bool) {
	var lineageErr *TerminalLineageError
	if !errors.As(err, &lineageErr) {
		return terminalLineageRepairFailedCode, true
	}
	switch lineageErr.Classification {
	case TerminalRecordMissing:
		return terminalLineageMissingCode, true
	case TerminalRecordCorrupt:
		return terminalLineageCorruptCode, false
	case TerminalRecordAssociationDefect:
		return terminalLineageAssociationDefectCode, false
	case TerminalRecordLineageDefect:
		return terminalLineageDefectCode, false
	case TerminalRecordPreAttribution:
		return terminalLineagePreAttributionCode, false
	default:
		return terminalLineageRepairFailedCode, true
	}
}

func newTerminalLineageRepairError(
	stage TerminalLineageRepairStage,
	marker TerminalMarkerV1,
	code string,
	retryable bool,
	cause error,
) *TerminalLineageRepairError {
	return &TerminalLineageRepairError{
		Stage:       stage,
		WorkspaceID: marker.WorkspaceID,
		RunID:       marker.RunID,
		Code:        code,
		Retryable:   retryable,
		cause:       cause,
	}
}
