package loomruntime

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

const terminalLineageMaxCASAttempts = 3

// TerminalLineagePublishFunc publishes one complete candidate through A3c CAS.
type TerminalLineagePublishFunc func(context.Context, TerminalEntryV3) error

// TerminalLineageRebuildReport records observable rebuild and retry facts.
type TerminalLineageRebuildReport struct {
	Scans         int
	CASConflicts  int
	RebuiltRunIDs []string
}

// RebuildTerminalLineage rebuilds one run and each existing canonical ancestor.
func RebuildTerminalLineage(
	ctx context.Context,
	store TerminalRecordStore,
	tenant string,
	targetRunID string,
	publish TerminalLineagePublishFunc,
) (TerminalLineageRebuildReport, error) {
	var report TerminalLineageRebuildReport
	if store == nil || isNilTerminalRecordStore(store) {
		return report, fmt.Errorf("terminal lineage record store is required")
	}
	if tenant == "" {
		return report, fmt.Errorf("terminal lineage tenant is required")
	}
	if targetRunID == "" {
		return report, fmt.Errorf("terminal lineage target run_id is required")
	}
	if publish == nil {
		return report, fmt.Errorf("terminal lineage publisher is required")
	}

	currentRunID := targetRunID
	ancestors := make(map[string]struct{})
	for {
		if _, repeated := ancestors[currentRunID]; repeated {
			return report, terminalLineageError(
				TerminalRecordLineageDefect,
				"canonical cycle reaches ancestor %q",
				currentRunID,
			)
		}
		ancestors[currentRunID] = struct{}{}

		var candidate TerminalEntryV3
		published := false
		for attempt := 0; attempt < terminalLineageMaxCASAttempts; attempt++ {
			report.Scans++
			assembled, err := scanAndAssembleTerminalLineage(
				ctx,
				store,
				tenant,
				currentRunID,
			)
			if err != nil {
				return report, err
			}
			candidate = assembled
			if err := publish(ctx, candidate); err != nil {
				if errors.Is(err, ErrTerminalConflict) {
					report.CASConflicts++
					continue
				}
				return report, fmt.Errorf(
					"publish rebuilt terminal lineage for %q: %w",
					currentRunID,
					err,
				)
			}
			published = true
			report.RebuiltRunIDs = append(report.RebuiltRunIDs, currentRunID)
			break
		}
		if !published {
			return report, fmt.Errorf(
				"publish rebuilt terminal lineage for %q after %d attempts: %w",
				currentRunID,
				terminalLineageMaxCASAttempts,
				ErrTerminalConflict,
			)
		}
		if candidate.ParentRunID == nil {
			return report, nil
		}
		currentRunID = *candidate.ParentRunID
	}
}

func scanAndAssembleTerminalLineage(
	ctx context.Context,
	store TerminalRecordStore,
	tenant string,
	targetRunID string,
) (TerminalEntryV3, error) {
	records, invalidByKey, err := scanTerminalLineageRecords(
		ctx,
		store,
		tenant,
		targetRunID,
	)
	if err != nil {
		return TerminalEntryV3{}, err
	}
	candidate, err := AssembleTerminalLineage(TerminalLineageAssemblyInput{
		Tenant:      tenant,
		TargetRunID: targetRunID,
		Records:     records,
	})
	return restoreTerminalLineageInspection(candidate, err, invalidByKey)
}

func scanAndAssembleTerminalLineageReplacement(
	ctx context.Context,
	store TerminalRecordStore,
	candidate TerminalEntryV3,
) (TerminalEntryV3, error) {
	records, invalidByKey, err := scanTerminalLineageRecords(
		ctx,
		store,
		candidate.Tenant,
		candidate.RunID,
	)
	if err != nil {
		return TerminalEntryV3{}, err
	}
	replaced := false
	for index := range records {
		if records[index].Entry.RunID != candidate.RunID {
			continue
		}
		current := records[index].Entry
		replacement := candidate
		replacement.ChildBreakdown = append(
			[]TerminalChildBreakdownV3(nil),
			current.ChildBreakdown...,
		)
		total := replacement.SelfExclusive
		for _, child := range replacement.ChildBreakdown {
			total, err = addTerminalUsage(total, child.SelfExclusive)
			if err != nil {
				return TerminalEntryV3{}, fmt.Errorf(
					"merge terminal lineage replacement %q: %w",
					candidate.RunID,
					err,
				)
			}
		}
		replacement.SubtreeTotal = total
		records[index].Entry = replacement
		replaced = true
		break
	}
	if !replaced {
		return TerminalEntryV3{}, terminalLineageRunError(
			TerminalRecordMissing,
			candidate.RunID,
			"target run %q is missing",
			candidate.RunID,
		)
	}
	assembled, err := AssembleTerminalLineage(TerminalLineageAssemblyInput{
		Tenant:      candidate.Tenant,
		TargetRunID: candidate.RunID,
		Records:     records,
	})
	return restoreTerminalLineageInspection(assembled, err, invalidByKey)
}

func scanTerminalLineageRecords(
	ctx context.Context,
	store TerminalRecordStore,
	tenant string,
	targetRunID string,
) ([]TerminalLineageRecord, map[string]TerminalRecordInspection, error) {
	namespace := "audit:" + tenant
	keys, err := store.ListKeys(ctx, namespace)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"list terminal lineage namespace %q: %w",
			namespace,
			err,
		)
	}
	sort.Strings(keys)

	records := make([]TerminalLineageRecord, 0, len(keys))
	invalidByKey := make(map[string]TerminalRecordInspection)
	for _, key := range keys {
		value, present, err := store.ReadValue(ctx, namespace, key)
		if err != nil {
			return nil, nil, fmt.Errorf(
				"read terminal lineage %q/%q: %w",
				namespace,
				key,
				err,
			)
		}
		if !present {
			if key == targetRunID {
				return nil, nil, terminalLineageRunError(
					TerminalRecordMissing,
					targetRunID,
					"target terminal %q disappeared during scan",
					targetRunID,
				)
			}
			continue
		}
		inspection := InspectTerminalRecord(true, value)
		if inspection.Classification != TerminalRecordValid || inspection.Entry == nil {
			invalidByKey[key] = inspection
			if key == targetRunID {
				return nil, nil, &TerminalLineageError{
					Classification: inspection.Classification,
					Evidence:       []string{key},
					message: fmt.Sprintf(
						"scan terminal lineage %s: target %q classification %s: %v",
						inspection.Classification,
						key,
						inspection.Classification,
						inspection.Err,
					),
				}
			}
			continue
		}
		records = append(records, TerminalLineageRecord{
			Key:   key,
			Entry: *inspection.Entry,
		})
	}
	return records, invalidByKey, nil
}

func restoreTerminalLineageInspection(
	candidate TerminalEntryV3,
	err error,
	invalidByKey map[string]TerminalRecordInspection,
) (TerminalEntryV3, error) {
	if err == nil {
		return candidate, nil
	}
	var lineageErr *TerminalLineageError
	if errors.As(err, &lineageErr) &&
		lineageErr.Classification == TerminalRecordMissing {
		for _, runID := range lineageErr.RunIDs {
			if inspection, exists := invalidByKey[runID]; exists {
				return TerminalEntryV3{}, &TerminalLineageError{
					Classification: inspection.Classification,
					Evidence:       []string{runID},
					RunIDs:         []string{runID},
					message: fmt.Sprintf(
						"scan terminal lineage %s: related run %q classification %s: %v",
						inspection.Classification,
						runID,
						inspection.Classification,
						inspection.Err,
					),
				}
			}
		}
	}
	return TerminalEntryV3{}, err
}
