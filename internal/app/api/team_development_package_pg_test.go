package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"

	"github.com/jinyitao123/weave/internal/app/agentcatalog"
	orgstore "github.com/jinyitao123/weave/internal/app/org"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/skills"
)

// A team exported from one workspace is imported into another as a new
// draft: what only means something by name is looked up there, and what is
// not found is left out and said so.
func TestTeamPackageCarriesATeamToAnotherWorkspaceRealPG(t *testing.T) {
	ctx := context.Background()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES('source','source','Source'),('target','target','Target');
		INSERT INTO weave_users(id,tenant_id,username,password,role) VALUES('dev-source','source','dev-source','unused','admin'),('dev-target','target','dev-target','unused','admin');
		INSERT INTO weave_mcp_servers(id,workspace_id,slug,display_name,transport,url,headers_cipher,env_cipher,enabled,created_by,last_handshake_at)
			VALUES('contracts-server','source','contracts','合同库','streamable_http','http://127.0.0.1:1/mcp','','',true,'dev-source',now())`); err != nil {
		t.Fatal(err)
	}
	// The skill library writes JSON bytes, which the test pools' simple
	// protocol would send as another type; this pool talks the server's way.
	extendedConfig := pool.Config()
	extendedConfig.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement
	extended, err := pgxpool.NewWithConfig(ctx, extendedConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer extended.Close()
	if _, err := skills.New(extended).CreateVersion(ctx, "source", "quote-check", skills.CreateVersionInput{Name: "报价核对", Description: "核对报价时使用", Body: "逐项核对单价、数量和税率。"}); err != nil {
		t.Fatal(err)
	}

	server := &Server{Pool: pool, Store: teamDispatchPoolStore{pool: pool}, Registry: agentcatalog.New(pool), OrgStore: orgstore.NewStore(pool)}
	call := func(ws, user, method string, body any, handler echo.HandlerFunc, teamID string) (int, []byte) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(method, "/", bytes.NewReader(raw)).WithContext(execution.WithSubject(ctx, execution.Subject{WorkspaceID: ws, UserID: user}))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		c := echo.New().NewContext(req, rr)
		if teamID != "" {
			c.SetParamNames("id")
			c.SetParamValues(teamID)
		}
		c.Set("tenant", ws)
		c.Set("user_id", user)
		if err := handler(c); err != nil {
			if failure, ok := err.(*echo.HTTPError); ok {
				return failure.Code, []byte(`{}`)
			}
			t.Fatalf("handler: %v", err)
		}
		return rr.Code, rr.Body.Bytes()
	}

	// A source team whose member has a model, an MCP service and a library skill.
	status, raw := call("source", "dev-source", "POST", map[string]any{"request_id": uuid.NewString(), "name": "合同复核团队", "objective": "复核合同条款"}, server.handleCreateDevelopmentTeam, "")
	var created developmentTeamCreated
	if err := json.Unmarshal(raw, &created); err != nil || status != http.StatusCreated {
		t.Fatalf("create: %d %s", status, raw)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	draft, err := readDevelopment(ctx, tx, "source", created.TeamID, true)
	if err != nil {
		t.Fatal(err)
	}
	var workerID string
	for i := range draft.Document.Members {
		if draft.Document.Members[i].Configuration.Role == "worker" {
			workerID = draft.Document.Members[i].ID
			draft.Document.Members[i].Configuration.Model = "source-only-model"
			draft.Document.Members[i].Configuration.MCPServerIDs = []string{"contracts-server"}
			draft.Document.Members[i].Configuration.SkillNames = []string{"报价核对"}
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE weave_team_development_drafts SET document=$3 WHERE workspace_id=$1 AND team_id=$2`, "source", created.TeamID, encodeDevelopment(draft.Document)); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	status, raw = call("source", "dev-source", "GET", nil, server.handleExportTeamDevelopment, created.TeamID)
	var pkg teamPackage
	if err := json.Unmarshal(raw, &pkg); err != nil || status != http.StatusOK {
		t.Fatalf("export: %d %s", status, raw)
	}
	if strings.Contains(string(raw), workerID) || strings.Contains(string(raw), "contracts-server") {
		t.Fatal("the package carries workspace-only ids")
	}
	var exported *teamPackageMember
	for i := range pkg.Members {
		if pkg.Members[i].Configuration.Role == "worker" {
			exported = &pkg.Members[i]
		}
	}
	if exported == nil || len(exported.MCPServers) != 1 || exported.MCPServers[0] != "合同库" ||
		len(exported.LibrarySkills) != 1 || exported.LibrarySkills[0].Body != "逐项核对单价、数量和税率。" || len(pkg.Workflows) != 1 {
		t.Fatalf("exported package = %+v", pkg)
	}

	// Imported into a workspace that has none of these.
	requestID := uuid.NewString()
	status, raw = call("target", "dev-target", "POST", map[string]any{"request_id": requestID, "package": pkg}, server.handleImportDevelopmentTeam, "")
	var imported developmentTeamCreated
	if err := json.Unmarshal(raw, &imported); err != nil || status != http.StatusCreated || imported.Name != "合同复核团队" {
		t.Fatalf("import: %d %s", status, raw)
	}
	joined := strings.Join(imported.Adjustments, "\n")
	for _, expected := range []string{"source-only-model", "合同库", "报价核对"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("adjustments do not mention %s: %v", expected, imported.Adjustments)
		}
	}
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	target, err := readDevelopment(ctx, tx, "target", imported.TeamID, false)
	_ = tx.Rollback(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(target.Document.Members) != 2 || len(target.Document.Workflows) != 1 {
		t.Fatalf("imported draft = %+v", target.Document)
	}
	var newWorker developmentMember
	for _, member := range target.Document.Members {
		if member.Configuration.Role == "worker" {
			newWorker = member
		}
	}
	if newWorker.Configuration.Model != "" || len(newWorker.Configuration.MCPServerIDs) != 0 || len(newWorker.Configuration.SkillNames) != 0 ||
		len(newWorker.Configuration.Skills) != 1 || newWorker.Configuration.Skills[0].Name != "报价核对" {
		t.Fatalf("imported member = %+v", newWorker.Configuration)
	}
	if !strings.Contains(string(target.Document.Workflows[0].Graph), newWorker.ID) {
		t.Fatal("the imported flow does not name the imported member")
	}

	// The same request again is the same team; a package without a lead is refused.
	status, raw = call("target", "dev-target", "POST", map[string]any{"request_id": requestID, "package": pkg}, server.handleImportDevelopmentTeam, "")
	var replay developmentTeamCreated
	if err := json.Unmarshal(raw, &replay); err != nil || status != http.StatusOK || replay.TeamID != imported.TeamID {
		t.Fatalf("replay: %d %s", status, raw)
	}
	leaderless := pkg
	leaderless.Members = []teamPackageMember{*exported}
	if status, raw = call("target", "dev-target", "POST", map[string]any{"request_id": uuid.NewString(), "package": leaderless}, server.handleImportDevelopmentTeam, ""); status != http.StatusUnprocessableEntity {
		t.Fatalf("leaderless package: %d %s", status, raw)
	}
}
