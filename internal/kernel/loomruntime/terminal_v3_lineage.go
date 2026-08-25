package loomruntime

import (
	"errors"
	"fmt"
	"sort"
)

// TerminalLineageRecord pairs a physical audit key with its validated entry.
type TerminalLineageRecord struct {
	Key   string
	Entry TerminalEntryV3
}

// TerminalLineageAssemblyInput contains the records needed to rebuild one run.
type TerminalLineageAssemblyInput struct {
	Tenant      string
	TargetRunID string
	Records     []TerminalLineageRecord
	Expected    []TerminalExpectedAggregationEdge
}

// TerminalExpectedAggregationEdge is caller-validated durable child evidence.
type TerminalExpectedAggregationEdge struct {
	ChildRunID             string
	ParentRunID            string
	ParentSeq              int64
	AggregationParentRunID string
}

// TerminalLineageError reports a stable terminal classification with edge evidence.
type TerminalLineageError struct {
	Classification TerminalRecordClassification
	Evidence       []string
	RunIDs         []string
	message        string
}

func (err *TerminalLineageError) Error() string {
	return err.message
}

// AssembleTerminalLineage rebuilds one terminal entry from descendant exclusive usage.
func AssembleTerminalLineage(input TerminalLineageAssemblyInput) (TerminalEntryV3, error) {
	if input.Tenant == "" {
		return TerminalEntryV3{}, fmt.Errorf("assemble terminal lineage: tenant is required")
	}
	if input.TargetRunID == "" {
		return TerminalEntryV3{}, fmt.Errorf("assemble terminal lineage: target run_id is required")
	}

	entries := make(map[string]TerminalEntryV3, len(input.Records))
	duplicateEvidence := make(map[string][]string)
	for _, record := range input.Records {
		if err := ValidateTerminalV3(record.Entry); err != nil {
			return TerminalEntryV3{}, terminalLineageError(
				terminalClassificationFromError(err),
				"record %q is invalid: %v",
				record.Key,
				err,
			)
		}
		if record.Key != record.Entry.RunID {
			duplicateEvidence[record.Entry.RunID] = append(
				duplicateEvidence[record.Entry.RunID],
				fmt.Sprintf("key %q", record.Key),
			)
		}
		if previous, exists := entries[record.Entry.RunID]; exists {
			duplicateEvidence[record.Entry.RunID] = append(
				duplicateEvidence[record.Entry.RunID],
				fmt.Sprintf("duplicate record for agent %q after agent %q", record.Entry.Agent, previous.Agent),
			)
			continue
		}
		entries[record.Entry.RunID] = record.Entry
	}

	root, present := entries[input.TargetRunID]
	if !present {
		return TerminalEntryV3{}, terminalLineageRunError(
			TerminalRecordMissing,
			input.TargetRunID,
			"target run %q is missing",
			input.TargetRunID,
		)
	}
	if evidence := duplicateEvidence[root.RunID]; len(evidence) != 0 {
		return TerminalEntryV3{}, terminalLineageError(
			TerminalRecordLineageDefect,
			"duplicate run_id %q: %v",
			root.RunID,
			evidence,
		)
	}

	expectedByChild := make(map[string]TerminalExpectedAggregationEdge, len(input.Expected))
	conflictingExpected := make(map[string]bool)
	for _, edge := range input.Expected {
		if edge.ChildRunID == "" || edge.ParentRunID == "" ||
			edge.AggregationParentRunID == "" || edge.ParentSeq < 0 {
			return TerminalEntryV3{}, terminalLineageError(
				TerminalRecordLineageDefect,
				"expected aggregation edge is incomplete: %#v",
				edge,
			)
		}
		if edge.AggregationParentRunID != edge.ParentRunID {
			return TerminalEntryV3{}, terminalLineageError(
				TerminalRecordLineageDefect,
				"expected aggregation parent %q does not equal canonical parent %q for run %q",
				edge.AggregationParentRunID,
				edge.ParentRunID,
				edge.ChildRunID,
			)
		}
		if previous, exists := expectedByChild[edge.ChildRunID]; exists {
			if previous != edge {
				conflictingExpected[edge.ChildRunID] = true
			}
			continue
		}
		expectedByChild[edge.ChildRunID] = edge
	}

	provenanceChildren := make(map[string]map[string]struct{})
	aggregationChildren := make(map[string]map[string]struct{})
	addChild := func(adjacency map[string]map[string]struct{}, parentRunID, childRunID string) {
		if adjacency[parentRunID] == nil {
			adjacency[parentRunID] = make(map[string]struct{})
		}
		adjacency[parentRunID][childRunID] = struct{}{}
	}
	for runID, entry := range entries {
		if entry.ParentRunID != nil {
			addChild(provenanceChildren, *entry.ParentRunID, runID)
		}
		if entry.AggregationParentRunID != nil {
			addChild(aggregationChildren, *entry.AggregationParentRunID, runID)
		}
	}
	for _, edge := range input.Expected {
		addChild(aggregationChildren, edge.AggregationParentRunID, edge.ChildRunID)
	}

	aggregationOrder, err := validateTerminalLineageGraph(
		input,
		entries,
		duplicateEvidence,
		expectedByChild,
		conflictingExpected,
		aggregationChildren,
		true,
	)
	if err != nil {
		return TerminalEntryV3{}, err
	}
	if _, err := validateTerminalLineageGraph(
		input,
		entries,
		duplicateEvidence,
		nil,
		nil,
		provenanceChildren,
		false,
	); err != nil {
		return TerminalEntryV3{}, err
	}

	descendants := make([]TerminalChildBreakdownV3, 0, len(aggregationOrder))
	for _, childRunID := range aggregationOrder {
		child := entries[childRunID]
		descendants = append(descendants, TerminalChildBreakdownV3{
			RunID:           child.RunID,
			ParentRunID:     *child.ParentRunID,
			ParentSeq:       *child.ParentSeq,
			Agent:           child.Agent,
			TeamID:          cloneTerminalString(child.TeamID),
			WorkflowID:      cloneTerminalString(child.WorkflowID),
			WorkflowVersion: cloneTerminalInt(child.WorkflowVersion),
			RunSnapshotID:   cloneTerminalString(child.RunSnapshotID),
			SelfExclusive:   child.SelfExclusive,
		})
	}

	sort.Slice(descendants, func(left, right int) bool {
		if descendants[left].ParentRunID != descendants[right].ParentRunID {
			return descendants[left].ParentRunID < descendants[right].ParentRunID
		}
		if descendants[left].ParentSeq != descendants[right].ParentSeq {
			return descendants[left].ParentSeq < descendants[right].ParentSeq
		}
		return descendants[left].RunID < descendants[right].RunID
	})

	total := root.SelfExclusive
	for _, child := range descendants {
		next, err := addTerminalUsage(total, child.SelfExclusive)
		if err != nil {
			return TerminalEntryV3{}, fmt.Errorf(
				"assemble terminal lineage child %q: %w",
				child.RunID,
				err,
			)
		}
		total = next
	}
	root.ChildBreakdown = descendants
	root.SubtreeTotal = total
	if err := ValidateTerminalV3(root); err != nil {
		return TerminalEntryV3{}, fmt.Errorf("assemble terminal lineage candidate: %w", err)
	}
	return root, nil
}

func validateOldTerminalLineageEvidence(
	root TerminalEntryV3,
	entries map[string]TerminalEntryV3,
) error {
	for _, previous := range root.ChildBreakdown {
		entry, present := entries[previous.RunID]
		if !present {
			return terminalLineageRunError(
				TerminalRecordMissing,
				previous.RunID,
				"old descendant terminal %q is missing",
				previous.RunID,
			)
		}
		if entry.ParentRunID == nil || entry.ParentSeq == nil ||
			entry.AggregationParentRunID == nil ||
			*entry.ParentRunID != previous.ParentRunID ||
			*entry.ParentSeq != previous.ParentSeq ||
			*entry.AggregationParentRunID != previous.ParentRunID {
			return terminalLineageError(
				TerminalRecordLineageDefect,
				"multiple parents for old descendant %q",
				previous.RunID,
			)
		}
	}
	return nil
}

func validateTerminalLineageGraph(
	input TerminalLineageAssemblyInput,
	entries map[string]TerminalEntryV3,
	duplicateEvidence map[string][]string,
	expectedByChild map[string]TerminalExpectedAggregationEdge,
	conflictingExpected map[string]bool,
	adjacency map[string]map[string]struct{},
	aggregation bool,
) ([]string, error) {
	const (
		unvisited = iota
		visiting
		visited
	)
	state := make(map[string]int)
	order := make([]string, 0)
	var visit func(string) error
	visit = func(runID string) error {
		switch state[runID] {
		case visiting:
			graphName := "canonical"
			if aggregation {
				graphName = "aggregation"
			}
			return terminalLineageError(
				TerminalRecordLineageDefect,
				"%s cycle reaches run %q",
				graphName,
				runID,
			)
		case visited:
			return nil
		}
		entry, present := entries[runID]
		if !present {
			return terminalLineageRunError(
				TerminalRecordMissing,
				runID,
				"terminal %q is missing",
				runID,
			)
		}
		if evidence := duplicateEvidence[runID]; len(evidence) != 0 {
			return terminalLineageError(
				TerminalRecordLineageDefect,
				"duplicate run_id %q: %v",
				runID,
				evidence,
			)
		}
		if entry.Tenant != input.Tenant {
			return terminalLineageError(
				TerminalRecordLineageDefect,
				"cross tenant run %q has tenant %q, want %q",
				runID,
				entry.Tenant,
				input.Tenant,
			)
		}
		if aggregation {
			if err := validateOldTerminalLineageEvidence(entry, entries); err != nil {
				return err
			}
		}
		if conflictingExpected[runID] {
			return terminalLineageError(
				TerminalRecordLineageDefect,
				"multiple parents in expected evidence for run %q",
				runID,
			)
		}
		if expected, exists := expectedByChild[runID]; exists {
			if entry.ParentRunID == nil || entry.ParentSeq == nil ||
				entry.AggregationParentRunID == nil ||
				*entry.ParentRunID != expected.ParentRunID ||
				*entry.ParentSeq != expected.ParentSeq ||
				*entry.AggregationParentRunID != expected.AggregationParentRunID {
				return terminalLineageError(
					TerminalRecordLineageDefect,
					"multiple parents between persisted and expected evidence for run %q",
					runID,
				)
			}
		}

		state[runID] = visiting
		children := sortedTerminalLineageChildren(adjacency[runID])
		for _, childRunID := range children {
			if conflictingExpected[childRunID] {
				return terminalLineageError(
					TerminalRecordLineageDefect,
					"multiple parents in expected evidence for run %q",
					childRunID,
				)
			}
			child, present := entries[childRunID]
			if !present {
				return terminalLineageRunError(
					TerminalRecordMissing,
					childRunID,
					"terminal %q is missing",
					childRunID,
				)
			}
			if aggregation && !sameTerminalAttributionDomain(entry, child) {
				return terminalLineageError(
					TerminalRecordLineageDefect,
					"cross attribution domain aggregation edge %q -> %q",
					runID,
					childRunID,
				)
			}
			if err := visit(childRunID); err != nil {
				return err
			}
			if aggregation {
				order = append(order, childRunID)
			}
		}
		state[runID] = visited
		return nil
	}
	if err := visit(input.TargetRunID); err != nil {
		return nil, err
	}
	return order, nil
}

func sortedTerminalLineageChildren(children map[string]struct{}) []string {
	sorted := make([]string, 0, len(children))
	for runID := range children {
		sorted = append(sorted, runID)
	}
	sort.Strings(sorted)
	return sorted
}

func sameTerminalAttributionDomain(left, right TerminalEntryV3) bool {
	return left.AttributionScope == right.AttributionScope &&
		sameTerminalStringPointer(left.TeamID, right.TeamID) &&
		sameTerminalStringPointer(left.WorkflowID, right.WorkflowID) &&
		sameTerminalIntPointer(left.WorkflowVersion, right.WorkflowVersion) &&
		sameTerminalStringPointer(left.RunSnapshotID, right.RunSnapshotID)
}

func terminalClassificationFromError(err error) TerminalRecordClassification {
	var validationErr *terminalRecordValidationError
	if errors.As(err, &validationErr) {
		return validationErr.class
	}
	return TerminalRecordCorrupt
}

func terminalLineageError(
	classification TerminalRecordClassification,
	format string,
	args ...any,
) *TerminalLineageError {
	message := fmt.Sprintf(format, args...)
	return &TerminalLineageError{
		Classification: classification,
		Evidence:       []string{message},
		message:        "assemble terminal lineage " + string(classification) + ": " + message,
	}
}

func terminalLineageRunError(
	classification TerminalRecordClassification,
	runID string,
	format string,
	args ...any,
) *TerminalLineageError {
	err := terminalLineageError(classification, format, args...)
	err.RunIDs = []string{runID}
	return err
}

func cloneTerminalString(value *string) *string {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneTerminalInt(value *int) *int {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}
