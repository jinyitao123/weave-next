package teamrun

import (
	"testing"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

func TestFanoutLegStepUsageKeepsEveryConfirmedReceipt(t *testing.T) {
	report := nodeUsageReport{
		MemberReceipts: []loomruntime.ConfirmedUsageReceipt{
			{AttemptID: "a1", Usage: contract.Usage{InputTokens: 10, OutputTokens: 2}, Metadata: loomruntime.UsageAttemptMetadata{HasTokens: true, HasCost: true}},
			{AttemptID: "a2", Usage: contract.Usage{InputTokens: 7, OutputTokens: 1}, ToolCalls: 1, Metadata: loomruntime.UsageAttemptMetadata{HasTokens: true, HasCost: true}},
		},
		Coverage: loomruntime.UsageCoverage{HasTokens: true, HasCost: true},
	}
	usage := newFanoutLegStepUsage("group/fanout/leg", report)
	if len(usage.Attempts) != 2 || usage.IncompleteReason != "" {
		t.Fatalf("step usage = %+v, want both receipts and complete", usage)
	}
	again := newFanoutLegStepUsage("group/fanout/leg", report)
	if again.Attempts[0].AttemptID != usage.Attempts[0].AttemptID {
		t.Fatal("attempt identity is not stable for the same step")
	}
	other := newFanoutLegStepUsage("group/fanout/other-leg", report)
	if other.Attempts[0].AttemptID == usage.Attempts[0].AttemptID {
		t.Fatal("two legs share an attempt identity")
	}
}

func TestFanoutLegStepUsageNamesWhatIsUnknown(t *testing.T) {
	cases := map[string]struct {
		report nodeUsageReport
		want   string
	}{
		"receipt without cost": {nodeUsageReport{
			MemberReceipts: []loomruntime.ConfirmedUsageReceipt{{AttemptID: "a", Usage: contract.Usage{InputTokens: 3}, Metadata: loomruntime.UsageAttemptMetadata{HasTokens: true}}},
			Coverage:       loomruntime.UsageCoverage{HasTokens: true},
		}, UsageIncompleteReasonAttemptLost},
		"lost attempt": {nodeUsageReport{UsageIncompleteReason: UsageIncompleteReasonAttemptLost}, UsageIncompleteReasonAttemptLost},
		"CLI without receipt": {nodeUsageReport{CLIExecution: true, CLIAttempts: []workflow.RuntimeCLIUsageAttempt{{AttemptID: "c"}}}, UsageIncompleteReasonCLINode},
		"CLI without cost": {nodeUsageReport{CLIExecution: true, Coverage: loomruntime.UsageCoverage{HasTokens: true},
			CLIAttempts: []workflow.RuntimeCLIUsageAttempt{{AttemptID: "c", InputTokens: 4, HasTokens: true}}}, UsageIncompleteReasonCLIDimensions},
		"measured in process": {nodeUsageReport{Totals: loomruntime.UsageTotals{InputTokens: 5}, Coverage: loomruntime.UsageCoverage{HasTokens: true, HasCost: true}}, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			usage := newFanoutLegStepUsage("key", tc.report)
			if usage.IncompleteReason != tc.want || usage.Attempts == nil {
				t.Fatalf("step usage = %+v, want reason %q", usage, tc.want)
			}
			accumulator := loomruntime.NewUsageAccumulator()
			callID, err := accumulator.NextCall("run", "node")
			if err != nil {
				t.Fatal(err)
			}
			if err := confirmNodeUsageAttempts(&accumulator, callID, usage.Attempts); err != nil {
				t.Fatalf("stored attempts do not confirm: %v", err)
			}
		})
	}
}
