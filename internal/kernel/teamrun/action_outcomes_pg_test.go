package teamrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
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

	limitedRunID := "action-outcome-limit-run"
	_, _ = h.seedRunningWorkflowTaskBeforeAdmission(t, limitedRunID)
	for index := 0; index < MaxBusinessActionOutcomesPerRun; index++ {
		callID := fmt.Sprintf("limit-call-%03d", index)
		event := actionActivityEvent("business_action_started", "lead", "lead-agent", "started", "snapshot/0/lead", callID, "ContractSubmit", "提交指定合同版本", "")
		event.WorkspaceID, event.RunID, event.EventID = "workspace-1", limitedRunID, uuid.NewString()
		recordActionActivity(t, store, event)
	}
	overLimit := actionActivityEvent("business_action_started", "lead", "lead-agent", "started", "snapshot/0/lead", "limit-call-over", "ContractSubmit", "提交指定合同版本", "")
	overLimit.WorkspaceID, overLimit.RunID, overLimit.EventID = "workspace-1", limitedRunID, uuid.NewString()
	if err := store.RecordBusinessActionEvent(ctx, overLimit); !errors.Is(err, ErrBusinessActionOutcomeLimitExceeded) {
		t.Fatalf("101st business action was not rejected before dispatch: %v", err)
	}
	events, err = fresh.ListBusinessActionEvents(ctx, "workspace-1", limitedRunID)
	if err != nil || len(events) != MaxBusinessActionOutcomesPerRun {
		t.Fatalf("action outcome limit was not enforced: events=%d err=%v", len(events), err)
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
