package teamrun

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
)

// A step's totals cover every round of the run, and its models are the ones
// its calls were sent to, not the label the caller gave them (#115, #116).
func TestStageTotalsSumEveryRoundAndNameTheModelsSentRealPG(t *testing.T) {
	h := newProcessNextHarness(t)
	_, run := h.seedRunningWorkflowTaskBeforeAdmission(t, "stage-totals")
	store := &PGActivityStore{Transactions: h.pool}
	record := func(node, member, kind, detail string) {
		t.Helper()
		if err := store.Record(t.Context(), ActivityEvent{WorkspaceID: run.WorkspaceID, RunID: run.RunID, EventID: uuid.NewString(),
			Kind: kind, NodeID: node, MemberID: member, MemberVersion: 1, Detail: json.RawMessage(detail), OccurredAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	for round := 0; round < 2; round++ {
		record("draft", "drafter", "member_started", `{}`)
		record("draft", "drafter", "model_completed", `{"model":"deepseek-flash","phase":"completed","request":{"model":"deepseek-v4-pro"}}`)
		record("draft", "drafter", "member_completed", `{"input_tokens":100,"output_tokens":20}`)
	}
	record("check", "checker", "member_started", `{}`)
	record("check", "checker", "model_completed", `{"model":"deepseek-flash","phase":"completed"}`)
	record("check", "checker", "member_failed", `{"input_tokens":7,"output_tokens":"not a number"}`)
	record("", "", "run_started", `{}`)

	totals, err := store.StageTotals(t.Context(), run.WorkspaceID, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	byNode := map[string]ActivityStageTotals{}
	for _, total := range totals {
		byNode[total.NodeID] = total
	}
	want := map[string]ActivityStageTotals{
		"draft": {NodeID: "draft", MemberID: "drafter", Rounds: 2, InputTokens: 200, OutputTokens: 40, Models: []string{"deepseek-v4-pro"}},
		"check": {NodeID: "check", MemberID: "checker", Rounds: 1, InputTokens: 7, Models: []string{"deepseek-flash"}},
	}
	if !reflect.DeepEqual(byNode, want) {
		t.Fatalf("totals = %+v, want %+v", byNode, want)
	}
}
