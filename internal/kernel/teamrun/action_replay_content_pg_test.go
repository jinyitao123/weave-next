package teamrun

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

const (
	replayCapability = "forge:action:sales_contract.ContractSubmit"
	replayRecord     = "private-record-id"
	digestA          = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB          = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// recordDigestedAction stores a start receipt carrying paramsSHA256 and, when
// status is not empty, its result receipt, exactly as the dispatcher would.
func recordDigestedAction(t *testing.T, store *PGActivityStore, runID, callID, paramsSHA256, status string) {
	t.Helper()
	withDigest := func(event ActivityEvent) ActivityEvent {
		var detail map[string]any
		if err := json.Unmarshal(event.Detail, &detail); err != nil {
			t.Fatal(err)
		}
		if paramsSHA256 != "" {
			detail["params_sha256"] = paramsSHA256
		}
		encoded, err := json.Marshal(detail)
		if err != nil {
			t.Fatal(err)
		}
		event.Detail = encoded
		event.WorkspaceID, event.RunID, event.EventID, event.OccurredAt = "workspace-1", runID, uuid.NewString(), time.Now().UTC()
		return event
	}
	recordActionActivity(t, store, withDigest(actionActivityEvent("business_action_started", "lead", "lead-agent",
		"started", "snapshot/0/lead", callID, "ContractSubmit", "提交指定合同版本", "")))
	if status != "" {
		recordActionActivity(t, store, withDigest(actionActivityEvent("business_action_result", "lead", "lead-agent",
			"result", "snapshot/0/lead", callID, "ContractSubmit", "提交指定合同版本", status)))
	}
}

func replayDecision(t *testing.T, store *PGActivityStore, runID, callID, recordID, paramsSHA256 string) BusinessActionReplayDecision {
	t.Helper()
	decision, err := store.CheckBusinessActionReplay(context.Background(), BusinessActionReplayCheck{
		WorkspaceID: "workspace-1", RunID: runID, NodeID: "lead", InvocationID: "snapshot/0/lead", CallID: callID,
		InputRevisionID: "revision-1", CapabilityID: replayCapability, RecordID: recordID, ParamsSHA256: paramsSHA256,
	})
	if err != nil {
		t.Fatal(err)
	}
	return decision
}

// A member that restarts re-issues the same write under a new model call ID.
// The earlier success is identified by content, so Forge is not called again.
func TestPGSameParamsSuccessBlocksReplayUnderNewCallID(t *testing.T) {
	h := newProcessNextHarness(t)
	runID := "replay-same-params"
	_, _ = h.seedRunningWorkflowTaskBeforeAdmission(t, runID)
	store := &PGActivityStore{Transactions: h.pool, ContentReplayEnabled: true}
	recordDigestedAction(t, store, runID, "call-original", digestA, "succeeded")

	decision := replayDecision(t, store, runID, "call-after-restart", replayRecord, digestA)
	if !decision.Blocked || decision.Status != "succeeded" || !decision.SameParams {
		t.Fatalf("identical write under a new call ID was not blocked as a same-params success: %+v", decision)
	}
}

// Two different price adjustments on the same quotation in one run are both
// legitimate; only an identical request is a replay.
func TestPGDifferentParamsAreNotBlockedByAnEarlierSuccess(t *testing.T) {
	h := newProcessNextHarness(t)
	runID := "replay-different-params"
	_, _ = h.seedRunningWorkflowTaskBeforeAdmission(t, runID)
	store := &PGActivityStore{Transactions: h.pool, ContentReplayEnabled: true}
	recordDigestedAction(t, store, runID, "call-first-line", digestA, "succeeded")

	if decision := replayDecision(t, store, runID, "call-second-line", replayRecord, digestB); decision.Blocked {
		t.Fatalf("a different request was blocked by an earlier success: %+v", decision)
	}
}

// A failed write may be retried with the same params: nothing was applied.
func TestPGSameParamsAfterFailureMayBeRetried(t *testing.T) {
	h := newProcessNextHarness(t)
	runID := "replay-retry-after-failure"
	_, _ = h.seedRunningWorkflowTaskBeforeAdmission(t, runID)
	store := &PGActivityStore{Transactions: h.pool, ContentReplayEnabled: true}
	recordDigestedAction(t, store, runID, "call-failed", digestA, "failed")

	if decision := replayDecision(t, store, runID, "call-retry", replayRecord, digestA); decision.Blocked {
		t.Fatalf("a failed write was not allowed to retry with the same params: %+v", decision)
	}
}

// An unresolved earlier start stays authoritative: the member is told the
// outcome is unknown, never that an identical write succeeded.
func TestPGUnresolvedStartOutranksSameParamsSuccess(t *testing.T) {
	h := newProcessNextHarness(t)
	runID := "replay-unresolved-first"
	_, _ = h.seedRunningWorkflowTaskBeforeAdmission(t, runID)
	store := &PGActivityStore{Transactions: h.pool, ContentReplayEnabled: true}
	recordDigestedAction(t, store, runID, "call-succeeded", digestA, "succeeded")
	recordDigestedAction(t, store, runID, "call-unknown", digestB, "")

	decision := replayDecision(t, store, runID, "call-next", replayRecord, digestA)
	if !decision.Blocked || decision.Status != "unknown" || decision.SameParams {
		t.Fatalf("an unresolved write did not outrank a same-params success: %+v", decision)
	}
}

// Without a digest the rule is off, and receipts written before digests
// existed can never match one.
func TestPGSameParamsRuleNeedsADigestOnBothSides(t *testing.T) {
	h := newProcessNextHarness(t)
	runID := "replay-digest-compat"
	_, _ = h.seedRunningWorkflowTaskBeforeAdmission(t, runID)
	store := &PGActivityStore{Transactions: h.pool, ContentReplayEnabled: true}
	recordDigestedAction(t, store, runID, "call-with-digest", digestA, "succeeded")
	recordDigestedAction(t, store, runID, "call-legacy", "", "succeeded")

	if decision := replayDecision(t, store, runID, "call-new", replayRecord, ""); decision.Blocked {
		t.Fatalf("an empty digest must not enable the same-params rule: %+v", decision)
	}
	if decision := replayDecision(t, store, runID, "call-new", replayRecord, digestB); decision.Blocked {
		t.Fatalf("a receipt without a digest matched a digest: %+v", decision)
	}
}

// The match is scoped to one capability, one record and one run.
func TestPGSameParamsMatchDoesNotCrossRecordOrRun(t *testing.T) {
	h := newProcessNextHarness(t)
	runID, otherRunID := "replay-scope-a", "replay-scope-b"
	_, _ = h.seedRunningWorkflowTaskBeforeAdmission(t, runID)
	_, _ = h.seedRunningWorkflowTaskBeforeAdmission(t, otherRunID)
	store := &PGActivityStore{Transactions: h.pool, ContentReplayEnabled: true}
	recordDigestedAction(t, store, runID, "call-original", digestA, "succeeded")

	if decision := replayDecision(t, store, runID, "call-other-record", "another-record", digestA); decision.Blocked {
		t.Fatalf("the same-params rule crossed the record boundary: %+v", decision)
	}
	if decision := replayDecision(t, store, otherRunID, "call-other-run", replayRecord, digestA); decision.Blocked {
		t.Fatalf("the same-params rule crossed the run boundary: %+v", decision)
	}
}

func TestPGContentReplayRequiresExplicitEnablement(t *testing.T) {
	h := newProcessNextHarness(t)
	runID := "replay-not-enabled"
	_, _ = h.seedRunningWorkflowTaskBeforeAdmission(t, runID)
	store := &PGActivityStore{Transactions: h.pool}
	recordDigestedAction(t, store, runID, "call-original", digestA, "succeeded")
	decision := replayDecision(t, store, runID, "call-intentional-repeat", replayRecord, digestA)
	if decision.Blocked {
		t.Fatalf("content-only protection must not mistake an intentional repeat for recovery: %+v", decision)
	}
}
