package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/app/agentcatalog"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/mcpregistry"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/labstack/echo/v4"
)

func TestMCPProductProjectionKeepsMembershipOutOfKernelRealPG(t *testing.T) {
	ctx := t.Context()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	cfg := pool.Config()
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement
	prepared, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	pool = prepared
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES('ws-a','ws-a','A'),('ws-b','ws-b','B')`); err != nil {
		t.Fatal(err)
	}
	resources := mcpregistry.New(pool, []byte(strings.Repeat("k", 32)))
	tool, err := resources.Create(ctx, "ws-a", "alice", mcpregistry.UpsertServerRequest{Slug: "tools", DisplayName: "Tools", Transport: mcpregistry.TransportStreamableHTTP, URL: "https://tools.invalid/mcp", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	directory := agentcatalog.New(pool)
	for _, entry := range []struct {
		ws, name string
		deleted  bool
	}{{"ws-a", "active", false}, {"ws-a", "removed", true}, {"ws-b", "foreign", false}} {
		record := registry.AgentRecord{Name: entry.name, Role: "worker", MCPServers: []registry.MCPServerConfig{{ServerID: tool.ID}, {ServerID: tool.ID}}}
		if err := directory.Put(ctx, entry.ws, &record); err != nil {
			t.Fatal(err)
		}
		if entry.deleted {
			if _, err := pool.Exec(ctx, `UPDATE weave_agents SET deleted=true WHERE workspace_id=$1 AND id=$2`, entry.ws, record.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	views, err := projectMCPServers(ctx, directory, "ws-a", []mcpregistry.ServerView{tool})
	if err != nil || len(views) != 1 || views[0].AgentCount != 1 {
		t.Fatalf("membership projection crossed workspace or double-counted: %+v %v", views, err)
	}
	if _, err := projectMCPServers(ctx, directory, "ws-b", []mcpregistry.ServerView{tool}); err == nil {
		t.Fatal("foreign tool projection accepted")
	}
	server := &Server{MCPRegistry: resources, Registry: directory}
	recorder := httptest.NewRecorder()
	c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/v1/mcp/servers", nil), recorder)
	c.Set("tenant", "ws-a")
	if err := server.handleListMCPServers(c); err != nil || recorder.Code != http.StatusOK {
		t.Fatalf("list response status=%d error=%v body=%s", recorder.Code, err, recorder.Body.String())
	}
	var body []map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 1 || body[0]["id"] != tool.ID || body[0]["agent_count"] != float64(1) || body[0]["tool_count"] != float64(0) {
		t.Fatalf("product response changed: %#v", body)
	}
	// The runtime resource catalog remains usable when the product directory is
	// unavailable; product presentation reports the missing facts explicitly.
	if _, err := pool.Exec(ctx, `ALTER TABLE weave_agents RENAME TO product_agent_directory`); err != nil {
		t.Fatal(err)
	}
	if _, err := resources.Get(ctx, "ws-a", tool.ID); err != nil {
		t.Fatalf("kernel catalog depends on product directory: %v", err)
	}
	if _, err := resources.ListMetadata(ctx, "ws-a"); err != nil {
		t.Fatalf("kernel discovery depends on product directory: %v", err)
	}
	if _, err := projectMCPServers(ctx, directory, "ws-a", []mcpregistry.ServerView{tool}); err == nil {
		t.Fatal("missing product membership was reported as zero")
	}
}

type incompleteMCPReferences struct{}

func (incompleteMCPReferences) MCPReferenceCounts(context.Context, string, []string) (map[string]int, error) {
	return map[string]int{}, nil
}
func TestMCPProductProjectionRequiresCompleteFacts(t *testing.T) {
	_, err := projectMCPServers(t.Context(), incompleteMCPReferences{}, "workspace", []mcpregistry.ServerView{{Server: mcpregistry.Server{ID: "tool", WorkspaceID: "workspace"}}})
	if err == nil {
		t.Fatal("missing count was reported as zero")
	}
}
