package teamrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jinyitao123/weave/internal/kernel/businessaction"
)

func recordActionActivity(t *testing.T, store *PGActivityStore, event ActivityEvent) {
	t.Helper()
	if event.EventID == "" {
		event.EventID = uuid.NewString()
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	}
	if err := store.RecordBusinessActionEvent(t.Context(), event); err != nil {
		t.Fatal(err)
	}
}

func TestPGActionOutcomePersistsUnknownReplayGuardAndLimitRealPG(t *testing.T) {
	h := newProcessNextHarness(t)
	runID := "action-outcome-run"
	_, _ = h.seedRunningWorkflowTaskBeforeAdmission(t, runID)
	ctx := context.Background()
	store := &PGActivityStore{Transactions: h.pool}
	started := actionActivityEvent("business_action_started", "lead", "lead-agent", "started", "snapshot/0/lead", "forge-call-1", "ContractSubmit", "提交指定合同版本", "")
	started.WorkspaceID, started.RunID, started.EventID = "workspace-1", runID, uuid.NewString()
	recordActionActivity(t, store, started)

	// A fresh reader represents an API or worker process that restarted after
	// the external call was started but before a result reached the activity DB.
	fresh := &PGActivityStore{Transactions: h.pool}
	events, err := fresh.ListBusinessActionEvents(ctx, "workspace-1", runID)
	if err != nil {
		t.Fatal(err)
	}
	outcomes, err := ProjectBusinessActionOutcomes(events)
	if err != nil || len(outcomes) != 1 || outcomes[0].Status != "unknown" {
		t.Fatalf("started-only action did not survive restart as unknown: outcomes=%+v err=%v", outcomes, err)
	}
	status, blocked, err := fresh.CheckBusinessActionReplay(ctx, "workspace-1", runID, "review", "snapshot/0/review", "new-call-id",
		"revision-1", "forge:action:sales_contract.ContractSubmit", "private-record-id")
	if err != nil || !blocked || status != "unknown" {
		t.Fatalf("unknown action was not guarded from blind retry: status=%s blocked=%v err=%v", status, blocked, err)
	}
	_, blocked, err = fresh.CheckBusinessActionReplay(ctx, "workspace-1", runID, "review", "snapshot/0/review", "new-call-id",
		"revision-1", "forge:action:sales_contract.ContractSubmit", "another-record")
	if err != nil || blocked {
		t.Fatalf("unknown action guard crossed the frozen record boundary: blocked=%v err=%v", blocked, err)
	}

	completed := actionActivityEvent("business_action_result", "lead", "lead-agent", "result", "snapshot/0/lead", "forge-call-1", "ContractSubmit", "提交指定合同版本", "succeeded")
	completed.WorkspaceID, completed.RunID, completed.EventID = "workspace-1", runID, uuid.NewString()
	recordActionActivity(t, store, completed)
	events, err = fresh.ListBusinessActionEvents(ctx, "workspace-1", runID)
	if err != nil {
		t.Fatal(err)
	}
	outcomes, err = ProjectBusinessActionOutcomes(events)
	if err != nil || len(outcomes) != 1 || outcomes[0].Status != "succeeded" {
		t.Fatalf("completed action was not durable: outcomes=%+v err=%v", outcomes, err)
	}
	_, blocked, err = fresh.CheckBusinessActionReplay(ctx, "workspace-1", runID, "review", "snapshot/0/review", "new-call-id",
		"revision-1", "forge:action:sales_contract.ContractSubmit", "private-record-id")
	if err != nil || blocked {
		t.Fatalf("a confirmed result was treated as an unknown replay: blocked=%v err=%v", blocked, err)
	}
	sequential := actionActivityEvent("business_action_started", "review", "review-agent", "started", "snapshot/0/review", "call-after-confirmed-result", "ContractSubmit", "提交指定合同版本", "")
	sequential.WorkspaceID, sequential.RunID, sequential.EventID, sequential.OccurredAt = "workspace-1", runID, uuid.NewString(), time.Now().UTC()
	if err := store.RecordBusinessActionEvent(ctx, sequential); err != nil {
		t.Fatalf("confirmed prior result blocked a later explicit action: %v", err)
	}

	limitedRunID := "action-outcome-limit-run"
	_, _ = h.seedRunningWorkflowTaskBeforeAdmission(t, limitedRunID)
	for index := 0; index < MaxBusinessActionOutcomesPerRun; index++ {
		callID := fmt.Sprintf("limit-call-%03d", index)
		event := actionActivityEvent("business_action_started", "lead", "lead-agent", "started", "snapshot/0/lead", callID, "ContractSubmit", "提交指定合同版本", "")
		event.WorkspaceID, event.RunID, event.EventID = "workspace-1", limitedRunID, uuid.NewString()
		recordActionActivity(t, store, event)
		result := actionActivityEvent("business_action_result", "lead", "lead-agent", "result", "snapshot/0/lead", callID, "ContractSubmit", "提交指定合同版本", "succeeded")
		result.WorkspaceID, result.RunID, result.EventID = "workspace-1", limitedRunID, uuid.NewString()
		recordActionActivity(t, store, result)
	}
	overLimit := actionActivityEvent("business_action_started", "lead", "lead-agent", "started", "snapshot/0/lead", "limit-call-over", "ContractSubmit", "提交指定合同版本", "")
	overLimit.WorkspaceID, overLimit.RunID, overLimit.EventID, overLimit.OccurredAt = "workspace-1", limitedRunID, uuid.NewString(), time.Now().UTC()
	if err := store.RecordBusinessActionEvent(ctx, overLimit); !errors.Is(err, ErrBusinessActionOutcomeLimitExceeded) {
		t.Fatalf("101st business action was not rejected before dispatch: %v", err)
	}
	events, err = fresh.ListBusinessActionEvents(ctx, "workspace-1", limitedRunID)
	if err != nil || len(events) != 2*MaxBusinessActionOutcomesPerRun {
		t.Fatalf("action outcome receipts were not retained: events=%d err=%v", len(events), err)
	}
	outcomes, err = ProjectBusinessActionOutcomes(events)
	if err != nil || len(outcomes) != MaxBusinessActionOutcomesPerRun {
		t.Fatalf("action outcome limit was not enforced: outcomes=%d err=%v", len(outcomes), err)
	}
}

func TestPGActionOutcomeEventRejectsNonObjectDetailRealPG(t *testing.T) {
	h := newProcessNextHarness(t)
	_, _ = h.seedRunningWorkflowTaskBeforeAdmission(t, "action-outcome-invalid")
	ctx := context.Background()
	store := &PGActivityStore{Transactions: h.pool}
	event := ActivityEvent{
		WorkspaceID: "workspace-1", RunID: "action-outcome-invalid", EventID: uuid.NewString(),
		Kind: "business_action_started", Detail: json.RawMessage(`[]`), OccurredAt: time.Now().UTC(),
	}
	if err := store.RecordBusinessActionEvent(ctx, event); err == nil {
		t.Fatal("accepted non-object activity detail")
	}
}

func TestPGConcurrentActionStartsReserveOneUnresolvedWriteRealPG(t *testing.T) {
	h := newProcessNextHarness(t)
	runID := "action-outcome-race"
	_, _ = h.seedRunningWorkflowTaskBeforeAdmission(t, runID)
	ctx := context.Background()
	store := &PGActivityStore{Transactions: h.pool}
	type precheck struct {
		callID  string
		blocked bool
		err     error
	}
	type writeResult struct {
		callID string
		err    error
	}
	prechecked := make(chan precheck, 2)
	writeResults := make(chan writeResult, 2)
	allowWrite := make(chan struct{})
	var dispatched atomic.Int32
	callIDs := []string{"concurrent-call-a", "concurrent-call-b"}
	var workers sync.WaitGroup
	for _, callID := range callIDs {
		callID := callID
		workers.Add(1)
		go func() {
			defer workers.Done()
			_, blocked, err := store.CheckBusinessActionReplay(ctx, "workspace-1", runID,
				"lead", "snapshot/0/lead", callID, "revision-1",
				"forge:action:sales_contract.ContractSubmit", "private-record-id")
			prechecked <- precheck{callID: callID, blocked: blocked, err: err}
			<-allowWrite
			event := actionActivityEvent("business_action_started", "lead", "lead-agent", "started",
				"snapshot/0/lead", callID, "ContractSubmit", "提交指定合同版本", "")
			event.WorkspaceID, event.RunID, event.EventID, event.OccurredAt = "workspace-1", runID, uuid.NewString(), time.Now().UTC()
			err = store.RecordBusinessActionEvent(ctx, event)
			if err == nil {
				dispatched.Add(1)
			}
			writeResults <- writeResult{callID: callID, err: err}
		}()
	}
	prechecksValid := true
	for range callIDs {
		result := <-prechecked
		if result.err != nil || result.blocked {
			prechecksValid = false
		}
	}
	close(allowWrite)
	if !prechecksValid {
		t.Fatal("precheck unexpectedly blocked before either reservation")
	}
	created, unresolved := 0, 0
	for range callIDs {
		result := <-writeResults
		switch {
		case result.err == nil:
			created++
		case errors.Is(result.err, businessaction.ErrActionOutcomeUnresolved):
			unresolved++
		default:
			t.Fatalf("unexpected start reservation failure for %s: %v", result.callID, result.err)
		}
	}
	workers.Wait()
	if created != 1 || unresolved != 1 || dispatched.Load() != 1 {
		t.Fatalf("concurrent writes were not serialized: created=%d unresolved=%d dispatched=%d", created, unresolved, dispatched.Load())
	}
	events, err := store.ListBusinessActionEvents(ctx, "workspace-1", runID)
	if err != nil || len(events) != 1 {
		t.Fatalf("expected one durable started receipt: events=%+v err=%v", events, err)
	}
	outcomes, err := ProjectBusinessActionOutcomes(events)
	if err != nil || len(outcomes) != 1 || outcomes[0].Status != "unknown" {
		t.Fatalf("reserved write was not exposed as unknown: outcomes=%+v err=%v", outcomes, err)
	}
}
