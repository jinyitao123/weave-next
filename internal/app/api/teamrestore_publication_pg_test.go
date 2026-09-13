package api

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jinyitao123/weave/internal/app/agentcatalog"
	"github.com/jinyitao123/weave/internal/app/teamconstruction"
	"github.com/jinyitao123/weave/internal/app/workflowcatalog"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/delivery"
	"github.com/jinyitao123/weave/internal/kernel/publicationservice"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/schedule"
	"github.com/jinyitao123/weave/internal/kernel/skills"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

func TestWorkflowRollbackPublicationReplaysFixedRequestRealPG(t *testing.T) {
	ctx := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "restore-ws", UserID: "operator"})
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES('restore-ws','restore-ws','Restore'); INSERT INTO weave_users(id,tenant_id,username,password,role) VALUES('operator','restore-ws','operator','unused','admin')`); err != nil {
		t.Fatal(err)
	}
	agents := agentcatalog.New(pool)
	lead := &registry.AgentRecord{Name: "restore-lead", Role: "avatar", GraphType: "standard"}
	if err := agents.Put(ctx, "restore-ws", lead); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_teams(id,workspace_id,name,lead_avatar_id,status) VALUES('restore-team','restore-ws','Restore Team',$1,'active')`, lead.ID); err != nil {
		t.Fatal(err)
	}
	flows := workflowcatalog.New(pool, nil, workflow.NewArtifactStore(pool, nil))
	trigger := json.RawMessage(`{"schema_version":1,"type":"conversation_explicit","config":{}}`)
	graph := json.RawMessage(`{"schema_version":1,"entry_node_id":"lead","input_contract":{"type":"text"},"output_contract":{"type":"text"},"nodes":[{"id":"lead","type":"lead","config":{"instruction":"Answer"},"inputs":{"task":{"value":{"source":"run_input","path":""},"expected_type":"text"}},"output":{"type":"text"}},{"id":"deliver","type":"deliver","config":{"result":{"source":"node_output","node_id":"lead","path":""}}}],"edges":[{"id":"done","from_node_id":"lead","to_node_id":"deliver","route":"success"}]}`)
	draft, err := flows.Create(ctx, &workflow.TeamWorkflow{WorkspaceID: "restore-ws", ID: "restore-flow", TeamID: "restore-team", Name: "Restore Flow"}, workflow.DraftInput{CreatedBy: "operator", TriggerConfig: trigger, GraphDefinition: graph})
	if err != nil {
		t.Fatal(err)
	}
	descriptors := compiler.NewDescriptorRegistry()
	if err = descriptors.Register(compiler.NewStandardFrozenDescriptor()); err != nil {
		t.Fatal(err)
	}
	key := []byte(strings.Repeat("r", 32))
	builder := workflowcatalog.NewCandidateBuilder(flows, agents, delivery.New(pool, key), skills.New(pool), credentials.New(pool, key), schedule.New(pool, nil), descriptors)
	authority := teamconstruction.NewPublicationAuthority(pool, builder)
	connection, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	query := connection.Query()
	query.Set("search_path", pool.Config().ConnConfig.RuntimeParams["search_path"])
	connection.RawQuery = query.Encode()
	kernelPublication, err := publicationservice.Open(ctx, connection.String(), authority)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(kernelPublication.Close)
	productPublication := teamconstruction.NewProductPublication(pool, kernelPublication, authority.AuthorizeProduct)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	candidate, report, err := authority.BuildCandidateTx(ctx, tx, workflow.CandidateInput{WorkspaceID: "restore-ws", WorkflowID: "restore-flow", WorkflowVersion: draft.Version})
	if err != nil || report == nil || len(report.Issues) != 0 {
		_ = tx.Rollback(ctx)
		t.Fatalf("build initial candidate err=%v report=%+v", err, report)
	}
	_ = tx.Rollback(ctx)
	command, err := teamconstruction.PublicationCommandForCandidate("restore-initial", candidate, teamconstruction.PublicationTarget{TeamID: "restore-team"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = productPublication.Publish(ctx, command); err != nil {
		t.Fatal(err)
	}

	restorer := workflowPublicationRestorer{pool: pool, workflows: flows, authority: authority, publications: productPublication}
	baselineGraph := json.RawMessage(strings.Replace(string(graph), "Answer", "Restore the frozen answer", 1))
	ref := teambuild.BaselineWorkflowRef{WorkflowID: "restore-flow", TeamID: "restore-team", Published: &teambuild.BaselineWorkflowPublished{
		Trigger: trigger, Graph: baselineGraph, ContentHash: strings.Repeat("a", 64),
	}}
	first, err := restorer.RestoreWorkflow(ctx, "restore-ws", "build-rollback", "restore-team", "operator", ref)
	if err != nil {
		t.Fatal(err)
	}
	second, err := restorer.RestoreWorkflow(ctx, "restore-ws", "build-rollback", "restore-team", "operator", ref)
	if err != nil {
		t.Fatal(err)
	}
	if first.Version != 2 || second.Version != first.Version || !first.Restored || !second.Restored {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
	var versions int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM weave_team_workflow_versions WHERE workspace_id='restore-ws' AND workflow_id='restore-flow'`).Scan(&versions); err != nil || versions != 2 {
		t.Fatalf("version count=%d err=%v", versions, err)
	}
}
