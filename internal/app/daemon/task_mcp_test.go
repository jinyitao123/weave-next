package daemon

import (
	"testing"

	"github.com/jinyitao123/weave/internal/kernel/runtimeprotocol"
)

func TestTaskMCPConfigRejectsCrossTaskTargets(t *testing.T) {
	task := &runtimeprotocol.ExecutionClaim{TaskID: "task", ClaimEpoch: 2}
	payload := runtimeprotocol.ExecutionRequest{TaskMCP: []runtimeprotocol.TaskMCPTarget{{URL: "/v1/runtime/tasks/task/mcp/0", Token: "tmcp1.opaque.signature"}}}
	targets, err := runtimeTaskMCPTargets("https://server.example/", task, payload)
	if err != nil || len(targets) != 1 || targets[0].URL != "https://server.example/v1/runtime/tasks/task/mcp/0" {
		t.Fatalf("targets=%v err=%v", targets, err)
	}
	for _, mutate := range []func(*runtimeprotocol.ExecutionRequest){
		func(p *runtimeprotocol.ExecutionRequest) {
			p.TaskMCP = []runtimeprotocol.TaskMCPTarget{{URL: "/v1/runtime/tasks/other/mcp/0", Token: "tmcp1.opaque.signature"}}
		},
		func(p *runtimeprotocol.ExecutionRequest) {
			p.TaskMCP = []runtimeprotocol.TaskMCPTarget{{URL: "https://upstream.example/tool", Token: "tmcp1.opaque.signature"}}
		},
		func(p *runtimeprotocol.ExecutionRequest) {
			p.TaskMCP = []runtimeprotocol.TaskMCPTarget{{URL: "/v1/runtime/tasks/task/mcp/0", Token: "wrong"}}
		},
	} {
		candidate := payload
		mutate(&candidate)
		if _, err := runtimeTaskMCPTargets("https://server.example", task, candidate); err == nil {
			t.Fatal("accepted invalid task authority")
		}
	}
}
