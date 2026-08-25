package fanout

import (
	"encoding/json"
	"sort"
	"time"
)

func EvaluateJoin(groupID, completionID, generation string, policy JoinPolicy, legs []GroupLegSnapshot, now time.Time) (JoinEvaluation, error) {
	if groupID == "" || completionID == "" || generation == "" || now.IsZero() || len(legs) == 0 {
		return JoinEvaluation{}, workflowError(ErrorInvalidRequest, "join identity, legs, and now are required")
	}
	if err := validatePolicy(policy, len(legs)); err != nil {
		return JoinEvaluation{}, err
	}
	ordered := append([]GroupLegSnapshot(nil), legs...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].BranchOrdinal < ordered[j].BranchOrdinal })
	if err := validateLegSnapshots(ordered); err != nil {
		return JoinEvaluation{}, err
	}

	succeeded, nonterminal, triggerFailures := 0, 0, 0
	allTerminal := true
	for _, leg := range ordered {
		switch leg.Status {
		case LegDecisionSucceeded:
			succeeded++
		case LegDecisionQueued, LegDecisionRunning, LegDecisionCancelRequested:
			nonterminal++
			allTerminal = false
		case LegDecisionFailed, LegDecisionTimeout, LegDecisionAbandoned:
			triggerFailures++
		}
	}

	evaluation := JoinEvaluation{}
	switch policy.Kind {
	case JoinAllSuccess:
		if allTerminal {
			evaluation.Ready = true
			if succeeded == len(ordered) {
				evaluation.Decision = JoinSucceeded
				evaluation.Diagnostic = "all_succeeded"
			} else {
				evaluation.Decision = JoinFailed
				evaluation.Diagnostic = "all_terminal_unsatisfied"
			}
		}
	case JoinQuorum:
		if succeeded >= policy.Quorum {
			evaluation.Ready, evaluation.Decision, evaluation.Diagnostic = true, JoinSucceeded, "quorum_reached"
		} else if succeeded+nonterminal < policy.Quorum {
			evaluation.Ready, evaluation.Decision, evaluation.Diagnostic = true, JoinFailed, "quorum_unreachable"
		}
	case JoinDeadline:
		if allTerminal || !now.Before(policy.DeadlineAt) {
			evaluation.Ready, evaluation.Decision = true, JoinSucceeded
			if now.Before(policy.DeadlineAt) {
				evaluation.Diagnostic = "all_terminal"
			} else {
				evaluation.Diagnostic = "deadline_reached"
			}
		}
	case JoinFailFast:
		if triggerFailures > 0 {
			evaluation.Ready, evaluation.Decision, evaluation.Diagnostic = true, JoinFailed, "failed_fast"
		} else if succeeded == len(ordered) {
			evaluation.Ready, evaluation.Decision, evaluation.Diagnostic = true, JoinSucceeded, "all_succeeded"
		} else if allTerminal {
			evaluation.Ready, evaluation.Decision, evaluation.Diagnostic = true, JoinFailed, "all_terminal_unsatisfied"
		}
	}
	if !evaluation.Ready && !now.Before(policy.DeadlineAt) {
		evaluation.Ready, evaluation.Decision, evaluation.Diagnostic = true, JoinSucceeded, "deadline_reached"
	}
	if !evaluation.Ready {
		return evaluation, nil
	}

	evaluation.Frozen = freezeJoinResult(groupID, completionID, generation, policy, ordered, now, evaluation.Decision, evaluation.Diagnostic)
	if policy.Kind != JoinAllSuccess {
		for _, leg := range ordered {
			switch leg.Status {
			case LegDecisionQueued:
				evaluation.Actions.CutLegIDs = append(evaluation.Actions.CutLegIDs, leg.LegID)
			case LegDecisionRunning:
				evaluation.Actions.RequestCancelLegIDs = append(evaluation.Actions.RequestCancelLegIDs, leg.LegID)
			}
		}
	}
	return evaluation, nil
}

func ProjectJoinResult(result JoinResultV1) (JoinProjectionV1, error) {
	if result.SchemaVersion != 1 || result.GroupID == "" || result.GroupCompletionID == "" ||
		result.Generation == "" || (result.Decision != JoinSucceeded && result.Decision != JoinFailed) {
		return JoinProjectionV1{}, workflowError(ErrorInvalidRequest, "join result identity is invalid")
	}
	projection := JoinProjectionV1{
		SchemaVersion: 1,
		Decision:      result.Decision,
		Results:       make(map[string]json.RawMessage),
		Errors:        make(map[string]string),
	}
	for _, leg := range result.Legs {
		if leg.BranchID == "" {
			return JoinProjectionV1{}, workflowError(ErrorInvalidRequest, "join result branch identity is invalid")
		}
		if _, exists := projection.Results[leg.BranchID]; exists {
			return JoinProjectionV1{}, workflowError(ErrorInvalidRequest, "duplicate join result branch %q", leg.BranchID)
		}
		if len(leg.Result) != 0 {
			projection.Results[leg.BranchID] = append(json.RawMessage(nil), leg.Result...)
		} else {
			projection.Results[leg.BranchID] = json.RawMessage(`null`)
		}
		if leg.ErrorCode != nil {
			projection.Errors[leg.BranchID] = *leg.ErrorCode
		}
	}
	return projection, nil
}

func validateLegSnapshots(legs []GroupLegSnapshot) error {
	seenLegs := make(map[string]struct{}, len(legs))
	seenBranches := make(map[string]struct{}, len(legs))
	for index, leg := range legs {
		if leg.LegID == "" || leg.BranchID == "" || leg.BranchOrdinal != index {
			return workflowError(ErrorInvalidRequest, "join legs require continuous branch order")
		}
		if _, exists := seenLegs[leg.LegID]; exists {
			return workflowError(ErrorInvalidRequest, "duplicate leg %q", leg.LegID)
		}
		if _, exists := seenBranches[leg.BranchID]; exists {
			return workflowError(ErrorInvalidRequest, "duplicate branch %q", leg.BranchID)
		}
		seenLegs[leg.LegID], seenBranches[leg.BranchID] = struct{}{}, struct{}{}
		if !validDecisionState(leg.Status) {
			return workflowError(ErrorUnknownLegStatus, "unknown workflow leg status %q", leg.Status)
		}
		if isTerminalDecisionState(leg.Status) != (leg.CompletedAt != nil) {
			return workflowError(ErrorInvalidRequest, "terminal facts do not match state for leg %q", leg.LegID)
		}
		if !isTerminalDecisionState(leg.Status) && (len(leg.Result) != 0 || leg.ErrorCode != "") {
			return workflowError(ErrorInvalidRequest, "nonterminal leg %q carries terminal facts", leg.LegID)
		}
	}
	return nil
}

func freezeJoinResult(groupID, completionID, generation string, policy JoinPolicy, legs []GroupLegSnapshot, now time.Time, decision JoinDecision, diagnostic string) JoinResultV1 {
	frozenLegs := make([]JoinResultLegV1, 0, len(legs))
	for _, leg := range legs {
		item := JoinResultLegV1{LegID: leg.LegID, BranchID: leg.BranchID, DecisionState: leg.Status}
		if terminal, ok := terminalForDecisionState(leg.Status); ok {
			item.Terminal = &terminal
			item.Result = append(json.RawMessage(nil), leg.Result...)
			if leg.ErrorCode != "" {
				errorCode := leg.ErrorCode
				item.ErrorCode = &errorCode
			}
			if leg.CompletedAt != nil {
				terminalAt := leg.CompletedAt.UTC()
				item.TerminalAt = &terminalAt
			}
		}
		frozenLegs = append(frozenLegs, item)
	}
	return JoinResultV1{
		SchemaVersion: 1, GroupID: groupID, GroupCompletionID: completionID,
		Generation: generation, Decision: decision, Diagnostic: diagnostic,
		DecidedAt: now.UTC(), Policy: normalizePolicy(policy), Legs: frozenLegs,
	}
}

func validDecisionState(status LegDecisionState) bool {
	switch status {
	case LegDecisionQueued, LegDecisionRunning, LegDecisionCancelRequested,
		LegDecisionSucceeded, LegDecisionFailed, LegDecisionTimeout,
		LegDecisionCut, LegDecisionCancelled, LegDecisionAbandoned:
		return true
	default:
		return false
	}
}

func isTerminalDecisionState(status LegDecisionState) bool {
	_, ok := terminalForDecisionState(status)
	return ok
}

func terminalForDecisionState(status LegDecisionState) (LegTerminal, bool) {
	switch status {
	case LegDecisionSucceeded:
		return LegSucceeded, true
	case LegDecisionFailed:
		return LegFailed, true
	case LegDecisionTimeout:
		return LegTimeout, true
	case LegDecisionCut:
		return LegCut, true
	case LegDecisionCancelled:
		return LegCancelled, true
	case LegDecisionAbandoned:
		return LegAbandoned, true
	default:
		return "", false
	}
}
