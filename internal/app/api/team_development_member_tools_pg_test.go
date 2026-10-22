package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/app/agentcatalog"
	orgstore "github.com/jinyitao123/weave/internal/app/org"
	"github.com/jinyitao123/weave/internal/app/teamconstruction"
	"github.com/jinyitao123/weave/internal/app/workflowcatalog"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/delivery"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
	org "github.com/jinyitao123/weave/internal/kernel/orgspec"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/schedule"
	"github.com/jinyitao123/weave/internal/kernel/skills"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/labstack/echo/v4"
)

// A developer gives a member an MCP service, a library skill and a tool rule
// by name; the draft stays trialable, and the prepared member carries the
// service with every tool not declared read-only behind the write gate.
func TestDevelopmentMemberExternalToolsRealPG(t *testing.T) {
	const ws = "tools-workspace"
	ctx := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: ws, UserID: "developer"})
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES($1,$1,'Tools');
		INSERT INTO weave_users(id,tenant_id,username,password,role) VALUES('developer',$1,'developer','unused','admin');
		INSERT INTO weave_mcp_servers(id,workspace_id,slug,display_name,transport,url,headers_cipher,env_cipher,enabled,created_by,last_handshake_at)
			VALUES('contracts-server',$1,'contracts','合同库','streamable_http','http://127.0.0.1:1/mcp','','',true,'developer',now()),
			      ('offline-server',$1,'offline','未连通服务','streamable_http','http://127.0.0.1:2/mcp','','',true,'developer',NULL);
		INSERT INTO weave_mcp_tools(workspace_id,server_id,name,description,input_schema,read_only_hint,discovered_at)
			VALUES($1,'contracts-server','read_contract','读取合同','{"type":"object"}',true,now()),
			      ($1,'contracts-server','delete_contract','删除合同','{"type":"object"}',NULL,now())`, ws); err != nil {
		t.Fatal(err)
	}
	// The skill library writes its resources as JSON bytes, which the test
	// pools' simple protocol sends as text of another type; this one pool
	// talks to the same schema the way the server does.
	extendedConfig := pool.Config()
	extendedConfig.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement
	extended, err := pgxpool.NewWithConfig(ctx, extendedConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer extended.Close()
	if _, err := skills.New(extended).CreateVersion(ctx, ws, "quote-check", skills.CreateVersionInput{Name: "报价核对", Description: "核对报价时使用", Body: "逐项核对单价、数量和税率。"}); err != nil {
		t.Fatal(err)
	}

	agents := agentcatalog.New(pool)
	lead := registry.AgentRecord{Name: "lead", DisplayName: "负责人", Role: "avatar", Engine: "loom", Model: "fixture-model", GraphType: "standard", Spec: stdlib.AgentSpec{SystemPrompt: "Coordinate"}}
	worker := registry.AgentRecord{Name: "worker", DisplayName: "审核员", Role: "worker", Engine: "loom", Model: "fixture-model", GraphType: "standard", Spec: stdlib.AgentSpec{SystemPrompt: "Review"}}
	for _, rec := range []*registry.AgentRecord{&lead, &worker} {
		if err := agents.Put(ctx, ws, rec); err != nil {
			t.Fatal(err)
		}
	}
	created, err := orgstore.NewStore(pool).CreateActiveTeam(ctx, ws, org.CreateActiveTeamInput{Name: "tools", Objective: "审核合同", LeadAvatarID: lead.ID, Workers: []org.InitialTeamWorker{{WorkerAgentID: worker.ID, Duty: "审核合同", AllowedKinds: []string{"consult"}, DefaultKind: "consult"}}})
	if err != nil {
		t.Fatal(err)
	}
	key := []byte(strings.Repeat("k", 32))
	providers := credentials.New(pool, key)
	if err = providers.Upsert(execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: ws, ServiceID: "provider:fixture"}), ws, llmrouter.ProviderConfig{Protocol: "openai-completions", ChatPath: "/v1/chat/completions", CredentialScope: frozen.CredentialScopeWorkspaceService, CredentialServiceID: "provider:fixture", ID: "fixture", Name: "Fixture", BaseURL: "http://127.0.0.1:1", APIKey: "test-only", Models: []string{"fixture-model"}}); err != nil {
		t.Fatal(err)
	}
	store := workflowcatalog.New(pool, nil, workflow.NewArtifactStore(pool, nil))
	builder := workflowcatalog.NewCandidateBuilder(store, agents, delivery.New(pool, key), skills.New(pool), providers, schedule.New(pool, nil), memberIntegrationDescriptors(t))
	server := &Server{Pool: pool, Registry: agents, PublicationAuthority: teamconstruction.NewPublicationAuthority(pool, builder)}
	call := func(method string, body any, handler echo.HandlerFunc) (int, map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(method, "/", bytes.NewReader(raw)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		c := echo.New().NewContext(req, rr)
		c.SetParamNames("id", "topic")
		c.SetParamValues(created.Team.ID, "member_external")
		c.Set("tenant", ws)
		c.Set("user_id", "developer")
		if err := handler(c); err != nil {
			if failure, ok := err.(*echo.HTTPError); ok {
				return failure.Code, map[string]any{"error": failure.Message}
			}
			t.Fatal(err)
		}
		var decoded map[string]any
		_ = json.Unmarshal(rr.Body.Bytes(), &decoded)
		return rr.Code, decoded
	}

	// The guide names what may be given right now.
	if status, guide := call("GET", nil, server.handleGetDevelopmentOperationTopic); status != http.StatusOK || len(guide["mcpServers"].([]any)) != 2 || guide["skills"].([]any)[0] != "报价核对" {
		t.Fatalf("guide: %d %v", status, guide)
	}
	_, draft := call("GET", nil, server.handleGetTeamDevelopment)
	revision := draft["revision"].(float64)
	apply := func(operations ...map[string]any) (int, map[string]any) {
		t.Helper()
		status, answer := call("POST", map[string]any{"expected_revision": revision, "request_id": uuid.NewString(), "operations": operations}, server.handleApplyTeamDevelopmentOperations)
		if status == http.StatusOK {
			revision = answer["revision"].(float64)
		}
		return status, answer
	}
	if status, refused := apply(map[string]any{"kind": "member_mcp", "member": "审核员", "server": "未连通服务", "selected": true}); status != http.StatusUnprocessableEntity {
		t.Fatalf("an unprobed service was given: %d %v", status, refused)
	}
	status, saved := apply(
		map[string]any{"kind": "member", "member": "负责人", "duty": "理解任务"},
		map[string]any{"kind": "flow_add", "name": "审核", "description": "审核合同", "member": "审核员"},
		map[string]any{"kind": "member_mcp", "member": "审核员", "server": "合同库", "selected": true},
		map[string]any{"kind": "member_library_skill", "member": "审核员", "skill": "报价核对", "selected": true},
		map[string]any{"kind": "member_tool_rule", "member": "审核员", "list": "ask", "tool": "send_mail", "selected": true},
	)
	if status != http.StatusOK {
		t.Fatalf("apply: %d %v", status, saved)
	}
	for _, raw := range saved["issues"].([]any) {
		if code := raw.(map[string]any)["code"]; code == "member_tools_unsupported" || code == "member_engine_unsupported" {
			t.Fatalf("external tools still block the trial: %v", saved["issues"])
		}
	}

	prepared, err := server.prepareDevelopment(ctx, ws, created.Team.ID, "developer", int64(revision))
	if err != nil {
		t.Fatal(err)
	}
	staged := prepared.Prepared[0].Members[worker.ID]
	if len(staged.MCPServers) != 1 || staged.MCPServers[0].ServerID != "contracts-server" || !slices.Equal(staged.MCPServers[0].WriteTools, []string{"delete_contract"}) {
		t.Fatalf("staged MCP binding = %+v", staged.MCPServers)
	}
	if len(staged.SkillRefs) != 1 || staged.SkillRefs[0].Name != "报价核对" || staged.SkillRefs[0].SkillVersion == nil || *staged.SkillRefs[0].SkillVersion != 1 {
		t.Fatalf("staged skills = %+v", staged.SkillRefs)
	}
	if !slices.Equal(staged.Permissions.Ask, []string{"send_mail"}) {
		t.Fatalf("staged permissions = %+v", staged.Permissions)
	}
	current, err := agents.Get(ctx, ws, worker.Name)
	if err != nil || current.Version != 1 || len(current.MCPServers) != 0 {
		t.Fatalf("preparing a trial changed the active member: %+v %v", current, err)
	}

	if status, removed := apply(map[string]any{"kind": "member_mcp", "member": "审核员", "server": "合同库", "selected": false}); status != http.StatusOK {
		t.Fatalf("remove: %d %v", status, removed)
	}
}
