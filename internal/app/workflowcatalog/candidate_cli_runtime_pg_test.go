package workflowcatalog_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/app/agentcatalog"
	"github.com/jinyitao123/weave/internal/app/teamconstruction"
	"github.com/jinyitao123/weave/internal/app/workflowcatalog"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/delivery"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/jinyitao123/weave/internal/kernel/schedule"
	"github.com/jinyitao123/weave/internal/kernel/skills"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

func TestCandidateBuildsForCLIRuntimeAfterMemberConfigurationChangesRealPG(t *testing.T) {
	pool := testutil.PostgresPool(t)
	ctx := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "cli-candidate", UserID: "developer"})
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES('cli-candidate','cli-candidate','CLI Candidate'); INSERT INTO weave_users(id,tenant_id,username,password,role) VALUES('developer','cli-candidate','developer','unused','developer')`); err != nil {
		t.Fatal(err)
	}

	runtimeStore := runtimes.NewStore(pool)
	runtime, _, err := runtimeStore.Create(ctx, "cli-candidate", "Local runtime")
	if err != nil {
		t.Fatal(err)
	}
	if err := runtimeStore.Hello(ctx, "cli-candidate", runtime.ID, []string{"codex"}); err != nil {
		t.Fatal(err)
	}

	agents := agentcatalog.New(pool)
	lead := &registry.AgentRecord{Name: "lead", DisplayName: "Lead", Role: "avatar", Engine: "codex", RuntimeID: runtime.ID, GraphType: "standard", Spec: stdlib.AgentSpec{SystemPrompt: "Coordinate the work.", Skills: []stdlib.SkillDef{{Name: "Review", Body: "Review the result.", AlwaysActive: true}}}}
	worker := &registry.AgentRecord{Name: "worker", DisplayName: "Worker", Role: "worker", Engine: "codex", RuntimeID: runtime.ID, GraphType: "standard", Spec: stdlib.AgentSpec{SystemPrompt: "Do the work."}}
	for _, agent := range []*registry.AgentRecord{lead, worker} {
		if err := agents.Put(ctx, "cli-candidate", agent); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_teams(id,workspace_id,name,lead_avatar_id,status) VALUES('team','cli-candidate','Team',$1,'active')`, lead.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := agentcatalog.NewTeamWorkerRepository(pool).Create(ctx, "cli-candidate", registry.TeamWorker{TeamID: "team", WorkerAgentID: worker.ID, Duty: "Execute", AllowedKinds: []string{"consult", "dispatch", "handoff"}, DefaultKind: "dispatch", ResultRequirement: "Return evidence", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	graph, err := json.Marshal(map[string]any{
		"schema_version": 1, "entry_node_id": "lead", "input_contract": map[string]any{"type": "text"}, "output_contract": map[string]any{"type": "text"},
		"nodes": []any{
			map[string]any{"id": "lead", "type": "lead", "config": map[string]any{"instruction": "Coordinate"}, "inputs": map[string]any{"task": map[string]any{"value": map[string]any{"source": "run_input", "path": ""}, "expected_type": "text"}}, "output": map[string]any{"type": "text"}},
			map[string]any{"id": "worker", "type": "worker", "config": map[string]any{"kind": "dispatch", "agent_id": worker.ID, "agent_version": worker.Version, "result_requirement": "Return evidence"}, "inputs": map[string]any{"task": map[string]any{"value": map[string]any{"source": "node_output", "node_id": "lead", "path": ""}, "expected_type": "text"}}, "output": map[string]any{"type": "text"}},
			map[string]any{"id": "deliver", "type": "deliver", "config": map[string]any{"result": map[string]any{"source": "node_output", "node_id": "worker", "path": ""}}},
		},
		"edges": []any{map[string]any{"id": "lead-worker", "from_node_id": "lead", "to_node_id": "worker", "route": "success"}, map[string]any{"id": "worker-deliver", "from_node_id": "worker", "to_node_id": "deliver", "route": "success"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	flows := workflowcatalog.New(pool, nil, workflow.NewArtifactStore(pool, nil))
	draft, err := flows.Create(ctx, &workflow.TeamWorkflow{ID: "flow", WorkspaceID: "cli-candidate", TeamID: "team", Name: "Flow"}, workflow.DraftInput{CreatedBy: "developer", TriggerConfig: json.RawMessage(`{"schema_version":1,"type":"conversation_explicit","config":{}}`), GraphDefinition: graph})
	if err != nil {
		t.Fatal(err)
	}
	descriptors := compiler.NewDescriptorRegistry()
	for _, descriptor := range []compiler.GraphFactoryDescriptor{compiler.NewStandardFrozenDescriptor(), compiler.NewStandardFrozenToolsDescriptor(), compiler.NewStandardFrozenCLIToolsDescriptor()} {
		if err := descriptors.Register(descriptor); err != nil {
			t.Fatal(err)
		}
	}
	key := []byte(strings.Repeat("k", 32))
	builder := workflowcatalog.NewCandidateBuilder(flows, agents, delivery.New(pool, key), skills.New(pool), credentials.New(pool, key), schedule.New(pool, nil), descriptors)
	authority := teamconstruction.NewPublicationAuthority(pool, builder)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	candidate, report, err := authority.BuildCandidateTx(ctx, tx, workflow.CandidateInput{WorkspaceID: "cli-candidate", WorkflowID: "flow", WorkflowVersion: draft.Version})
	if err != nil {
		t.Fatal(err)
	}
	if candidate == nil || report == nil {
		t.Fatalf("candidate=%v report=%+v", candidate != nil, report)
	}
	var leadSkills []string
	for _, bundle := range candidate.Payload.Bundles {
		if bundle.Agent.AgentID != lead.ID {
			continue
		}
		for _, skill := range bundle.Skills {
			leadSkills = append(leadSkills, skill.Name+":"+skill.Body)
		}
	}
	if len(leadSkills) != 1 || leadSkills[0] != "Review:Review the result." {
		t.Fatalf("frozen lead skills=%v", leadSkills)
	}
}
