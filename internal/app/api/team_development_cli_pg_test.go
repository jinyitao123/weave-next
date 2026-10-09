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
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/delivery"
	org "github.com/jinyitao123/weave/internal/kernel/orgspec"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/jinyitao123/weave/internal/kernel/schedule"
	"github.com/jinyitao123/weave/internal/kernel/skills"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/labstack/echo/v4"
)

// A team of CLI members (Claude codes, Codex verifies) is prepared for a
// trial without any provider model: the members run on runtime nodes with the
// node's own engine login.
func TestTeamDevelopmentPreparesCLIMembersRealPG(t *testing.T) {
	ctx := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "cli-workspace", UserID: "developer"})
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES('cli-workspace','cli-workspace','CLI'); INSERT INTO weave_users(id,tenant_id,username,password,role) VALUES('developer','cli-workspace','developer','unused','admin')`); err != nil {
		t.Fatal(err)
	}
	agents := agentcatalog.New(pool)
	lead := registry.AgentRecord{Name: "cli-lead", DisplayName: "负责人", Role: "avatar", Engine: "loom", GraphType: "standard", Spec: stdlib.AgentSpec{SystemPrompt: "理解目标"}}
	coder := registry.AgentRecord{Name: "cli-coder", DisplayName: "编码", Role: "worker", Engine: "claude", GraphType: "standard", Spec: stdlib.AgentSpec{SystemPrompt: "修改代码"}}
	verifier := registry.AgentRecord{Name: "cli-verifier", DisplayName: "验证", Role: "worker", Engine: "codex", GraphType: "standard", Spec: stdlib.AgentSpec{SystemPrompt: "运行测试"}}
	for _, record := range []*registry.AgentRecord{&lead, &coder, &verifier} {
		if err := agents.Put(ctx, "cli-workspace", record); err != nil {
			t.Fatal(err)
		}
	}
	created, err := orgstore.NewStore(pool).CreateActiveTeam(ctx, "cli-workspace", org.CreateActiveTeamInput{
		Name: "cli-team", Objective: "开发并验证", LeadAvatarID: lead.ID,
		Workers: []org.InitialTeamWorker{
			{WorkerAgentID: coder.ID, Duty: "编码", AllowedKinds: []string{"consult"}, DefaultKind: "consult"},
			{WorkerAgentID: verifier.ID, Duty: "验证", AllowedKinds: []string{"consult"}, DefaultKind: "consult"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	key := []byte(strings.Repeat("k", 32))
	providers := credentials.New(pool, key)
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
		c.Set("tenant", "cli-workspace")
		c.Set("user_id", "developer")
		return rr, handler(c)
	}
	rr, err := call("GET", nil, server.handleGetTeamDevelopment)
	if err != nil {
		t.Fatal(err)
	}
	var draft developmentDraft
	if err = json.Unmarshal(rr.Body.Bytes(), &draft); err != nil {
		t.Fatal(err)
	}
	// Publication freezes a CLI member's runtime binding, so each CLI member
	// is pinned to a registered node that offers its engine.
	runtimeStore := runtimes.NewStore(pool)
	node, _, err := runtimeStore.Create(ctx, "cli-workspace", "claude-node")
	if err != nil {
		t.Fatal(err)
	}
	codexNode, _, err := runtimeStore.Create(ctx, "cli-workspace", "codex-node")
	if err != nil {
		t.Fatal(err)
	}
	capability := func(name string) runtimes.EngineCapability {
		return runtimes.EngineCapability{Engine: name, BinaryPath: "/fixture/" + name, BinaryVersion: "1", ProtocolVersion: "1", EndpointClass: "fixture", AuthMode: "oauth", Availability: runtimes.EngineAvailabilityReady}
	}
	for _, registered := range []string{node.ID, codexNode.ID} {
		if err := runtimeStore.HelloWithCapabilities(ctx, "cli-workspace", registered, []string{"claude", "codex"}, []runtimes.EngineCapability{capability("claude"), capability("codex")}, 2); err != nil {
			t.Fatal(err)
		}
	}
	nodeFor := map[string]string{"claude": node.ID, "codex": codexNode.ID}
	for index := range draft.Document.Members {
		draft.Document.Members[index].Relationship.Duty = "按职责完成"
		if draft.Document.Members[index].Configuration.Role == "worker" {
			draft.Document.Members[index].Configuration.RuntimeID = nodeFor[draft.Document.Members[index].Configuration.Engine]
		}
		if draft.Document.Members[index].Configuration.Model != "" {
			t.Fatalf("CLI member gained a model: %+v", draft.Document.Members[index].Configuration)
		}
	}
	graph := json.RawMessage(fmt.Sprintf(`{"schema_version":1,"entry_node_id":"code","input_contract":{"type":"text"},"output_contract":{"type":"text"},"nodes":[`+
		`{"id":"code","type":"worker","label":"编码","config":{"kind":"consult","agent_id":%q,"agent_version":1,"result_requirement":"改代码"},"inputs":{"original":{"value":{"source":"run_input","path":""},"expected_type":"text"}},"output":{"type":"text"}},`+
		`{"id":"verify","type":"worker","label":"验证","config":{"kind":"consult","agent_id":%q,"agent_version":1,"result_requirement":"跑测试"},"inputs":{"original":{"value":{"source":"run_input","path":""},"expected_type":"text"},"previous":{"value":{"source":"node_output","node_id":"code","path":""},"expected_type":"text"}},"output":{"type":"text"}},`+
		`{"id":"deliver","type":"deliver","config":{"result":{"source":"node_output","node_id":"verify","path":""}}}],`+
		`"edges":[{"id":"a","from_node_id":"code","to_node_id":"verify","route":"success"},{"id":"b","from_node_id":"verify","to_node_id":"deliver","route":"success"}]}`, coder.ID, verifier.ID))
	draft.Document.Workflows = []developmentWorkflow{{ID: uuid.NewString(), Name: "开发与验证", Graph: graph, Trigger: json.RawMessage(`{"schema_version":1,"type":"conversation_explicit","config":{}}`)}}
	if _, err = call("PUT", map[string]any{"expected_revision": draft.Revision, "document": draft.Document}, server.handleSaveTeamDevelopment); err != nil {
		t.Fatal(err)
	}
	prepared, err := server.prepareDevelopment(ctx, "cli-workspace", created.Team.ID, "developer", draft.Revision+1)
	if err != nil {
		t.Fatalf("prepare CLI team: %v", err)
	}
	if len(prepared.Prepared) != 1 {
		t.Fatalf("prepared = %+v", prepared.Prepared)
	}
	for _, member := range prepared.Prepared[0].Members {
		if member.Role == "worker" && (member.Engine == "loom" || member.Model != "" || member.RuntimeID != nodeFor[member.Engine]) {
			t.Fatalf("CLI member was not kept as a pinned CLI member: %+v", member)
		}
	}
	revision := draft.Revision + 1
	resave := func(pin func(engine string) string) error {
		t.Helper()
		for index := range draft.Document.Members {
			if draft.Document.Members[index].Configuration.Role == "worker" {
				draft.Document.Members[index].Configuration.RuntimeID = pin(draft.Document.Members[index].Configuration.Engine)
			}
		}
		if _, err := call("PUT", map[string]any{"expected_revision": revision, "document": draft.Document}, server.handleSaveTeamDevelopment); err != nil {
			t.Fatal(err)
		}
		revision++
		_, err := server.prepareDevelopment(ctx, "cli-workspace", created.Team.ID, "developer", revision)
		return err
	}
	// Two engines on one node are refused with a readable reason.
	if err := resave(func(string) string { return node.ID }); err == nil || !strings.Contains(err.Error(), "分配到不同节点") {
		t.Fatalf("mixed engines on one node = %v", err)
	}
	// An unpinned CLI member cannot be published: the runtime binding is frozen.
	if err := resave(func(string) string { return "" }); err == nil {
		t.Fatal("unpinned CLI member was prepared")
	}
}
