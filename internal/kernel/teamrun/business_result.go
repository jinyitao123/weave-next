package teamrun

import (
	"encoding/json"
	"sort"
	"strings"
)

// RunBusinessResult is the one server-side answer to "how did this run end for
// the business". The continuation context and the employee run event both take
// it from here, so a client never has to infer it from run status and action
// receipts. It says nothing about Forge's formal approval state.
type RunBusinessResult string

const (
	// The run produced no business-facing result (failed, cancelled, abandoned
	// or still running and without a failing action).
	RunBusinessResultNone RunBusinessResult = ""
	// The team finished and reported the work complete.
	RunBusinessResultCompleted RunBusinessResult = "completed"
	// The team finished and needs the employee to supply more material.
	RunBusinessResultNeedsInput RunBusinessResult = "needs_input"
	// At least one Forge action call returned a failure.
	RunBusinessResultActionFailed RunBusinessResult = "action_failed"
	// At least one Forge action call has an unknown outcome and none failed.
	RunBusinessResultActionUnknown RunBusinessResult = "action_unknown"
)

// BusinessActionCounts is what the platform recorded about a run's Forge action
// calls. Unlike ProjectBusinessActionOutcomes it never fails on a malformed
// record: an employee must still be told that a run ended, so anything that is
// not a recorded success or failure counts as unknown.
type BusinessActionCounts struct {
	Total     int
	Succeeded int
	Failed    int
	Unknown   int
	// Summary describes each call in the order it started, in the words shown to
	// the employee.
	Summary string
}

// NeedsVerification reports whether the employee must check Forge before
// trusting the run's result.
func (counts BusinessActionCounts) NeedsVerification() bool {
	return counts.Failed > 0 || counts.Unknown > 0
}

// ClassifyRunBusinessResult applies one precedence: an action that failed or is
// unknown outranks whatever the team said, because a run that finished can
// still have written nothing.
func ClassifyRunBusinessResult(runStatus, disposition string, counts BusinessActionCounts) RunBusinessResult {
	switch {
	case counts.Failed > 0:
		return RunBusinessResultActionFailed
	case counts.Unknown > 0:
		return RunBusinessResultActionUnknown
	case runStatus == string(StatusSucceeded) && disposition == "needs_input" && counts.Succeeded == 0:
		return RunBusinessResultNeedsInput
	case runStatus == string(StatusSucceeded):
		return RunBusinessResultCompleted
	default:
		return RunBusinessResultNone
	}
}

// CountBusinessActions reads a run's activity events, in any order. A call is
// identified by node, member, invocation and tool call; its latest start
// represents it and its latest result gives the status.
func CountBusinessActions(events []ActivityEvent) BusinessActionCounts {
	type key struct{ node, member, invocation, call, input, operation string }
	type call struct {
		seq   int64
		label string
	}
	type result struct {
		seq    int64
		status string
	}
	type detailFields struct {
		Source          string `json:"source"`
		InputRevisionID string `json:"input_revision_id"`
		OperationID     string `json:"operation_id"`
		InvocationID    string `json:"invocation_id"`
		CallID          string `json:"tool_call_id"`
		ActionLabel     string `json:"action_label"`
		ActionName      string `json:"action_name"`
		Status          string `json:"status"`
	}
	started := map[key]call{}
	results := map[key]result{}
	for _, event := range events {
		if event.Kind != "business_action_started" && event.Kind != "business_action_result" {
			continue
		}
		var detail detailFields
		if json.Unmarshal(event.Detail, &detail) != nil {
			continue
		}
		id := key{event.NodeID, event.MemberID, detail.InvocationID, detail.CallID, detail.InputRevisionID, detail.OperationID}
		if event.Kind == "business_action_started" {
			if detail.Source != "forge_mcp.run_action" {
				continue
			}
			if latest, seen := started[id]; !seen || event.Seq > latest.seq {
				label := detail.ActionLabel
				if label == "" {
					label = detail.ActionName
				}
				if label == "" {
					label = "业务动作"
				}
				if runes := []rune(label); len(runes) > 128 {
					label = string(runes[:128])
				}
				started[id] = call{seq: event.Seq, label: label}
			}
			continue
		}
		if latest, seen := results[id]; !seen || event.Seq > latest.seq {
			results[id] = result{event.Seq, detail.Status}
		}
	}
	ordered := make([]key, 0, len(started))
	for id := range started {
		ordered = append(ordered, id)
	}
	sort.Slice(ordered, func(i, j int) bool { return started[ordered[i]].seq < started[ordered[j]].seq })
	counts := BusinessActionCounts{Total: len(ordered)}
	parts := make([]string, 0, len(ordered))
	for _, id := range ordered {
		text := "结果未知，请先核对业务记录后再处理。"
		switch results[id].status {
		case "succeeded":
			counts.Succeeded++
			text = "调用返回成功。"
		case "failed":
			counts.Failed++
			text = "调用返回失败，请先核对业务记录后再处理。"
		default:
			counts.Unknown++
		}
		parts = append(parts, "业务动作“"+started[id].label+"”"+text)
	}
	counts.Summary = strings.Join(parts, "；")
	return counts
}
