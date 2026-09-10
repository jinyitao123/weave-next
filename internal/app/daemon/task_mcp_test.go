package daemon

import (
	"testing"

	"github.com/jinyitao123/weave/internal/base/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/execenv"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
)

func TestTaskMCPConfigRejectsUnboundOrCrossTaskTargets(t *testing.T) {
	task := &taskqueue.Task{ID: "task", ClaimEpoch: 2}
	payload := runtimes.EngineExecRequest{BoundMCP: true, Record: &registry.AgentRecord{}, TaskMCP: []execenv.TaskMCPTarget{{URL: "/v1/runtime/tasks/task/mcp/0", Token: "tmcp1.opaque.signature"}}}
	targets, err := runtimeTaskMCPTargets("https://server.example/", task, payload)
	if err != nil || len(targets) != 1 || targets[0].URL != "https://server.example/v1/runtime/tasks/task/mcp/0" {
		t.Fatalf("targets=%v err=%v", targets, err)
	}
	for _, mutate := range []func(*runtimes.EngineExecRequest){
		func(p *runtimes.EngineExecRequest) { p.BoundMCP = false },
		func(p *runtimes.EngineExecRequest) { p.TaskMCP = nil },
		func(p *runtimes.EngineExecRequest) {
			p.TaskMCP = []execenv.TaskMCPTarget{{URL: "/v1/runtime/tasks/other/mcp/0", Token: "tmcp1.opaque.signature"}}
		},
		func(p *runtimes.EngineExecRequest) {
			p.TaskMCP = []execenv.TaskMCPTarget{{URL: "https://upstream.example/tool", Token: "tmcp1.opaque.signature"}}
		},
		func(p *runtimes.EngineExecRequest) {
			p.Record = &registry.AgentRecord{MCPServers: []registry.MCPServerConfig{{ServerID: "live"}}}
		},
		func(p *runtimes.EngineExecRequest) { p.FrozenMCP = &execenv.FrozenMCPInvocation{} },
	} {
		candidate := payload
		mutate(&candidate)
		if _, err := runtimeTaskMCPTargets("https://server.example", task, candidate); err == nil {
			t.Fatal("accepted invalid task authority")
		}
	}
}
