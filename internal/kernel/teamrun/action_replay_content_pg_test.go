package teamrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/businessaction"
)

const (
	replayCapability = "forge:action:sales_contract.ContractSubmit"
	replayRecord     = "private-record-id"
	digestA          = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB          = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func operationActivity(runID, operationID, callID, digest, phase, status string, result *contract.ToolResult) ActivityEvent {
	event := actionActivityEvent("business_action_"+phase, "lead", "lead-agent", phase,
		"snapshot/0/lead", callID, "ContractSubmit", "提交指定合同版本", status)
	var detail map[string]any
	_ = json.Unmarshal(event.Detail, &detail)
	detail["operation_id"], detail["params_sha256"] = operationID, digest
	if result != nil {
		detail["result"] = result
	}
	event.Detail, _ = json.Marshal(detail)
	event.WorkspaceID, event.RunID, event.EventID, event.OccurredAt = "workspace-1", runID, uuid.NewString(), time.Now().UTC()
	return event
}

func operationCheck(runID, operationID, callID, digest string) BusinessActionReplayCheck {
	return BusinessActionReplayCheck{
		WorkspaceID: "workspace-1", RunID: runID, NodeID: "lead", InvocationID: "snapshot/0/lead", CallID: callID,
		OperationID: operationID, InputRevisionID: "revision-1", CapabilityID: replayCapability,
		RecordID: replayRecord, ParamsSHA256: digest,
	}
}

func recordOperation(t *testing.T, store *PGActivityStore, runID, operationID, callID, digest, status string, result *contract.ToolResult) {
	t.Helper()
	recordActionActivity(t, store, operationActivity(runID, operationID, callID, digest, "started", "", nil))
	if status != "" {
		recordActionActivity(t, store, operationActivity(runID, operationID, callID, digest, "result", status, result))
	}
}

// This fixture models the actual precheck/reserve/send/record contract. The
// network effect happens only after the PG reservation succeeds. Both duplicate
// prechecks may see empty state; the reservation is the dispatch authority.
func sendOperation(ctx context.Context, store *PGActivityStore, endpoint, runID, operationID, callID, digest string) (*contract.ToolResult, error) {
	check := operationCheck(runID, operationID, callID, digest)
	decision, err := store.CheckBusinessActionReplay(ctx, check)
	if err != nil {
		return nil, err
	}
	return sendPrecheckedOperation(ctx, store, endpoint, runID, operationID, callID, digest, decision)
}

func sendPrecheckedOperation(ctx context.Context, store *PGActivityStore, endpoint, runID, operationID, callID, digest string, decision BusinessActionReplayDecision) (*contract.ToolResult, error) {
	cached := func(decision BusinessActionReplayDecision) (*contract.ToolResult, error) {
		if !decision.SameOperation || decision.Status == "unknown" || decision.Result == nil {
			return nil, businessaction.ErrActionOutcomeUnresolved
		}
		result := *decision.Result
		result.CallID, result.ToolName = callID, "forge_submit"
		return &result, nil
	}
	if decision.Blocked {
		return cached(decision)
	}
	if err := store.RecordBusinessActionEvent(ctx, operationActivity(runID, operationID, callID, digest, "started", "", nil)); err != nil {
		if !errors.Is(err, businessaction.ErrActionAlreadyRecorded) {
			return nil, err
		}
		decision, err := store.CheckBusinessActionReplay(ctx, operationCheck(runID, operationID, callID, digest))
		if err != nil {
			return nil, err
		}
		return cached(decision)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return nil, err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	content, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	result := &contract.ToolResult{CallID: callID, ToolName: "forge_submit", Content: string(content)}
	if err := store.RecordBusinessActionEvent(ctx, operationActivity(runID, operationID, callID, digest, "result", "succeeded", result)); err != nil {
		return nil, err
	}
	return businessaction.SanitizeActionOutcomeResult(result), nil
}

func TestPGOperationReplayReturnsOriginalNativeReceiptAndOneNetworkEffect(t *testing.T) {
	h := newProcessNextHarness(t)
	runID := "operation-replay"
	_, _ = h.seedRunningWorkflowTaskBeforeAdmission(t, runID)
	store := &PGActivityStore{Transactions: h.pool}
	var effects atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		effects.Add(1)
		_, _ = io.WriteString(w, `{"ok":true,"data":{"approvalId":"approval-123","amount":900,"status":"submitted"}}`)
	}))
	defer server.Close()
	original, err := sendOperation(t.Context(), store, server.URL, runID, "slot-1", "call-original", digestA)
	if err != nil {
		t.Fatal(err)
	}
	fresh := &PGActivityStore{Transactions: h.pool}
	replayed, err := sendOperation(t.Context(), fresh, server.URL, runID, "slot-1", "call-after-restart", digestA)
	if err != nil || replayed == nil || replayed.Content != original.Content || replayed.CallID != "call-after-restart" || effects.Load() != 1 {
		t.Fatalf("native receipt replay failed: original=%+v replayed=%+v effects=%d err=%v", original, replayed, effects.Load(), err)
	}
	events, err := store.ListBusinessActionEvents(t.Context(), "workspace-1", runID)
	if err != nil || len(events) != 2 {
		t.Fatalf("replay created another audit action: events=%d err=%v", len(events), err)
	}
	projection, err := ProjectBusinessActionOutcomes(events)
	raw, _ := json.Marshal(projection)
	var public []map[string]any
	_ = json.Unmarshal(raw, &public)
	if err != nil || len(public) != 1 || len(public[0]) != 7 || public[0]["operation_id"] != nil || public[0]["result"] != nil {
		t.Fatalf("private replay state leaked into employee projection: %s err=%v", raw, err)
	}
}

func TestPGDifferentOperationsPermit900To1000To900WithSameModelCallID(t *testing.T) {
	h := newProcessNextHarness(t)
	runID := "operation-intentional-repeat"
	_, _ = h.seedRunningWorkflowTaskBeforeAdmission(t, runID)
	store := &PGActivityStore{Transactions: h.pool}
	var effects atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		effects.Add(1)
		_, _ = io.WriteString(w, `{"ok":true,"data":{"status":"adjusted"}}`)
	}))
	defer server.Close()
	for index, digest := range []string{digestA, digestB, digestA} {
		if _, err := sendOperation(t.Context(), store, server.URL, runID, fmt.Sprintf("slot-%d", index), "reused-model-call", digest); err != nil {
			t.Fatalf("intentional price adjustment %d blocked: %v", index, err)
		}
	}
	if effects.Load() != 3 {
		t.Fatalf("intentional identical request was mistaken for recovery: effects=%d", effects.Load())
	}
	events, err := store.ListBusinessActionEvents(t.Context(), "workspace-1", runID)
	outcomes, projectionErr := ProjectBusinessActionOutcomes(events)
	if err != nil || projectionErr != nil || len(outcomes) != 3 {
		t.Fatalf("distinct operations collapsed in audit: outcomes=%+v err=%v projection=%v", outcomes, err, projectionErr)
	}
}

func TestPGSameOperationRejectsChangedRequestIdentityBeforeReservation(t *testing.T) {
	h := newProcessNextHarness(t)
	runID := "operation-conflict"
	_, _ = h.seedRunningWorkflowTaskBeforeAdmission(t, runID)
	store := &PGActivityStore{Transactions: h.pool}
	recordOperation(t, store, runID, "slot-1", "call-original", digestA, "succeeded", &contract.ToolResult{Content: `{"ok":true}`})
	for _, field := range []string{"params", "capability", "record"} {
		t.Run(field, func(t *testing.T) {
			check := operationCheck(runID, "slot-1", "call-new", digestA)
			switch field {
			case "params":
				check.ParamsSHA256 = digestB
			case "capability":
				check.CapabilityID = "forge:action:sales_quote.AdjustPrice"
			case "record":
				check.RecordID = "another-record"
			}
			if _, err := store.CheckBusinessActionReplay(t.Context(), check); !errors.Is(err, businessaction.ErrActionOperationConflict) {
				t.Fatalf("changed %s was not a conflict: %v", field, err)
			}
			started := operationActivity(runID, "slot-1", "call-new", check.ParamsSHA256, "started", "", nil)
			var detail map[string]any
			_ = json.Unmarshal(started.Detail, &detail)
			detail["capability_id"], detail["record_id"] = check.CapabilityID, check.RecordID
			started.Detail, _ = json.Marshal(detail)
			if err := store.RecordBusinessActionEvent(t.Context(), started); !errors.Is(err, businessaction.ErrActionOperationConflict) {
				t.Fatalf("reservation accepted changed %s: %v", field, err)
			}
		})
	}
}

func TestPGKnownFailureReturnsOriginalReceiptWithoutResending(t *testing.T) {
	h := newProcessNextHarness(t)
	runID := "operation-known-failure"
	_, _ = h.seedRunningWorkflowTaskBeforeAdmission(t, runID)
	store := &PGActivityStore{Transactions: h.pool}
	native := &contract.ToolResult{CallID: "failed-call", Content: `{"ok":false,"error":{"code":"VERSION_CONFLICT","message":"版本已变化"}}`, IsError: true}
	recordOperation(t, store, runID, "slot-1", "failed-call", digestA, "failed", native)
	decision, err := store.CheckBusinessActionReplay(t.Context(), operationCheck(runID, "slot-1", "call-after-restart", digestA))
	if err != nil || !decision.Blocked || !decision.SameOperation || decision.Status != "failed" || !reflect.DeepEqual(decision.Result, businessaction.SanitizeActionOutcomeResult(native)) {
		t.Fatalf("explicit failure did not replay the native receipt: decision=%+v err=%v", decision, err)
	}
	if err := store.RecordBusinessActionEvent(t.Context(), operationActivity(runID, "slot-1", "new-call", digestA, "started", "", nil)); !errors.Is(err, businessaction.ErrActionAlreadyRecorded) {
		t.Fatalf("known failure permitted dispatch: %v", err)
	}
}

func TestPGUnknownOutcomeProtectsTargetAndOutranksCachedSuccess(t *testing.T) {
	for _, status := range []string{"", "unknown"} {
		t.Run("status-"+status, func(t *testing.T) {
			h := newProcessNextHarness(t)
			runID := "operation-unknown"
			_, _ = h.seedRunningWorkflowTaskBeforeAdmission(t, runID)
			store := &PGActivityStore{Transactions: h.pool}
			recordOperation(t, store, runID, "slot-known", "call-known", digestA, "succeeded", &contract.ToolResult{Content: `{"ok":true}`})
			recordOperation(t, store, runID, "slot-unknown", "call-unknown", digestB, status, nil)
			for _, op := range []string{"slot-unknown", "slot-new", "slot-known"} {
				digest := digestA
				if op == "slot-unknown" {
					digest = digestB
				}
				decision, err := store.CheckBusinessActionReplay(t.Context(), operationCheck(runID, op, "new-call", digest))
				if err != nil || !decision.Blocked || decision.Status != "unknown" || decision.Result != nil {
					t.Fatalf("unknown guard failed for %s: decision=%+v err=%v", op, decision, err)
				}
				if err := store.RecordBusinessActionEvent(t.Context(), operationActivity(runID, op, "new-call", digest, "started", "", nil)); !errors.Is(err, businessaction.ErrActionOutcomeUnresolved) {
					t.Fatalf("unknown reservation permitted dispatch: %v", err)
				}
			}
		})
	}
}

func TestPGOperationReplayDoesNotCrossRunInputOrWorkspace(t *testing.T) {
	h := newProcessNextHarness(t)
	runID, otherRunID := "operation-scope-a", "operation-scope-b"
	_, _ = h.seedRunningWorkflowTaskBeforeAdmission(t, runID)
	_, _ = h.seedRunningWorkflowTaskBeforeAdmission(t, otherRunID)
	store := &PGActivityStore{Transactions: h.pool}
	recordOperation(t, store, runID, "same-slot", "original", digestA, "succeeded", &contract.ToolResult{Content: `{"ok":true}`})
	for _, field := range []string{"run", "input", "workspace"} {
		check := operationCheck(runID, "same-slot", "new-call", digestA)
		switch field {
		case "run":
			check.RunID = otherRunID
		case "input":
			check.InputRevisionID = "revision-2"
		case "workspace":
			check.WorkspaceID = "workspace-2"
		}
		if decision, err := store.CheckBusinessActionReplay(t.Context(), check); err != nil || decision.Blocked {
			t.Fatalf("operation replay crossed %s: decision=%+v err=%v", field, decision, err)
		}
	}
}

func TestPGConcurrentSameOperationSendsOnceIncludingCompletedBeforeSecondReservation(t *testing.T) {
	for _, firstCompleted := range []bool{false, true} {
		t.Run(fmt.Sprintf("first-completed-%t", firstCompleted), func(t *testing.T) {
			h := newProcessNextHarness(t)
			runID := "operation-race"
			_, _ = h.seedRunningWorkflowTaskBeforeAdmission(t, runID)
			store := &PGActivityStore{Transactions: h.pool}
			var effects atomic.Int32
			firstSent, releaseForge := make(chan struct{}), make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if effects.Add(1) == 1 {
					close(firstSent)
					<-releaseForge
				}
				_, _ = io.WriteString(w, `{"ok":true,"data":{"approvalId":"one-approval"}}`)
			}))
			defer server.Close()
			// Both workers precheck before either creates the PG reservation.
			first, err := store.CheckBusinessActionReplay(t.Context(), operationCheck(runID, "shared-slot", "call-a", digestA))
			if err != nil || first.Blocked {
				t.Fatalf("first precheck: %+v %v", first, err)
			}
			second, err := store.CheckBusinessActionReplay(t.Context(), operationCheck(runID, "shared-slot", "call-b", digestA))
			if err != nil || second.Blocked {
				t.Fatalf("second precheck: %+v %v", second, err)
			}
			type completed struct {
				result *contract.ToolResult
				err    error
			}
			firstDone := make(chan completed, 1)
			go func() {
				result, err := sendPrecheckedOperation(t.Context(), store, server.URL, runID, "shared-slot", "call-a", digestA, first)
				firstDone <- completed{result, err}
			}()
			select {
			case <-firstSent:
			case result := <-firstDone:
				t.Fatalf("first worker failed before dispatch: %v", result.err)
			}
			var original completed
			if firstCompleted {
				close(releaseForge)
				original = <-firstDone
				if original.err != nil || original.result == nil {
					t.Fatalf("first worker failed after dispatch: %v", original.err)
				}
			}
			replayed, replayErr := sendPrecheckedOperation(t.Context(), store, server.URL, runID, "shared-slot", "call-b", digestA, second)
			if !firstCompleted {
				close(releaseForge)
				original = <-firstDone
				if !errors.Is(replayErr, businessaction.ErrActionOutcomeUnresolved) {
					t.Fatalf("pending duplicate was not stopped: %v", replayErr)
				}
			} else if replayErr != nil || replayed == nil || replayed.Content != original.result.Content || replayed.CallID != "call-b" {
				t.Fatalf("completed duplicate did not replay original: result=%+v err=%v", replayed, replayErr)
			}
			if original.err != nil || effects.Load() != 1 {
				t.Fatalf("network was repeated: effects=%d first-error=%v", effects.Load(), original.err)
			}
		})
	}
}

func TestPGLegacyReceiptUsesCallIdentityAndUnknownProtectionWithoutContentDedup(t *testing.T) {
	h := newProcessNextHarness(t)
	runID := "operation-legacy"
	_, _ = h.seedRunningWorkflowTaskBeforeAdmission(t, runID)
	store := &PGActivityStore{Transactions: h.pool}
	recordOperation(t, store, runID, "", "legacy-call", digestA, "succeeded", nil)
	decision, err := store.CheckBusinessActionReplay(t.Context(), operationCheck(runID, "modern-slot", "legacy-call", digestA))
	if err != nil || !decision.Blocked || decision.SameOperation {
		t.Fatalf("legacy call identity lost: %+v %v", decision, err)
	}
	decision, err = store.CheckBusinessActionReplay(t.Context(), operationCheck(runID, "modern-slot", "new-call", digestA))
	if err != nil || decision.Blocked {
		t.Fatalf("legacy content-only match must not stop intentional action: %+v %v", decision, err)
	}
	recordOperation(t, store, runID, "", "legacy-unknown", digestB, "", nil)
	decision, err = store.CheckBusinessActionReplay(t.Context(), operationCheck(runID, "modern-slot", "new-call", digestA))
	if err != nil || !decision.Blocked || decision.Status != "unknown" {
		t.Fatalf("legacy unknown protection lost: %+v %v", decision, err)
	}
}

func TestPGOperationReservationRejectsEventIDCollision(t *testing.T) {
	h := newProcessNextHarness(t)
	runID := "operation-event-collision"
	_, _ = h.seedRunningWorkflowTaskBeforeAdmission(t, runID)
	store := &PGActivityStore{Transactions: h.pool}
	original := operationActivity(runID, "slot-1", "same-model-call", digestA, "started", "", nil)
	recordActionActivity(t, store, original)
	recordActionActivity(t, store, operationActivity(runID, "slot-1", "same-model-call", digestA, "result", "succeeded", &contract.ToolResult{Content: `{"ok":true}`}))
	collision := operationActivity(runID, "slot-2", "same-model-call", digestA, "started", "", nil)
	collision.EventID = original.EventID
	if err := store.RecordBusinessActionEvent(t.Context(), collision); !errors.Is(err, businessaction.ErrActionOperationConflict) {
		t.Fatalf("event collision admitted an unreserved external operation: %v", err)
	}
}

func TestPGOperationReceiptSanitizesAndPreservesOriginalResult(t *testing.T) {
	h := newProcessNextHarness(t)
	runID := "operation-receipt-integrity"
	_, _ = h.seedRunningWorkflowTaskBeforeAdmission(t, runID)
	store := &PGActivityStore{Transactions: h.pool}
	original := &contract.ToolResult{CallID: "call-1", ToolName: "forge_submit", Content: `{"ok":true,"data":{"approvalId":"approval-123","params":{"private":"discard"},"access_token":"discard"}}`,
		StatePatch: map[string]any{"secret": "discard"}, StopLoop: true}
	recordOperation(t, store, runID, "slot-1", "call-1", digestA, "succeeded", original)
	decision, err := store.CheckBusinessActionReplay(t.Context(), operationCheck(runID, "slot-1", "new-call", digestA))
	if err != nil || decision.Result == nil || decision.Result.Content != `{"data":{"approvalId":"approval-123"},"ok":true}` ||
		decision.Result.StatePatch != nil || decision.Result.StopLoop {
		t.Fatalf("cached receipt was not bounded to native business fields: %+v err=%v", decision, err)
	}
	changed := &contract.ToolResult{CallID: "call-1", Content: `{"ok":true,"data":{"approvalId":"different-approval"}}`}
	if err := store.RecordBusinessActionEvent(t.Context(), operationActivity(runID, "slot-1", "call-1", digestA, "result", "succeeded", changed)); !errors.Is(err, businessaction.ErrActionOperationConflict) {
		t.Fatalf("original trusted receipt was overwritten: %v", err)
	}
	unchanged, err := store.CheckBusinessActionReplay(t.Context(), operationCheck(runID, "slot-1", "new-call", digestA))
	if err != nil || !reflect.DeepEqual(decision.Result, unchanged.Result) {
		t.Fatalf("original receipt changed after rejected overwrite: %+v %v", unchanged, err)
	}
}

func TestPGOperationReceiptRejectsContradictoryCachedStatus(t *testing.T) {
	for _, status := range []string{"succeeded", "failed"} {
		t.Run(status, func(t *testing.T) {
			h := newProcessNextHarness(t)
			runID := "operation-invalid-cache"
			_, _ = h.seedRunningWorkflowTaskBeforeAdmission(t, runID)
			store := &PGActivityStore{Transactions: h.pool}
			recordOperation(t, store, runID, "slot-1", "call-1", digestA, "", nil)
			contradictory := &contract.ToolResult{Content: `{"ok":true}`, IsError: status == "succeeded"}
			if err := store.RecordBusinessActionEvent(t.Context(), operationActivity(runID, "slot-1", "call-1", digestA, "result", status, contradictory)); err == nil {
				t.Fatal("status contradicted native result but was cached as trusted")
			}
			decision, err := store.CheckBusinessActionReplay(t.Context(), operationCheck(runID, "slot-1", "new-call", digestA))
			if err != nil || decision.Status != "unknown" || decision.Result != nil {
				t.Fatalf("invalid receipt changed unknown reservation: %+v %v", decision, err)
			}
		})
	}
}

func slottedOperationActivity(runID, slot, callID, phase, status string, result *contract.ToolResult) ActivityEvent {
	operationID := execution.EngineOperationID("revision-1", "snapshot/0/lead", slot, replayCapability)
	event := operationActivity(runID, operationID, callID, digestA, phase, status, result)
	var detail map[string]any
	_ = json.Unmarshal(event.Detail, &detail)
	detail["operation_slot"] = slot
	event.Detail, _ = json.Marshal(detail)
	return event
}

func operationReconcileCheck(runID, slot string) BusinessActionOperationReconcileCheck {
	return BusinessActionOperationReconcileCheck{WorkspaceID: "workspace-1", RunID: runID,
		NodeID: "lead", MemberID: "lead-agent", InvocationID: "snapshot/0/lead", OperationSlot: slot}
}

func TestPGOperationReconciliationReadsConfirmedReceiptWithoutWriting(t *testing.T) {
	for _, status := range []string{"succeeded", "failed"} {
		t.Run(status, func(t *testing.T) {
			h := newProcessNextHarness(t)
			runID, slot := "operation-reconcile", "member-run/segment/000000000002"
			_, _ = h.seedRunningWorkflowTaskBeforeAdmission(t, runID)
			store := &PGActivityStore{Transactions: h.pool}
			content := `{"ok":true,"data":{"approvalId":"approval-123"}}`
			if status == "failed" {
				content = `{"ok":false,"error":{"code":"STALE_RECORD"}}`
			}
			native := &contract.ToolResult{CallID: "original-call", ToolName: "forge_submit", Content: content, IsError: status == "failed"}
			recordActionActivity(t, store, slottedOperationActivity(runID, slot, "original-call", "started", "", nil))
			recordActionActivity(t, store, slottedOperationActivity(runID, slot, "original-call", "result", status, native))
			before, err := store.ListBusinessActionEvents(t.Context(), "workspace-1", runID)
			if err != nil {
				t.Fatal(err)
			}
			fresh := &PGActivityStore{Transactions: h.pool}
			decision, err := fresh.ReconcileBusinessActionOperation(t.Context(), operationReconcileCheck(runID, slot))
			if err != nil || !decision.Blocked || !decision.SameOperation || decision.Status != status ||
				!reflect.DeepEqual(decision.Result, businessaction.SanitizeActionOutcomeResult(native)) {
				t.Fatalf("confirmed journal slot did not recover native receipt: %+v %v", decision, err)
			}
			after, err := store.ListBusinessActionEvents(t.Context(), "workspace-1", runID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("read-only reconciliation wrote activity: before=%+v after=%+v err=%v", before, after, err)
			}
		})
	}
}

func TestPGOperationReconciliationCannotResolveMissingUnknownOrSlotlessReceipt(t *testing.T) {
	for _, scenario := range []string{"missing", "pending", "unknown", "known-without-cache", "slotless"} {
		t.Run(scenario, func(t *testing.T) {
			h := newProcessNextHarness(t)
			runID, slot := "operation-reconcile-unresolved", "member-run/segment/000000000002"
			_, _ = h.seedRunningWorkflowTaskBeforeAdmission(t, runID)
			store := &PGActivityStore{Transactions: h.pool}
			if scenario == "slotless" {
				recordOperation(t, store, runID, "old-modern-operation", "call-1", digestA, "succeeded", &contract.ToolResult{Content: `{"ok":true}`})
			} else if scenario != "missing" {
				recordActionActivity(t, store, slottedOperationActivity(runID, slot, "call-1", "started", "", nil))
				if scenario == "unknown" || scenario == "known-without-cache" {
					status := "unknown"
					if scenario == "known-without-cache" {
						status = "succeeded"
					}
					recordActionActivity(t, store, slottedOperationActivity(runID, slot, "call-1", "result", status, nil))
				}
			}
			decision, err := store.ReconcileBusinessActionOperation(t.Context(), operationReconcileCheck(runID, slot))
			if err != nil || decision.SameOperation || decision.Result != nil {
				t.Fatalf("unconfirmed scenario %s became resolved: %+v %v", scenario, decision, err)
			}
		})
	}
}

func TestPGOperationReconciliationCannotCrossTrustedExecutionScope(t *testing.T) {
	h := newProcessNextHarness(t)
	runID, slot := "operation-reconcile-scope", "member-run/segment/000000000002"
	_, _ = h.seedRunningWorkflowTaskBeforeAdmission(t, runID)
	store := &PGActivityStore{Transactions: h.pool}
	recordActionActivity(t, store, slottedOperationActivity(runID, slot, "call-1", "started", "", nil))
	recordActionActivity(t, store, slottedOperationActivity(runID, slot, "call-1", "result", "succeeded", &contract.ToolResult{Content: `{"ok":true}`}))
	for _, field := range []string{"workspace", "run", "node", "member", "invocation", "slot"} {
		check := operationReconcileCheck(runID, slot)
		switch field {
		case "workspace":
			check.WorkspaceID = "other-workspace"
		case "run":
			check.RunID = "other-run"
		case "node":
			check.NodeID = "other-node"
		case "member":
			check.MemberID = "other-member"
		case "invocation":
			check.InvocationID = "other-invocation"
		case "slot":
			check.OperationSlot = "other-slot"
		}
		decision, err := store.ReconcileBusinessActionOperation(t.Context(), check)
		if err != nil || decision.SameOperation || decision.Result != nil {
			t.Fatalf("reconciliation crossed %s: %+v %v", field, decision, err)
		}
	}
}

func TestPGOperationReconciliationRejectsCorruptIdentityAndOversizedSlot(t *testing.T) {
	h := newProcessNextHarness(t)
	runID, slot := "operation-reconcile-invalid", "member-run/segment/000000000002"
	_, _ = h.seedRunningWorkflowTaskBeforeAdmission(t, runID)
	store := &PGActivityStore{Transactions: h.pool}
	started := slottedOperationActivity(runID, slot, "call-1", "started", "", nil)
	var detail map[string]any
	_ = json.Unmarshal(started.Detail, &detail)
	detail["operation_id"] = "forged-operation"
	started.Detail, _ = json.Marshal(detail)
	if err := store.RecordBusinessActionEvent(t.Context(), started); !errors.Is(err, businessaction.ErrActionOperationConflict) {
		t.Fatalf("invalid slot/operation relation recorded: %v", err)
	}
	// Generic activity corruption must not be promoted into a trusted receipt.
	if err := store.Record(t.Context(), started); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReconcileBusinessActionOperation(t.Context(), operationReconcileCheck(runID, slot)); !errors.Is(err, businessaction.ErrActionOperationConflict) {
		t.Fatalf("corrupt durable identity accepted: %v", err)
	}
	oversized := slottedOperationActivity(runID, strings.Repeat("x", MaxBusinessActionOperationSlotBytes+1), "oversized", "started", "", nil)
	if err := store.RecordBusinessActionEvent(t.Context(), oversized); err == nil {
		t.Fatal("oversized operation slot recorded")
	}
}

func TestPGWorkflowOperationReconcilerCorrelatesJournalInputWithoutDispatch(t *testing.T) {
	h := newProcessNextHarness(t)
	runID, slot := "operation-reconcile-callback", "member-run/segment/000000000002"
	_, _ = h.seedRunningWorkflowTaskBeforeAdmission(t, runID)
	store := &PGActivityStore{Transactions: h.pool}
	native := &contract.ToolResult{CallID: "original-call", ToolName: "run_action", Content: `{"ok":true,"data":{"approvalId":"approval-123"}}`}
	recordActionActivity(t, store, slottedOperationActivity(runID, slot, "original-call", "started", "", nil))
	recordActionActivity(t, store, slottedOperationActivity(runID, slot, "original-call", "result", "succeeded", native))
	runtime := &WorkflowSerialRuntime{Activities: store}
	ctx := execution.WithInvocationID(t.Context(), "snapshot/0/lead")
	ctx = context.WithValue(ctx, runtimeActivityScopeKey{}, runtimeActivityScope{NodeID: "lead", MemberID: "lead-agent"})
	ctx = runtime.withBusinessActionOutcomeContext(ctx, TeamRun{WorkspaceID: "workspace-1", RunID: runID})
	input, _ := json.Marshal(contract.ToolCall{ID: "journal-call", Name: "journal-tool", Args: `{"params":{"ignored-for-lookup":true}}`})
	before, _ := store.ListBusinessActionEvents(t.Context(), "workspace-1", runID)
	raw, resolved, err := execution.ReconcileOperation(ctx, slot, input)
	var result contract.ToolResult
	decodeErr := json.Unmarshal(raw, &result)
	if err != nil || !resolved || decodeErr != nil || result.CallID != "journal-call" || result.ToolName != "journal-tool" ||
		result.Content != businessaction.SanitizeActionOutcomeResult(native).Content {
		t.Fatalf("journal response correlation failed: raw=%s resolved=%t err=%v decode=%v", raw, resolved, err, decodeErr)
	}
	after, _ := store.ListBusinessActionEvents(t.Context(), "workspace-1", runID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("journal reconciliation wrote another activity event")
	}
	if _, resolved, err := execution.ReconcileOperation(ctx, "ordinary-tool-slot", input); err != nil || resolved {
		t.Fatalf("ordinary tool was resolved: %t %v", resolved, err)
	}
	if _, resolved, err := execution.ReconcileOperation(ctx, slot, json.RawMessage(`{"name":"missing-call-id"}`)); err == nil || resolved {
		t.Fatalf("invalid journal call was resolved: %t %v", resolved, err)
	}
}
