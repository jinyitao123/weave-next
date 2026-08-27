package loomruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
)

// ErrTerminalConflict identifies a rejected monotonic terminal mutation.
var ErrTerminalConflict = errors.New("terminal monotonic conflict")

// TerminalSink persists complete schema-v3 terminal candidates.
type TerminalSink interface {
	Put(ctx context.Context, candidate TerminalEntryV3) error
}

// TerminalRecordStore provides exact reads and atomic single-value mutation.
type TerminalRecordStore interface {
	ReadValue(
		ctx context.Context,
		namespace string,
		key string,
	) (value []byte, present bool, err error)

	ListKeys(
		ctx context.Context,
		namespace string,
	) ([]string, error)

	MutateValue(
		ctx context.Context,
		namespace string,
		key string,
		mutate func(current []byte, present bool) (next []byte, err error),
	) error
}

type monotonicTerminalSink struct {
	store TerminalRecordStore
}

type lineageTerminalSink struct {
	store     TerminalRecordStore
	monotonic *monotonicTerminalSink
	observers []TerminalLineageObserver
}

type preparedTerminalMutation struct {
	namespace string
	key       string
	candidate TerminalEntryV3
	data      []byte
}

// TerminalLineageDiagnostic reports best-effort work after a successful self write.
type TerminalLineageDiagnostic struct {
	Tenant string
	RunID  string
	Report TerminalLineageRebuildReport
	Err    error
}

// TerminalLineageObserver receives structured best-effort rebuild diagnostics.
type TerminalLineageObserver func(TerminalLineageDiagnostic)

// NewLineageTerminalSink adds self and ancestor rebuilds after the A3c write.
func NewLineageTerminalSink(
	store TerminalRecordStore,
	observers ...TerminalLineageObserver,
) (TerminalSink, error) {
	if store == nil || isNilTerminalRecordStore(store) {
		return nil, fmt.Errorf("terminal lineage record store is required")
	}
	if len(observers) == 0 {
		observers = []TerminalLineageObserver{logTerminalLineageDiagnostic}
	}
	for _, observer := range observers {
		if observer == nil {
			return nil, fmt.Errorf("terminal lineage observer must not be nil")
		}
	}
	return &lineageTerminalSink{
		store:     store,
		monotonic: &monotonicTerminalSink{store: store},
		observers: append([]TerminalLineageObserver(nil), observers...),
	}, nil
}

func isNilTerminalRecordStore(store TerminalRecordStore) bool {
	value := reflect.ValueOf(store)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func (sink *monotonicTerminalSink) Put(
	ctx context.Context,
	candidate TerminalEntryV3,
) error {
	return sink.putCandidate(ctx, candidate)
}

func (sink *monotonicTerminalSink) A4NormalTerminalGuaranteed() bool {
	if sink == nil {
		return false
	}
	_, ok := sink.store.(expectedRunAdmissionTxStore)
	return ok
}

func (sink *monotonicTerminalSink) CommitNormalTerminal(
	ctx context.Context,
	commit NormalTerminalCommit,
) error {
	_, err := sink.commitNormalTerminalWithOutcome(ctx, commit)
	return err
}

func (sink *monotonicTerminalSink) commitNormalTerminalWithOutcome(
	ctx context.Context,
	commit NormalTerminalCommit,
) (bool, error) {
	if sink == nil {
		return false, normalTerminalPersistenceFailure(
			TerminalMarkerPersistenceValidate,
			commit,
			ErrA4NormalTerminalUnsupported,
		)
	}
	store, ok := sink.store.(expectedRunAdmissionTxStore)
	if !ok {
		return false, normalTerminalPersistenceFailure(
			TerminalMarkerPersistenceValidate,
			commit,
			ErrA4NormalTerminalUnsupported,
		)
	}
	var advanced bool
	err := commitNormalTerminalPGWithOutcome(
		ctx,
		store,
		commit,
		&advanced,
	)
	return advanced, err
}

func (sink *monotonicTerminalSink) putCandidate(
	ctx context.Context,
	candidate TerminalEntryV3,
) error {
	mutation, err := prepareTerminalMutation(candidate)
	if err != nil {
		return err
	}
	return sink.store.MutateValue(
		ctx,
		mutation.namespace,
		mutation.key,
		mutation.apply,
	)
}

func prepareTerminalMutation(candidate TerminalEntryV3) (preparedTerminalMutation, error) {
	if err := ValidateTerminalV3(candidate); err != nil {
		return preparedTerminalMutation{}, fmt.Errorf("validate terminal candidate: %w", err)
	}
	candidateBytes, err := json.Marshal(candidate)
	if err != nil {
		return preparedTerminalMutation{}, fmt.Errorf("marshal terminal candidate: %w", err)
	}
	frozenInspection := InspectTerminalRecord(true, candidateBytes)
	if frozenInspection.Classification != TerminalRecordValid ||
		frozenInspection.Entry == nil ||
		frozenInspection.Err != nil {
		return preparedTerminalMutation{}, terminalInspectionError("candidate", frozenInspection)
	}
	frozen := *frozenInspection.Entry
	return preparedTerminalMutation{
		namespace: "audit:" + frozen.Tenant,
		key:       frozen.RunID,
		candidate: frozen,
		data:      bytes.Clone(candidateBytes),
	}, nil
}

func (mutation preparedTerminalMutation) apply(
	currentBytes []byte,
	present bool,
) ([]byte, error) {
	if !present {
		return bytes.Clone(mutation.data), nil
	}
	currentInspection := InspectTerminalRecord(true, currentBytes)
	switch currentInspection.Classification {
	case TerminalRecordPreAttribution:
		return bytes.Clone(mutation.data), nil
	case TerminalRecordValid:
		if currentInspection.Entry == nil {
			return nil, fmt.Errorf("terminal current valid inspection has no entry")
		}
		if err := compareTerminalEntries(
			*currentInspection.Entry,
			mutation.candidate,
			mutation.namespace,
			mutation.key,
		); err != nil {
			return nil, err
		}
		return bytes.Clone(mutation.data), nil
	default:
		return nil, terminalInspectionError("current", currentInspection)
	}
}

func (mutation preparedTerminalMutation) applyPreservingCurrentLineage(
	currentBytes []byte,
	present bool,
) ([]byte, error) {
	if !present {
		return mutation.apply(currentBytes, false)
	}
	currentInspection := InspectTerminalRecord(true, currentBytes)
	if currentInspection.Classification != TerminalRecordValid {
		return mutation.apply(currentBytes, true)
	}
	if currentInspection.Entry == nil {
		return nil, fmt.Errorf("terminal current valid inspection has no entry")
	}

	replacement := mutation.candidate
	replacement.ChildBreakdown = make(
		[]TerminalChildBreakdownV3,
		len(currentInspection.Entry.ChildBreakdown),
	)
	copy(
		replacement.ChildBreakdown,
		currentInspection.Entry.ChildBreakdown,
	)
	total := replacement.SelfExclusive
	var err error
	for _, child := range replacement.ChildBreakdown {
		total, err = addTerminalUsage(total, child.SelfExclusive)
		if err != nil {
			return nil, fmt.Errorf(
				"preserve terminal current lineage: %w",
				err,
			)
		}
	}
	replacement.SubtreeTotal = total
	replacementMutation, err := prepareTerminalMutation(replacement)
	if err != nil {
		return nil, fmt.Errorf("prepare preserved terminal lineage: %w", err)
	}
	return replacementMutation.apply(currentBytes, true)
}

func (sink *lineageTerminalSink) Put(
	ctx context.Context,
	candidate TerminalEntryV3,
) error {
	if err := sink.putSelfCandidate(ctx, candidate); err != nil {
		return err
	}
	report, err := RebuildTerminalLineage(
		ctx,
		sink.store,
		candidate.Tenant,
		candidate.RunID,
		sink.monotonic.putCandidate,
	)
	if err != nil {
		diagnostic := TerminalLineageDiagnostic{
			Tenant: candidate.Tenant,
			RunID:  candidate.RunID,
			Report: report,
			Err:    err,
		}
		for _, observer := range sink.observers {
			observer(diagnostic)
		}
	}
	return nil
}

func (sink *lineageTerminalSink) A4NormalTerminalGuaranteed() bool {
	return sink != nil &&
		sink.monotonic != nil &&
		sink.monotonic.A4NormalTerminalGuaranteed()
}

func (sink *lineageTerminalSink) CommitNormalTerminal(
	ctx context.Context,
	commit NormalTerminalCommit,
) error {
	if sink == nil || sink.monotonic == nil {
		return normalTerminalPersistenceFailure(
			TerminalMarkerPersistenceValidate,
			commit,
			ErrA4NormalTerminalUnsupported,
		)
	}
	_, err := sink.monotonic.commitNormalTerminalWithOutcome(ctx, commit)
	if err != nil {
		return err
	}
	report, _, err := repairTerminalLineageRun(
		ctx,
		sink.store,
		commit.Candidate.Tenant,
		commit.Candidate.RunID,
	)
	if err != nil {
		diagnostic := TerminalLineageDiagnostic{
			Tenant: commit.Candidate.Tenant,
			RunID:  commit.Candidate.RunID,
			Report: report,
			Err:    err,
		}
		for _, observer := range sink.observers {
			observer(diagnostic)
		}
	}
	return nil
}

func (sink *lineageTerminalSink) putSelfCandidate(
	ctx context.Context,
	candidate TerminalEntryV3,
) error {
	err := sink.monotonic.putCandidate(ctx, candidate)
	if err == nil || !isTerminalLineageMergeConflict(err) {
		return err
	}
	for attempt := 0; attempt < terminalLineageMaxCASAttempts; attempt++ {
		merged, mergeErr := scanAndAssembleTerminalLineageReplacement(
			ctx,
			sink.store,
			candidate,
		)
		if mergeErr != nil {
			return fmt.Errorf(
				"merge terminal self write with persisted lineage: %w",
				mergeErr,
			)
		}
		err = sink.monotonic.putCandidate(ctx, merged)
		if err == nil {
			return nil
		}
		if !isTerminalLineageMergeConflict(err) {
			return err
		}
	}
	return fmt.Errorf(
		"publish terminal self with preserved lineage after %d attempts: %w",
		terminalLineageMaxCASAttempts,
		err,
	)
}

func isTerminalLineageMergeConflict(err error) bool {
	if !errors.Is(err, ErrTerminalConflict) {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "descendant_missing") ||
		strings.Contains(message, "subtree_total")
}

func logTerminalLineageDiagnostic(diagnostic TerminalLineageDiagnostic) {
	slog.Warn(
		"terminal lineage rebuild incomplete after self write",
		"tenant", diagnostic.Tenant,
		"run_id", diagnostic.RunID,
		"scans", diagnostic.Report.Scans,
		"cas_conflicts", diagnostic.Report.CASConflicts,
		"rebuilt_run_ids", diagnostic.Report.RebuiltRunIDs,
		"error", diagnostic.Err,
	)
}

func terminalInspectionError(label string, inspection TerminalRecordInspection) error {
	if inspection.Err != nil {
		return fmt.Errorf(
			"terminal %s %s: %w",
			label,
			inspection.Classification,
			inspection.Err,
		)
	}
	return fmt.Errorf(
		"terminal %s classification %s",
		label,
		inspection.Classification,
	)
}

func compareTerminalEntries(
	current TerminalEntryV3,
	candidate TerminalEntryV3,
	namespace string,
	key string,
) error {
	if current.Tenant != candidate.Tenant ||
		"audit:"+current.Tenant != namespace ||
		"audit:"+candidate.Tenant != namespace ||
		current.RunID != candidate.RunID ||
		current.RunID != key ||
		candidate.RunID != key {
		return terminalConflict("identity_conflict", "physical_namespace_or_key")
	}
	if current.SchemaVersion != candidate.SchemaVersion ||
		current.Agent != candidate.Agent ||
		current.AttributionScope != candidate.AttributionScope ||
		!sameTerminalStringPointer(current.TeamID, candidate.TeamID) ||
		!sameTerminalStringPointer(current.WorkflowID, candidate.WorkflowID) ||
		!sameTerminalIntPointer(current.WorkflowVersion, candidate.WorkflowVersion) ||
		!sameTerminalStringPointer(current.RunSnapshotID, candidate.RunSnapshotID) ||
		current.StartedAt != candidate.StartedAt {
		return terminalConflict("identity_conflict", "top_level")
	}
	if !sameTerminalStringPointer(current.ParentRunID, candidate.ParentRunID) ||
		!sameTerminalInt64Pointer(current.ParentSeq, candidate.ParentSeq) ||
		!sameTerminalStringPointer(
			current.AggregationParentRunID,
			candidate.AggregationParentRunID,
		) ||
		!sameTerminalStringPointer(current.TaskGroupID, candidate.TaskGroupID) {
		return terminalConflict("lineage_conflict", "top_level")
	}
	if err := compareTerminalUsage(
		current.SelfExclusive,
		candidate.SelfExclusive,
		"self_exclusive",
	); err != nil {
		return err
	}
	if err := compareTerminalUsage(
		current.SubtreeTotal,
		candidate.SubtreeTotal,
		"subtree_total",
	); err != nil {
		return err
	}

	candidateChildren := make(
		map[string]TerminalChildBreakdownV3,
		len(candidate.ChildBreakdown),
	)
	for _, child := range candidate.ChildBreakdown {
		candidateChildren[child.RunID] = child
	}
	for _, currentChild := range current.ChildBreakdown {
		candidateChild, present := candidateChildren[currentChild.RunID]
		if !present {
			return terminalConflict(
				"descendant_missing",
				currentChild.RunID,
			)
		}
		if !sameTerminalChildIdentity(currentChild, candidateChild) {
			return terminalConflict(
				"lineage_conflict",
				"child_breakdown["+currentChild.RunID+"]",
			)
		}
		if err := compareTerminalUsage(
			currentChild.SelfExclusive,
			candidateChild.SelfExclusive,
			"child_breakdown["+currentChild.RunID+"].self_exclusive",
		); err != nil {
			return err
		}
	}
	return nil
}

func compareTerminalUsage(current, candidate TerminalUsage, path string) error {
	switch {
	case candidate.InputTokens < current.InputTokens:
		return terminalConflict("usage_regression", path+".input_tokens")
	case candidate.OutputTokens < current.OutputTokens:
		return terminalConflict("usage_regression", path+".output_tokens")
	case candidate.ToolCalls < current.ToolCalls:
		return terminalConflict("usage_regression", path+".tool_calls")
	case candidate.CostUSD < current.CostUSD:
		return terminalConflict("usage_regression", path+".cost_usd")
	default:
		return nil
	}
}

func sameTerminalChildIdentity(
	current TerminalChildBreakdownV3,
	candidate TerminalChildBreakdownV3,
) bool {
	return current.RunID == candidate.RunID &&
		current.ParentRunID == candidate.ParentRunID &&
		current.ParentSeq == candidate.ParentSeq &&
		current.Agent == candidate.Agent &&
		sameTerminalStringPointer(current.TeamID, candidate.TeamID) &&
		sameTerminalStringPointer(current.WorkflowID, candidate.WorkflowID) &&
		sameTerminalIntPointer(current.WorkflowVersion, candidate.WorkflowVersion) &&
		sameTerminalStringPointer(current.RunSnapshotID, candidate.RunSnapshotID)
}

func sameTerminalInt64Pointer(left, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func terminalConflict(reason, path string) error {
	return fmt.Errorf("%w: %s: %s", ErrTerminalConflict, reason, path)
}
