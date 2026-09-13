package api

import (
	"encoding/json"
	"testing"

	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

func TestAgentUpdateCanSelectHostCLIModelWithoutClearingOmittedModel(t *testing.T) {
	for _, tt := range []struct{ body, want string }{
		{`{"engine":"claude","model":""}`, ""},
		{`{"engine":"claude"}`, "old-model"},
		{`{"engine":"loom","model":""}`, "old-model"},
	} {
		var request agentWriteRequest
		if err := json.Unmarshal([]byte(tt.body), &request); err != nil {
			t.Fatal(err)
		}
		got := mergeAgentRecordWithPresence(&registry.AgentRecord{Engine: "codex", Model: "old-model"}, &request.AgentRecord,
			agentMergePresence{enginePresent: request.enginePresent, modelPresent: request.modelPresent})
		if got.Model != tt.want {
			t.Fatalf("%s: model=%q want=%q", tt.body, got.Model, tt.want)
		}
	}
}

func TestAgentUpdatePreservesExplicitLoomToolLoopControl(t *testing.T) {
	for _, body := range []string{`{"tool_loop_control":{"slice_rounds":1,"initial_total_rounds":1}}`, `{}`, `{"tool_loop_control":null}`} {
		var request agentWriteRequest
		if err := json.Unmarshal([]byte(body), &request); err != nil {
			t.Fatal(err)
		}
		existing := &registry.AgentRecord{Engine: "loom", ToolLoopControl: &frozen.ToolLoopControl{SliceRounds: 2, InitialTotalRounds: 3}}
		got := mergeAgentRecordWithPresence(existing, &request.AgentRecord, agentMergePresence{toolLoopControlPresent: request.toolLoopControlPresent})
		if body == `{}` {
			if got.ToolLoopControl == nil || got.ToolLoopControl.SliceRounds != 2 {
				t.Fatal("omitted control was cleared")
			}
		} else if request.ToolLoopControl == nil {
			if got.ToolLoopControl != nil {
				t.Fatal("explicit clearing ignored")
			}
		} else if got.ToolLoopControl == nil || got.ToolLoopControl.SliceRounds != 1 {
			t.Fatal("explicit durable control ignored")
		}
	}
}
