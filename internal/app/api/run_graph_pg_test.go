package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/labstack/echo/v4"
)

func TestRunGraphReadsTheAdmittedCandidateAndRequiresDeveloperAccessRealPG(t *testing.T) {
	t.Setenv("WEAVE_SECRET_KEY_FILE", "")
	t.Setenv("WEAVE_SECRET_KEY", strings.Repeat("31", 32))
	ctx := execution.WithSubject(t.Context(), execution.Subject{WorkspaceID: "ws", UserID: "user"})
	seed := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, seed); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES('ws','ws','ws');
		INSERT INTO weave_users(id,tenant_id,username,password,role) VALUES('user','ws','user','unused','admin');
		INSERT INTO weave_external_identities(issuer,subject,workspace_id,user_id,native_organization) VALUES('forge:task-delegation-test','native-user','ws','user','native-org')`); err != nil {
		t.Fatal(err)
	}
	cfg := seed.Config()
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	_, _, envelope, authority := publishBusinessCompletionSample(t, pool, []byte(strings.Repeat("1", 32)), "http://127.0.0.1:1", publishedSecondBusinessCapability)
	kernel := openAPIKernelPublication(t, ctx, pool, authority)
	trial := admitDevelopmentCoverageTrial(t, ctx, pool, kernel, envelope, developmentTestActionDefinitions(t, false, false), "graph snapshot fixture")
	runs := teamrun.NewPGStore()
	runs.Transactions = pool
	server := &Server{Pool: pool, Store: teamDeliveryPoolStore{teamDispatchPoolStore{pool: pool}}, Snapshots: snapshot.NewStore(pool), WorkflowArtifacts: workflow.NewArtifactStore(pool, nil),
		teamRunCancel: &teamrun.CancelService{Transactions: pool, Runs: runs}}
	read := func(workspace, role, runID string) *httptest.ResponseRecorder {
		t.Helper()
		actor := "user"
		if role == "admin" {
			actor = "reviewer"
		}
		viewer := execution.WithSubject(t.Context(), execution.Subject{WorkspaceID: workspace, UserID: actor})
		recorder := httptest.NewRecorder()
		c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/v1/runs/"+runID+"/graph", nil).WithContext(viewer), recorder)
		c.Set("tenant", workspace)
		c.Set("user_id", actor)
		c.Set("roles", []string{role})
		c.SetParamNames("id")
		c.SetParamValues(runID)
		if err := server.handleGetRunGraph(c); err != nil {
			t.Fatal(err)
		}
		return recorder
	}
	if response := read("ws", "member", trial.runID); response.Code != http.StatusForbidden {
		t.Fatalf("member read trial graph: %d %s", response.Code, response.Body.String())
	}
	for _, request := range [][2]string{{"other-workspace", trial.runID}, {"ws", "missing-run"}} {
		if response := read(request[0], "admin", request[1]); response.Code != http.StatusNotFound {
			t.Fatalf("graph scope %v: %d %s", request, response.Code, response.Body.String())
		}
	}
	// A later catalog draft must not replace the admitted graph.
	payload, err := frozen.DecodeArtifactEnvelopeV1(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_team_workflow_versions(workspace_id,workflow_id,version,status,trigger_config,graph_definition,created_by)
		VALUES('ws','flow',$1,'draft',$2,jsonb_set($3::jsonb,'{nodes,0,label}','"Later draft"'),'user')`, envelope.WorkflowVersion+1, payload.TriggerConfig, payload.GraphDefinition); err != nil {
		t.Fatal(err)
	}
	response := read("ws", "developer", trial.runID)
	var graph runGraphView
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &graph) != nil {
		t.Fatalf("developer graph: %d %s", response.Code, response.Body.String())
	}
	if graph.RunID != trial.runID || graph.WorkflowID != envelope.WorkflowID || graph.WorkflowVersion != envelope.WorkflowVersion || graph.ContentHash != envelope.ContentHash || !graph.DevelopmentTrial || canonicalDevelopmentValue(graph.GraphDefinition) != canonicalDevelopmentValue(payload.GraphDefinition) {
		t.Fatalf("run did not return its admitted graph: %+v", graph)
	}
	if response := read("ws", "admin", trial.runID); response.Code != http.StatusOK {
		t.Fatalf("authorized reviewer could not inspect another actor's graph: %d %s", response.Code, response.Body.String())
	}
}
