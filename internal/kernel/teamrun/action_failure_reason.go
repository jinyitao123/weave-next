package teamrun

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/businessaction"
)

// Existing failed events can recover a display reason from their exact private
// journal slot. This changes only this read's projection: no event, outcome,
// cached receipt or replay decision is written or upgraded.
func (store *PGActivityStore) enrichBusinessActionFailureReasons(ctx context.Context, events []ActivityEvent) {
	type identity struct{ node, member, operation string }
	starts := map[identity]businessActionActivityDetailV1{}
	var tx pgx.Tx
	defer func() {
		if tx != nil {
			_ = tx.Rollback(ctx)
		}
	}()
	for index, event := range events {
		var detail businessActionActivityDetailV1
		if json.Unmarshal(event.Detail, &detail) != nil || detail.Source != businessaction.ActionOutcomeSourceForgeMCP {
			continue
		}
		key := identity{event.NodeID, event.MemberID, detail.OperationID}
		if event.Kind == "business_action_started" && detail.Phase == "started" {
			starts[key] = detail
			continue
		}
		started, found := starts[key]
		if event.Kind != "business_action_result" || detail.Phase != "result" || detail.Status != businessaction.ActionOutcomeStatusFailed ||
			detail.PublicReason != "" || !found || !sameBusinessActionDetail(started, detail) {
			continue
		}
		reason := businessaction.PublicActionFailureReason(detail.Result, detail.ActionName)
		if reason == "" && detail.OperationSlot != "" && len(detail.OperationSlot) <= MaxBusinessActionOperationSlotBytes &&
			detail.OperationID == execution.EngineOperationID(detail.InputRevisionID, detail.InvocationID, detail.OperationSlot, detail.CapabilityID) {
			var raw []byte
			var snapshot, callID string
			if tx == nil {
				var err error
				tx, err = store.Transactions.Begin(ctx)
				if err != nil {
					return
				}
			}
			err := tx.QueryRow(ctx, `SELECT j.value,m.run_snapshot_id,m.call_id
				FROM loom_store j
				JOIN weave_workflow_member_runs m ON m.workspace_id=$1 AND split_part(j.key,'/',1)=m.member_run_id
				JOIN weave_team_runs r ON r.workspace_id=m.workspace_id AND r.run_id=m.parent_run_id AND r.run_snapshot_id=m.run_snapshot_id
				WHERE j.namespace='member-operation:' || $1 AND j.key=$2 AND octet_length(j.value)<=65536
				  AND m.parent_run_id=$3 AND m.node_id=$4 AND m.initial_state->>'__agent_id'=$5`,
				event.WorkspaceID, detail.OperationSlot, event.RunID, event.NodeID, event.MemberID).Scan(&raw, &snapshot, &callID)
			if err == nil && strings.HasPrefix(detail.InvocationID, snapshot+"/") && strings.HasSuffix(detail.InvocationID, "/"+callID) {
				generation := strings.TrimSuffix(strings.TrimPrefix(detail.InvocationID, snapshot+"/"), "/"+callID)
				if _, err := strconv.ParseUint(generation, 10, 64); err == nil {
					reason = journalBusinessActionFailureReason(raw, detail)
				}
			}
		}
		if reason != "" {
			detail.PublicReason = reason
			events[index].Detail, _ = json.Marshal(detail)
		}
	}
}

func journalBusinessActionFailureReason(raw []byte, detail businessActionActivityDetailV1) string {
	var operation struct {
		Kind      string              `json:"kind"`
		Input     json.RawMessage     `json:"input"`
		InputHash string              `json:"input_hash"`
		Response  contract.ToolResult `json:"response"`
	}
	if json.Unmarshal(raw, &operation) != nil || operation.Kind != "tool" {
		return ""
	}
	digest, err := frozen.HashCanonicalJSON(operation.Input)
	if err != nil || digest != operation.InputHash {
		return ""
	}
	var call contract.ToolCall
	tool, err := businessaction.CapabilityToolName(detail.CapabilityID)
	if err != nil || json.Unmarshal(operation.Input, &call) != nil || call.ID != detail.CallID || call.Name != tool ||
		operation.Response.CallID != call.ID || operation.Response.ToolName != tool {
		return ""
	}
	return businessaction.PublicActionFailureReason(&operation.Response, detail.ActionName)
}
