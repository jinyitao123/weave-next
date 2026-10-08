package teamrun

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/businessaction"
)

func TestPGFailureReasonReadsExactJournalWithoutChangingReplayOrLedger(t *testing.T) {
	for _, mismatch := range []string{"", "workspace", "parent", "node", "member", "invocation", "slot", "tool", "call", "input-hash", "unknown", "journal-sql-unavailable", "activity-sql-unavailable"} {
		t.Run("scope-"+mismatch, func(t *testing.T) {
			h := newProcessNextHarness(t)
			runID := "failure-reason"
			_, running := h.seedRunningWorkflowTaskBeforeAdmission(t, runID)
			store := &PGActivityStore{Transactions: h.pool}
			memberID, memberRun, memberCall := "lead-agent", "native-member-run", "native-member-call"
			slot := memberRun + "/000000000001/chat/000000000002"
			invocation := running.RunSnapshotID + "/0/" + memberCall
			operationID := execution.EngineOperationID("revision-1", invocation, slot, replayCapability)
			status := "failed"
			if mismatch == "unknown" {
				status = "unknown"
			}
			for _, phase := range []string{"started", "result"} {
				eventStatus := ""
				if phase == "result" {
					eventStatus = status
				}
				event := operationActivity(runID, operationID, "original-call", digestA, phase, eventStatus, nil)
				var detail map[string]any
				_ = json.Unmarshal(event.Detail, &detail)
				detail["operation_slot"], detail["invocation_id"] = slot, invocation
				event.Detail, _ = json.Marshal(detail)
				recordActionActivity(t, store, event)
			}
			tool, _ := businessaction.CapabilityToolName(replayCapability)
			call := contract.ToolCall{ID: "original-call", Name: tool, Args: `{"params":{}}`}
			if mismatch == "tool" {
				call.Name = "other-tool"
			}
			if mismatch == "call" {
				call.ID = "other-call"
			}
			input, _ := json.Marshal(call)
			inputHash, _ := frozen.HashCanonicalJSON(input)
			if mismatch == "input-hash" {
				inputHash = digestB
			}
			const reason = "销售业务设置缺少项目客户分类；请先由管理员维护分类后再转化线索"
			op, _ := json.Marshal(map[string]any{"kind": "tool", "input": call, "input_hash": inputHash,
				"response": contract.ToolResult{CallID: call.ID, ToolName: call.Name, Content: "action 'ContractSubmit' threw: Error: " + reason, IsError: true}})
			workspace, parent, node, snapshot := "workspace-1", runID, "lead", running.RunSnapshotID
			switch mismatch {
			case "workspace":
				workspace = "other-workspace"
			case "parent":
				parent = "other-parent"
			case "node":
				node = "other-node"
			case "member":
				memberID = "other-member"
			case "invocation":
				memberCall = "other-invocation"
			case "slot":
				slot += "-other"
			}
			initial, _ := json.Marshal(map[string]string{"__agent_id": memberID})
			_, err := h.pool.Exec(t.Context(), `INSERT INTO weave_workflow_member_runs
				(workspace_id,parent_run_id,member_run_id,call_id,run_snapshot_id,node_id,parent_generation,identity_hash,initial_state)
				VALUES($1,$2,$3,$4,$5,$6,1,$7,$8)`, workspace, parent, memberRun, memberCall, snapshot, node, digestA, string(initial))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := h.pool.Exec(t.Context(), `INSERT INTO loom_store(namespace,key,value) VALUES($1,$2,$3)`, "member-operation:"+workspace, slot, op); err != nil {
				t.Fatal(err)
			}
			if mismatch == "journal-sql-unavailable" {
				if _, err := h.pool.Exec(t.Context(), `ALTER TABLE loom_store RENAME TO unavailable_journal`); err != nil {
					t.Fatal(err)
				}
			}
			if mismatch == "activity-sql-unavailable" {
				if _, err := h.pool.Exec(t.Context(), `ALTER TABLE weave_team_run_activity_events RENAME TO unavailable_activity`); err != nil {
					t.Fatal(err)
				}
				if _, err := store.ListBusinessActionEvents(t.Context(), "workspace-1", runID); err == nil {
					t.Fatal("missing authoritative activity was hidden by optional reason recovery")
				}
				return
			}
			ledgerDigest := func() string {
				var digest string
				if err := h.pool.QueryRow(t.Context(), `SELECT md5(string_agg(detail::text,'' ORDER BY seq)) FROM weave_team_run_activity_events WHERE run_id=$1`, runID).Scan(&digest); err != nil {
					t.Fatal(err)
				}
				return digest
			}
			before := ledgerDigest()
			events, err := store.ListBusinessActionEvents(t.Context(), "workspace-1", runID)
			if err != nil {
				t.Fatal(err)
			}
			outcomes, err := ProjectBusinessActionOutcomes(events)
			if err != nil || len(outcomes) != 1 || outcomes[0].Status != status || strings.Contains(outcomes[0].Summary, reason) != (mismatch == "") {
				t.Fatalf("history projection crossed its scope: %+v %v", outcomes, err)
			}
			check := BusinessActionOperationReconcileCheck{WorkspaceID: "workspace-1", RunID: runID, NodeID: "lead", MemberID: "lead-agent", InvocationID: invocation, OperationSlot: memberRun + "/000000000001/chat/000000000002"}
			decision, err := store.ReconcileBusinessActionOperation(t.Context(), check)
			if err != nil || !decision.Blocked || decision.Status != status || decision.SameOperation || decision.Result != nil {
				t.Fatalf("display projection changed unrecoverable receipt: %+v %v", decision, err)
			}
			if after := ledgerDigest(); after != before {
				t.Fatalf("display projection mutated ledger: %s != %s", after, before)
			}
		})
	}
}
