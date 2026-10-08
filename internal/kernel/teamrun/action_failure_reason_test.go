package teamrun

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBusinessActionFailureReasonFeedsCardAndTerminalWithoutChangingOutcome(t *testing.T) {
	for _, test := range []struct {
		status, reason string
		visible        bool
	}{
		{"failed", "请先维护项目客户分类", true},
		{"failed", "", false},
		{"failed", "凭据 Bearer private-token", false},
		{"unknown", "请先维护项目客户分类", false},
		{"succeeded", "请先维护项目客户分类", false},
	} {
		t.Run(test.status+test.reason, func(t *testing.T) {
			started := actionActivityEvent("business_action_started", "lead", "member", "started", "snapshot/0/lead", "call", "Convert", "转为商机", "")
			result := actionActivityEvent("business_action_result", "lead", "member", "result", "snapshot/0/lead", "call", "Convert", "转为商机", test.status)
			started.Seq, result.Seq = 1, 2
			var detail map[string]any
			_ = json.Unmarshal(result.Detail, &detail)
			detail["public_reason"] = test.reason
			result.Detail, _ = json.Marshal(detail)
			events := []ActivityEvent{started, result}
			outcomes, err := ProjectBusinessActionOutcomes(events)
			if err != nil || len(outcomes) != 1 || outcomes[0].Status != test.status {
				t.Fatalf("status changed: %+v %v", outcomes, err)
			}
			counts := CountBusinessActions(events)
			for _, summary := range []string{outcomes[0].Summary, counts.Summary} {
				if strings.Contains(summary, "请先维护项目客户分类") != test.visible || strings.Contains(summary, "private-token") {
					t.Fatalf("unsafe or absent public reason: %q", summary)
				}
			}
			if counts.Total != 1 || (test.status == "failed" && counts.Failed != 1) || (test.status == "unknown" && counts.Unknown != 1) || (test.status == "succeeded" && counts.Succeeded != 1) {
				t.Fatalf("reason changed counts: %+v", counts)
			}
		})
	}
}
