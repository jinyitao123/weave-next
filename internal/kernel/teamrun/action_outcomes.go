package teamrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const MaxBusinessActionOutcomesPerRun = 100

var ErrBusinessActionOutcomeLimitExceeded = errors.New("team run business action outcome limit exceeded")

type BusinessActionOutcomeV1 struct {
	NodeID     string `json:"node_id"`
	CallID     string `json:"call_id"`
	ActionName string `json:"action_name"`
	ObjectName string `json:"object_name"`
	RecordID   string `json:"record_id,omitempty"`
	Status     string `json:"status"`
	Summary    string `json:"summary"`
}

type businessActionActivityDetailV1 struct {
	Source             string `json:"source"`
	Phase              string `json:"phase"`
	InvocationID       string `json:"invocation_id"`
	CallID             string `json:"tool_call_id"`
	CapabilityID       string `json:"capability_id"`
	ActionKey          string `json:"action_key"`
	ActionLabel        string `json:"action_label,omitempty"`
	ActionName         string `json:"action_name"`
	ObjectName         string `json:"object_name"`
	InputRevisionID    string `json:"input_revision_id"`
	RecordID           string `json:"record_id,omitempty"`
	FrozenRecordSHA256 string `json:"frozen_record_sha256,omitempty"`
	Status             string `json:"status,omitempty"`
}

// ProjectBusinessActionOutcomes converts only platform activity receipts into
// the Workbench action facts. Member output and generic tool events are ignored.
func ProjectBusinessActionOutcomes(events []ActivityEvent) ([]BusinessActionOutcomeV1, error) {
	type key struct{ workspaceID, runID, nodeID, memberID, invocationID, callID string }
	type receipt struct {
		started ActivityEvent
		detail  businessActionActivityDetailV1
		status  string
	}

	receipts := make(map[key]receipt)
	order := make([]key, 0)
	workspaceID, runID := "", ""
	for _, event := range events {
		if event.Kind != "business_action_started" && event.Kind != "business_action_result" {
			continue
		}
		var detail businessActionActivityDetailV1
		if err := json.Unmarshal(event.Detail, &detail); err != nil {
			return nil, fmt.Errorf("decode business action activity: %w", err)
		}
		if event.WorkspaceID == "" || event.RunID == "" || event.NodeID == "" || event.MemberID == "" ||
			detail.Source != "forge_mcp.run_action" ||
			strings.TrimSpace(detail.InvocationID) == "" || strings.TrimSpace(detail.CallID) == "" ||
			strings.TrimSpace(detail.CapabilityID) == "" || strings.TrimSpace(detail.ActionKey) == "" ||
			strings.TrimSpace(detail.ActionName) == "" || strings.TrimSpace(detail.ObjectName) == "" ||
			strings.TrimSpace(detail.InputRevisionID) == "" ||
			(detail.Phase != "started" && detail.Phase != "result") {
			return nil, errors.New("business action activity provenance is incomplete")
		}
		if workspaceID == "" {
			workspaceID, runID = event.WorkspaceID, event.RunID
		} else if event.WorkspaceID != workspaceID || event.RunID != runID {
			return nil, errors.New("business action projection crossed workspace or run identity")
		}
		if (event.Kind == "business_action_started" && detail.Phase != "started") ||
			(event.Kind == "business_action_result" && detail.Phase != "result") {
			return nil, errors.New("business action activity phase does not match its event")
		}
		if detail.Phase == "started" && detail.Status != "" {
			return nil, errors.New("business action start must not include a result status")
		}
		if detail.Phase == "result" && detail.Status != "succeeded" && detail.Status != "failed" && detail.Status != "unknown" {
			return nil, errors.New("business action result status is invalid")
		}
		id := key{workspaceID: event.WorkspaceID, runID: event.RunID, nodeID: event.NodeID,
			memberID: event.MemberID, invocationID: detail.InvocationID, callID: detail.CallID}
		current, exists := receipts[id]
		if detail.Phase == "started" {
			if exists {
				if !sameBusinessActionDetail(current.detail, detail) {
					return nil, errors.New("business action call has conflicting start records")
				}
				continue
			}
			receipts[id] = receipt{started: event, detail: detail}
			order = append(order, id)
			continue
		}
		if !exists || !sameBusinessActionDetail(current.detail, detail) {
			return nil, errors.New("business action result has no matching start record")
		}
		current.status = detail.Status
		receipts[id] = current
	}
	if len(receipts) > MaxBusinessActionOutcomesPerRun {
		return nil, ErrBusinessActionOutcomeLimitExceeded
	}
	items := make([]BusinessActionOutcomeV1, 0, len(receipts))
	for _, id := range order {
		item := receipts[id]
		status := item.status
		if status == "" {
			status = "unknown"
		}
		label := boundedBusinessActionLabel(item.detail.ActionLabel)
		if label == "" {
			label = boundedBusinessActionLabel(item.detail.ActionName)
		}
		items = append(items, BusinessActionOutcomeV1{
			NodeID: item.started.NodeID, CallID: item.detail.CallID,
			ActionName: label, ObjectName: item.detail.ObjectName,
			RecordID: item.detail.RecordID, Status: status,
			Summary: businessActionOutcomeSummary(label, status),
		})
	}
	return items, nil
}

func boundedBusinessActionLabel(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) > 128 {
		return string(runes[:127]) + "…"
	}
	return value
}

func sameBusinessActionDetail(left, right businessActionActivityDetailV1) bool {
	return left.Source == right.Source && left.InvocationID == right.InvocationID && left.CallID == right.CallID &&
		left.CapabilityID == right.CapabilityID && left.ActionKey == right.ActionKey &&
		left.ActionName == right.ActionName && left.ActionLabel == right.ActionLabel &&
		left.ObjectName == right.ObjectName && left.InputRevisionID == right.InputRevisionID &&
		left.RecordID == right.RecordID && left.FrozenRecordSHA256 == right.FrozenRecordSHA256
}

func businessActionOutcomeSummary(label, status string) string {
	switch status {
	case "succeeded":
		return "平台记录：业务动作“" + label + "”已确认完成。"
	case "failed":
		return "平台记录：业务动作“" + label + "”返回失败。"
	default:
		return "平台记录：业务动作“" + label + "”结果未知，请先核对业务记录。"
	}
}

type BusinessActionActivityStore interface {
	ListBusinessActionEvents(ctx context.Context, workspaceID, runID string) ([]ActivityEvent, error)
	RecordBusinessActionEvent(ctx context.Context, event ActivityEvent) error
	CheckBusinessActionReplay(ctx context.Context, workspaceID, runID, nodeID, invocationID, callID, inputRevisionID, capabilityID, recordID string) (status string, blocked bool, err error)
}
