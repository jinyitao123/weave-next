package teamrun

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

// approvalGraph is the shape a team draft's approval compiles to: the work,
// a person's decision, and a condition that lets only "approve" go on.
var approvalGraph = json.RawMessage(`{
	"schema_version":1,"entry_node_id":"work",
	"input_contract":{"type":"text"},"output_contract":{"type":"text"},
	"nodes":[
		{"id":"work","type":"worker","label":"核对条款","config":{"kind":"consult","agent_id":"worker","agent_version":1,"result_requirement":"核对"},"inputs":{"original":{"value":{"source":"run_input","path":""},"expected_type":"text"}},"output":{"type":"text"}},
		{"id":"approval","type":"wait","label":"法务审批","config":{"kind":"human","task":{"title":"法务审批","instructions":"核对结论"},
			"resume_schema":{"type":"object","additionalProperties":false,"required":["decision","comment"],"properties":{"decision":{"type":"string","enum":["approve","reject"]},"comment":{"type":"string"}}}}},
		{"id":"check","type":"condition","label":"法务审批：是否批准","config":{}},
		{"id":"deliver","type":"deliver","label":"交付结果","config":{"result":{"source":"node_output","node_id":"work","path":""}}},
		{"id":"rejected","type":"deliver","label":"法务审批：驳回意见","config":{"result":{"source":"node_output","node_id":"approval","path":"/comment"}}}
	],
	"edges":[
		{"id":"a","from_node_id":"work","to_node_id":"approval","route":"success"},
		{"id":"b","from_node_id":"approval","to_node_id":"check","route":"success"},
		{"id":"c","from_node_id":"check","to_node_id":"deliver","route":"case","priority":1,
			"predicate":{"left":{"source":"node_output","node_id":"approval","path":"/decision"},"operator":"eq","right":{"source":"literal","value":"approve"}}},
		{"id":"d","from_node_id":"check","to_node_id":"rejected","route":"default"}
	]
}`)

func TestApprovalDecisionChoosesWhatTheRunDelivers(t *testing.T) {
	graph, report := machine.DecodeGraphDefinitionV1(approvalGraph)
	if report != nil && len(report.Issues) > 0 {
		t.Fatalf("decode: %+v", report.Issues)
	}
	for decision, want := range map[string]string{"approve": "条款核对完毕", "reject": "违约金条款需要重谈"} {
		var delivered string
		// A human resume stores the decision as the wait's output and continues
		// at the wait's success target.
		result := runSerialMachine(context.Background(), graph, frozen.ArtifactPayloadV1{}, &workflow.RuntimeArtifact{}, "合同原文", serialMachineStart{
			NodeID: "check",
			Outputs: map[string]any{
				"work":     "条款核对完毕",
				"approval": map[string]any{"decision": decision, "comment": "违约金条款需要重谈"},
			},
			Run: TeamRun{WorkspaceID: "ws", ProjectID: "project", RunID: "run-" + decision, RunSnapshotID: "snapshot", TeamID: "team", WorkflowID: "flow", WorkflowVersion: 1},
			RecordOutput: func(_ context.Context, node machine.Node, output any, final bool) error {
				if final {
					delivered, _ = output.(string)
				}
				return nil
			},
		})
		if result.Status != serialCompleted || result.Err != nil || delivered != want {
			t.Fatalf("%s: status=%v err=%v delivered=%q, want %q", decision, result.Status, result.Err, delivered, want)
		}
	}
}
