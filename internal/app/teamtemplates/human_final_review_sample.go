package teamtemplates

import (
	"encoding/json"

	"github.com/jinyitao123/weave/internal/build/teamforge"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

func humanFinalReviewDeclarativeSpec() *teamforge.DeclarativeWorkflowSpecV1 {
	return &teamforge.DeclarativeWorkflowSpecV1{
		SchemaVersion: 1,
		EntryNodeID:   "draft",
		InputContract: machine.OutputContract{Type: machine.ValueText},
		OutputContract: machine.OutputContract{
			Type: machine.ValueJSON,
			Schema: json.RawMessage(`{
				"type":"object",
				"properties":{
					"decision":{"type":"string","enum":["approve","reject"]},
					"comments":{"type":"string"}
				},
				"required":["decision","comments"],
				"additionalProperties":false
			}`),
		},
		Nodes: []teamforge.DeclarativeWorkflowNodeV1{
			{
				ID: "draft", Type: machine.NodeTransform, Label: "接收待审交付稿",
				Output: &machine.OutputContract{Type: machine.ValueText},
				Config: json.RawMessage(`{"operation":"identity","value":{"source":"run_input"}}`),
			},
			{
				ID: "human-final-review", Type: machine.NodeWait, Label: "人工终审",
				Config: json.RawMessage(`{
					"kind":"human",
					"resume_schema":{
						"type":"object",
						"properties":{
							"decision":{"type":"string","enum":["approve","reject"]},
							"comments":{"type":"string"}
						},
						"required":["decision","comments"],
						"additionalProperties":false
					},
					"task":{
						"title":"终审交付稿",
						"instructions":"查看待审交付稿，选择通过或驳回，并填写终审批注。",
						"audience_ref":"workspace-member"
					}
				}`),
			},
			{
				ID: "deliver", Type: machine.NodeDeliver, Label: "记录终审决定并交付",
				Config: json.RawMessage(`{"result":{"source":"node_output","node_id":"human-final-review"}}`),
			},
		},
		Edges: []teamforge.DeclarativeWorkflowEdgeV1{
			{From: "draft", To: "human-final-review", Route: machine.RouteSuccess},
			{From: "human-final-review", To: "deliver", Route: machine.RouteSuccess},
		},
	}
}
