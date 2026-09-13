package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/app/agentcatalog"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/delivery"
	"github.com/jinyitao123/weave/internal/kernel/freezer"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/schedule"
	"github.com/jinyitao123/weave/internal/kernel/skills"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

func publishMemberSample(ctx context.Context, pool *pgxpool.Pool, key []byte, workspace, serverURL, serverID string, revision int64, toolName, prompt string) (frozen.FrozenExecutionBundle, compiler.FrozenResolver, *workflow.PublishedArtifactContent) {
	providers := credentials.New(pool, key)
	if err := providers.Upsert(ctx, workspace, llmrouter.ProviderConfig{ID: "fixture", Name: "Loom acceptance inference gateway", BaseURL: serverURL, APIKey: "isolated-gateway-credential", Models: []string{"acceptance-model"}}); err != nil {
		must(err)
	}
	agents := agentcatalog.New(pool)
	lead := &registry.AgentRecord{Name: "lead", Role: "avatar", Engine: "loom", Model: "acceptance-model", GraphType: "standard", Spec: stdlib.AgentSpec{SystemPrompt: "Return one short work brief. The engineering worker receives the original task and files. Do not perform its calculations or invent results."}}
	worker := &registry.AgentRecord{Name: "worker", Role: "worker", Engine: "loom", Model: "acceptance-model", GraphType: "standard", Spec: stdlib.AgentSpec{SystemPrompt: prompt}, MCPServers: []registry.MCPServerConfig{{ServerID: serverID, Filter: []string{toolName}}}}
	for _, record := range []*registry.AgentRecord{lead, worker} {
		if err := agents.Put(ctx, workspace, record); err != nil {
			must(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_teams(id,workspace_id,name,lead_avatar_id,status) VALUES('team',$1,'日冕工程成员恢复验收',$2,'active')`, workspace, lead.ID); err != nil {
		must(err)
	}
	if _, err := agentcatalog.NewTeamWorkerRepository(pool).Create(ctx, workspace, registry.TeamWorker{TeamID: "team", WorkerAgentID: worker.ID, Duty: "Compute", AllowedKinds: []string{"consult"}, DefaultKind: "consult", ResultRequirement: prompt, Enabled: true}); err != nil {
		must(err)
	}
	graph := json.RawMessage(fmt.Sprintf(`{"schema_version":1,"entry_node_id":"brief","input_contract":{"type":"text"},"output_contract":{"type":"text"},"nodes":[{"id":"brief","type":"lead","config":{"instruction":"Brief"},"inputs":{"task":{"value":{"source":"run_input","path":""},"expected_type":"text"}},"output":{"type":"text"}},{"id":"compute","type":"worker","config":{"kind":"consult","agent_id":%q,"agent_version":%d,"result_requirement":"Execute the configured tool task"},"inputs":{"task":{"value":{"source":"run_input","path":""},"expected_type":"text"}},"output":{"type":"text"}},{"id":"deliver","type":"deliver","config":{"result":{"source":"node_output","node_id":"compute","path":""}}}],"edges":[{"id":"a","from_node_id":"brief","to_node_id":"compute","route":"success"},{"id":"b","from_node_id":"compute","to_node_id":"deliver","route":"success"}]}`, worker.ID, worker.Version))
	store := workflow.New(pool, nil)
	draft, err := store.Create(ctx, &workflow.TeamWorkflow{ID: "flow", WorkspaceID: workspace, TeamID: "team", Name: "日冕工程成员恢复验收"}, workflow.DraftInput{CreatedBy: "test", TriggerConfig: json.RawMessage(`{"schema_version":1,"type":"conversation_explicit","config":{}}`), GraphDefinition: graph})
	if err != nil {
		must(err)
	}
	descriptors := descriptors()
	builder := workflow.NewCandidateBuilder(store, agents, delivery.New(pool, key), skills.New(pool), providers, schedule.New(pool, nil), descriptors)
	tx, err := pool.Begin(ctx)
	if err != nil {
		must(err)
	}
	defer tx.Rollback(context.Background())
	candidate, report, err := builder.BuildTx(ctx, tx, workflow.CandidateInput{WorkspaceID: workspace, WorkflowID: "flow", WorkflowVersion: draft.Version})
	if err != nil || candidate == nil || report != nil && len(report.Issues) > 0 {
		panic(fmt.Sprintf("build publication: err=%+v report=%+v", err, report))
	}
	publication, err := workflow.PublicationFromCandidate(candidate)
	if err != nil {
		must(err)
	}
	if err := store.InsertPublicationTx(ctx, tx, publication); err != nil {
		must(err)
	}
	if err := tx.Commit(ctx); err != nil {
		must(err)
	}
	saved, err := store.GetArtifact(ctx, workspace, "flow", draft.Version)
	if err != nil {
		must(err)
	}
	if strings.Contains(string(saved.Payload), "isolated-gateway-credential") {
		panic("credential leaked into publication")
	}
	payload, err := frozen.DecodeArtifactEnvelopeV1(frozen.ArtifactEnvelopeV1{
		WorkspaceID: saved.WorkspaceID, WorkflowID: saved.WorkflowID, WorkflowVersion: saved.WorkflowVersion,
		ArtifactSchemaVersion: saved.ArtifactSchemaVersion, CanonicalizationAlgorithm: saved.CanonicalizationAlgorithm,
		CanonicalizationVersion: saved.CanonicalizationVersion, HashAlgorithm: saved.HashAlgorithm, ContentHash: saved.ContentHash, Payload: saved.Payload,
	})
	if err != nil {
		must(err)
	}
	var bundle frozen.FrozenExecutionBundle
	for _, entry := range payload.Bundles {
		if entry.Agent.AgentID == worker.ID {
			bundle = entry
		} else if entry.FactoryKey.FactoryVersion != "1" {
			panic("lead unexpectedly upgraded")
		}
	}
	if bundle.FactoryKey != compiler.StandardFrozenToolsKey() || len(bundle.MCPBindings) != 1 || len(bundle.MCPBindings[0].Tools) != 1 {
		panic(fmt.Sprintf("tools missing: %#v", bundle.FactoryKey))
	}
	if bundle.MCPBindings[0].ServerRevision != revision {
		panic("revision not pinned")
	}
	resolver, err := freezer.RebuildArtifactResolver(bundle)
	if err != nil {
		agentHash, _ := frozen.HashDTO(bundle.Agent, frozen.PreorderFrozenAgentRecord)
		mcpHash, _ := frozen.HashDTO(bundle.MCPBindings[0], frozen.PreorderFrozenMCPBinding)
		modelHash, _ := frozen.HashDTO(bundle.PrimaryModel, frozen.PreorderFrozenModelBinding)
		fmt.Printf("agent hash=%s; mcp hash=%s stored=%s; model hash=%s stored=%s; dependencies=%+v", agentHash, mcpHash, bundle.MCPBindings[0].ContentHash, modelHash, bundle.PrimaryModel.ContentHash, bundle.Dependencies)
		must(err)
	}
	return bundle, resolver, saved
}
