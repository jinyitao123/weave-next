package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/app/agentcatalog"
	"github.com/jinyitao123/weave/internal/app/teamconstruction"
	"github.com/jinyitao123/weave/internal/app/workflowcatalog"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/delivery"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/publicationservice"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/jinyitao123/weave/internal/kernel/schedule"
	"github.com/jinyitao123/weave/internal/kernel/skills"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/labstack/echo/v4"
)

func TestMemberExecutionPublishesNativeModelsAndRollsBackDraftConflictRealPG(t *testing.T) {
	ctx := context.Background()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	cfg := pool.Config()
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement
	productionPool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer productionPool.Close()
	pool = productionPool
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES('ws','ws','Workspace'); INSERT INTO weave_users(id,tenant_id,username,password,role) VALUES('fixture','ws','fixture','unused','admin')`); err != nil {
		t.Fatal(err)
	}
	ctx = execution.WithSubject(ctx, execution.Subject{WorkspaceID: "ws", UserID: "fixture"})
	agents := agentcatalog.New(pool)
	record := &registry.AgentRecord{Name: "lead", Role: "avatar", Engine: engine.Claude, GraphType: "standard"}
	if err := agents.Put(ctx, "ws", record); err != nil {
		t.Fatal(err)
	}
	runtimeStore := runtimes.NewStore(pool)
	runtime, _, err := runtimeStore.Create(ctx, "ws", "Native CLI")
	if err != nil {
		t.Fatal(err)
	}
	if err := runtimeStore.HelloWithCapabilities(ctx, "ws", runtime.ID, []string{engine.Claude}, []runtimes.EngineCapability{{Engine: engine.Claude, BinaryPath: "/fixture/claude", BinaryVersion: "fixture", AuthMode: runtimes.AuthModeOAuth, ProtocolVersion: "1", EndpointClass: "host_configured"}}, 1); err != nil {
		t.Fatal(err)
	}
	record.RuntimeID = runtime.ID
	if err := agents.Put(ctx, "ws", record); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_teams(id,workspace_id,name,lead_avatar_id,status) VALUES('team','ws','Team',$1,'active')`, record.ID); err != nil {
		t.Fatal(err)
	}
	store := workflowcatalog.New(pool, nil, workflow.NewArtifactStore(pool, nil))
	draft, err := store.Create(ctx, &workflow.TeamWorkflow{WorkspaceID: "ws", ID: "flow", TeamID: "team", Name: "Flow"}, workflow.DraftInput{CreatedBy: "fixture", TriggerConfig: json.RawMessage(`{"schema_version":1,"type":"conversation_explicit","config":{}}`), GraphDefinition: json.RawMessage(`{"schema_version":1,"entry_node_id":"lead","input_contract":{"type":"text"},"output_contract":{"type":"text"},"nodes":[{"id":"lead","type":"lead","config":{"instruction":"Answer"},"inputs":{"task":{"value":{"source":"run_input","path":""},"expected_type":"text"}},"output":{"type":"text"}},{"id":"deliver","type":"deliver","config":{"result":{"source":"node_output","node_id":"lead","path":""}}}],"edges":[{"id":"done","from_node_id":"lead","to_node_id":"deliver","route":"success"}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	descriptors := compiler.NewDescriptorRegistry()
	if err := descriptors.Register(compiler.NewStandardFrozenDescriptor()); err != nil {
		t.Fatal(err)
	}
	key := []byte(strings.Repeat("k", 32))
	server := &Server{Pool: pool, Registry: agents, Runtimes: runtimeStore, Workflow: store, ScheduleTransactions: pool, Descriptors: descriptors, DeliveryTargets: delivery.New(pool, key), Skills: skills.New(pool), Credentials: credentials.New(pool, key), AgentSchedules: schedule.New(pool, nil)}
	builder := workflowcatalog.NewCandidateBuilder(store, agents, server.DeliveryTargets, server.Skills, server.Credentials, server.AgentSchedules, descriptors)
	authority := teamconstruction.NewPublicationAuthority(pool, builder)
	server.PublicationAuthority = authority
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
	server.ProductPublication = teamconstruction.NewProductPublication(pool, kernelPublication, authority.AuthorizeProduct)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	candidate, report, err := authority.BuildCandidateTx(ctx, tx, workflow.CandidateInput{WorkspaceID: "ws", WorkflowID: "flow", WorkflowVersion: draft.Version})
	if err != nil || candidate == nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("initial publication err=%v report=%+v", err, report)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	command, err := teamconstruction.PublicationCommandForCandidate("initial-flow-publication", candidate, teamconstruction.PublicationTarget{TeamID: "team"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.ProductPublication.Publish(ctx, command); err != nil {
		t.Fatal(err)
	}
	call := func(input agentExecutionRequest, want int) {
		t.Helper()
		raw, _ := json.Marshal(input)
		recorder := httptest.NewRecorder()
		httpRequest := httptest.NewRequest("PUT", "/v1/agents/lead/execution", bytes.NewReader(raw)).WithContext(ctx)
		c := echo.New().NewContext(httpRequest, recorder)
		c.SetParamNames("name")
		c.SetParamValues("lead")
		c.Set("tenant", "ws")
		c.Set("user_id", "fixture")
		if err := server.handleConfigureAgentExecution(c); err != nil || recorder.Code != want {
			t.Fatalf("save status=%d want=%d err=%v body=%s", recorder.Code, want, err, recorder.Body.String())
		}
	}
	changedRequest := agentExecutionRequest{ExpectedVersion: record.Version, Engine: engine.Claude, RuntimeID: runtime.ID, Model: "native-primary", FallbackModels: []string{"native-backup"}, FallbackRetries: 1}
	call(changedRequest, 200)
	// The original request replays its completed response even though the member
	// head has advanced. It must not create another workflow version.
	call(changedRequest, 200)
	saved, err := agents.Get(ctx, "ws", "lead")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Model != "native-primary" || len(saved.FallbackModels) != 1 {
		t.Fatalf("member=%+v", saved)
	}
	var version int
	var payload []byte
	if err := pool.QueryRow(ctx, `SELECT w.published_version,a.payload FROM weave_team_workflows w JOIN weave_published_artifact_contents a ON a.workspace_id=w.workspace_id AND a.workflow_id=w.id AND a.workflow_version=w.published_version WHERE w.id='flow'`).Scan(&version, &payload); err != nil {
		t.Fatal(err)
	}
	if version != 2 || !bytes.Contains(payload, []byte("native-primary")) || !bytes.Contains(payload, []byte("native-backup")) {
		t.Fatalf("published version=%d payload=%s", version, payload)
	}
	var versionCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM weave_team_workflow_versions WHERE workspace_id='ws' AND workflow_id='flow'`).Scan(&versionCount); err != nil || versionCount != 2 {
		t.Fatalf("replay version count=%d err=%v", versionCount, err)
	}
	// Draft conflict occurs after the member write but rolls the transaction back.
	if _, err := store.CreateDraft(ctx, "ws", "flow", "fixture"); err != nil {
		t.Fatal(err)
	}
	call(agentExecutionRequest{ExpectedVersion: saved.Version, Engine: engine.Claude, RuntimeID: runtime.ID, Model: "different"}, 409)
	after, err := agents.Get(ctx, "ws", "lead")
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != saved.Version || after.Model != saved.Model {
		t.Fatalf("failed publication changed member version=%d model=%s", after.Version, after.Model)
	}
	call(agentExecutionRequest{ExpectedVersion: saved.Version - 1, Engine: engine.Claude, RuntimeID: runtime.ID, Model: "stale"}, 409)
}
