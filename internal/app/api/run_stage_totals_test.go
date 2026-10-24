package api

import (
	"reflect"
	"testing"

	"github.com/jinyitao123/weave/internal/kernel/teamrun"
)

// Whole-run totals land on the stage of the member and step they belong to.
func TestRunStageTotalsLandOnTheirOwnStage(t *testing.T) {
	members := []runActivityMember{
		{AgentID: "drafter", Stages: []runActivityMemberStage{{NodeID: "draft"}, {NodeID: "other"}}},
		{AgentID: "checker", Stages: []runActivityMemberStage{{NodeID: "draft"}}},
	}
	applyRunStageTotals(members, []teamrun.ActivityStageTotals{
		{NodeID: "draft", MemberID: "drafter", Rounds: 3, InputTokens: 8744, OutputTokens: 747, Models: []string{"deepseek-v4-pro"}},
		{NodeID: "missing", MemberID: "drafter", Rounds: 9},
	})
	draft := members[0].Stages[0]
	if draft.Rounds != 3 || draft.InputTokens != 8744 || draft.OutputTokens != 747 || !reflect.DeepEqual(draft.Models, []string{"deepseek-v4-pro"}) {
		t.Fatalf("draft = %+v", draft)
	}
	if members[0].Stages[1].Rounds != 0 || members[1].Stages[0].Rounds != 0 {
		t.Fatalf("totals leaked onto other stages: %+v", members)
	}
}
