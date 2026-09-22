package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/app/agentcatalog"
	orgstore "github.com/jinyitao123/weave/internal/app/org"
	"github.com/jinyitao123/weave/internal/app/teamconstruction"
	"github.com/jinyitao123/weave/internal/app/workflowcatalog"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/businessaction"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/delivery"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
	org "github.com/jinyitao123/weave/internal/kernel/orgspec"
	"github.com/jinyitao123/weave/internal/kernel/publication"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/schedule"
	"github.com/jinyitao123/weave/internal/kernel/skills"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/labstack/echo/v4"
)

func TestTeamDevelopmentStagingAndAtomicPublicationRealPG(t *testing.T) {
	ctx := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "dev-workspace", UserID: "developer"})
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES('dev-workspace','dev-workspace','Development'); INSERT INTO weave_users(id,tenant_id,username,password,role) VALUES('developer','dev-workspace','developer','unused','admin')`); err != nil {
		t.Fatal(err)
	}
	agents := agentcatalog.New(pool)
	lead := registry.AgentRecord{Name: "lead", DisplayName: "负责人", Role: "avatar", Engine: "loom", Model: "fixture-model", GraphType: "standard", Spec: stdlib.AgentSpec{SystemPrompt: "Coordinate"}}
	worker := registry.AgentRecord{Name: "worker", DisplayName: "审核员", Role: "worker", Engine: "loom", Model: "fixture-model", GraphType: "standard", Spec: stdlib.AgentSpec{SystemPrompt: "Review original"}}
	for _, rec := range []*registry.AgentRecord{&lead, &worker} {
		if err := agents.Put(ctx, "dev-workspace", rec); err != nil {
			t.Fatal(err)
		}
	}
	created, err := orgstore.NewStore(pool).CreateActiveTeam(ctx, "dev-workspace", org.CreateActiveTeamInput{Name: "development", Objective: "审核合同原件", LeadAvatarID: lead.ID, Workers: []org.InitialTeamWorker{{WorkerAgentID: worker.ID, Duty: "审核合同", AllowedKinds: []string{"consult"}, DefaultKind: "consult"}}})
	if err != nil {
		t.Fatal(err)
	}
	key := []byte(strings.Repeat("k", 32))
	providers := credentials.New(pool, key)
	providerService := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "dev-workspace", ServiceID: "provider:fixture"})
	if err = providers.Upsert(providerService, "dev-workspace", llmrouter.ProviderConfig{CredentialScope: frozen.CredentialScopeWorkspaceService, CredentialServiceID: "provider:fixture", ID: "fixture", Name: "Fixture", BaseURL: "http://127.0.0.1:1", APIKey: "test-only", Models: []string{"fixture-model"}}); err != nil {
		t.Fatal(err)
	}
	store := workflowcatalog.New(pool, nil, workflow.NewArtifactStore(pool, nil))
	builder := workflowcatalog.NewCandidateBuilder(store, agents, delivery.New(pool, key), skills.New(pool), providers, schedule.New(pool, nil), memberIntegrationDescriptors(t))
	authority := teamconstruction.NewPublicationAuthority(pool, builder)
	server := &Server{Pool: pool, Registry: agents, PublicationAuthority: authority, KernelPublication: openAPIKernelPublication(t, ctx, pool, authority)}
	call := func(method string, body any, handler echo.HandlerFunc) (*httptest.ResponseRecorder, error) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(method, "/", bytes.NewReader(raw)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		c := echo.New().NewContext(req, rr)
		c.SetParamNames("id")
		c.SetParamValues(created.Team.ID)
		c.Set("tenant", "dev-workspace")
		c.Set("user_id", "developer")
		return rr, handler(c)
	}
	rr, err := call("GET", nil, server.handleGetTeamDevelopment)
	if err != nil {
		t.Fatal(err)
	}
	var d developmentDraft
	if err = json.Unmarshal(rr.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	for i := range d.Document.Members {
		d.Document.Members[i].Relationship.Duty = "审核与核对"
		d.Document.Members[i].Configuration.SystemPrompt = "必须阅读收到的原文，保留逐条依据"
		if d.Document.Members[i].Configuration.Role == "worker" {
			d.Document.Members[i].Configuration.BusinessCapabilityIDs = []string{"forge:action:sales_contract.ContractSubmit"}
		}
	}
	newWorker := d.Document.Members[1]
	newWorker.ID = uuid.NewString()
	newWorker.Configuration.DisplayName = "提交员"
	newWorker.Configuration.SystemPrompt = "只提交经过复核的固定材料"
	newWorker.Relationship.Duty = "提交固定材料"
	d.Document.Members = append(d.Document.Members, newWorker)
	flowID := uuid.NewString()
	graph := json.RawMessage(fmt.Sprintf(`{"schema_version":1,"entry_node_id":"lead","input_contract":{"type":"text"},"output_contract":{"type":"text"},"nodes":[{"id":"lead","type":"lead","config":{"instruction":"Understand"},"inputs":{"task":{"value":{"source":"run_input","path":""},"expected_type":"text"}},"output":{"type":"text"}},{"id":"review","type":"worker","config":{"kind":"consult","agent_id":%q,"agent_version":1,"result_requirement":"逐条审核"},"inputs":{"original":{"value":{"source":"run_input","path":""},"expected_type":"text"},"brief":{"value":{"source":"node_output","node_id":"lead","path":""},"expected_type":"text"}},"output":{"type":"text"}},{"id":"deliver","type":"deliver","config":{"result":{"source":"node_output","node_id":"review","path":""}}}],"edges":[{"id":"a","from_node_id":"lead","to_node_id":"review","route":"success"},{"id":"b","from_node_id":"review","to_node_id":"deliver","route":"success"}]}`, newWorker.ID))
	d.Document.Workflows = []developmentWorkflow{{ID: flowID, Name: "审核", Graph: graph, Trigger: json.RawMessage(`{"schema_version":1,"type":"conversation_explicit","config":{}}`)}}
	save := map[string]any{"expected_revision": d.Revision, "document": d.Document}
	if _, err = call("PUT", save, server.handleSaveTeamDevelopment); err != nil {
		t.Fatal(err)
	}
	if _, err = call("PUT", save, server.handleSaveTeamDevelopment); err == nil {
		t.Fatal("stale edit overwrote draft")
	}
	prepared, err := server.prepareDevelopment(ctx, "dev-workspace", created.Team.ID, "developer", 2)
	if err != nil {
		t.Fatal(err)
	}
	current, err := agents.Get(ctx, "dev-workspace", worker.Name)
	if err != nil || current.Version != 1 {
		t.Fatalf("trial moved active head: %+v %v", current, err)
	}
	again, err := server.prepareDevelopment(ctx, "dev-workspace", created.Team.ID, "developer", 2)
	if err != nil || again.Prepared[0].Envelope.ContentHash != prepared.Prepared[0].Envelope.ContentHash {
		t.Fatalf("retry changed candidate: %v", err)
	}
	// Real Kernel admission; no model endpoint is contacted by admission itself.
	trial := developmentTrialRequest{Revision: 2, WorkflowID: flowID, RequestID: uuid.NewString(), Input: "合同原文：金额 ¥186,420.50\n签字页", BusinessActions: []businessaction.DevelopmentAction{{CapabilityID: "forge:action:sales_contract.ContractSubmit", Name: "ContractSubmit", ObjectName: "sales_contract", Label: "提交指定合同版本", RequiresRecord: true}}}
	rr, err = call("POST", trial, server.handleTrialTeamDevelopment)
	if err != nil || rr.Code != 201 {
		t.Fatalf("trial %v %s", err, rr.Body.String())
	}
	first := rr.Body.String()
	rr, err = call("POST", trial, server.handleTrialTeamDevelopment)
	if err != nil || rr.Body.String() != first {
		t.Fatalf("retry changed receipt: %v %s", err, rr.Body.String())
	}
	trial.Input = "different"
	if _, err = call("POST", trial, server.handleTrialTeamDevelopment); err == nil {
		t.Fatal("same request accepted different material")
	}
	if _, err = call("POST", map[string]int{"revision": 2}, server.handlePublishTeamDevelopment); err == nil {
		t.Fatal("published without successful trial")
	}
	// Exercise exact production publication and atomic activation, independently
	// of the success gate above. This is not an end-to-end model acceptance test.
	for _, p := range prepared.Prepared {
		req := publication.PublishRequest{Version: publication.ContractVersion, RequestID: "fixture-publish:" + p.Envelope.ContentHash, Candidate: p.Envelope}
		receipt, err := server.KernelPublication.Publish(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if err = receipt.Verify(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	if err = server.activateDevelopment(ctx, "dev-workspace", created.Team.ID, "developer", prepared); err != nil {
		t.Fatal(err)
	}
	if err = server.activateDevelopment(ctx, "dev-workspace", created.Team.ID, "developer", prepared); err != nil {
		t.Fatal("activation replay", err)
	}
	current, err = agents.Get(ctx, "dev-workspace", worker.Name)
	if err != nil || current.Version != 2 {
		t.Fatalf("active head not moved: %+v %v", current, err)
	}
	rr, err = call("GET", nil, server.handleGetTeamDevelopment)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(rr.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	d.Document.Objective = "再次修改"
	if _, err = call("PUT", map[string]any{"expected_revision": d.Revision, "document": d.Document}, server.handleSaveTeamDevelopment); err != nil {
		t.Fatal(err)
	}
	if _, err = server.prepareDevelopment(ctx, "dev-workspace", created.Team.ID, "developer", 3); err != nil {
		t.Fatal("edit after publication", err)
	}
}
