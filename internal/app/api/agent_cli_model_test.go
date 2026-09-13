package api

import (
	"encoding/json"
	"testing"

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
