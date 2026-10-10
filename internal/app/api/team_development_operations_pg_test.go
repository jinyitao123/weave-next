package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jinyitao123/loom/stdlib"
	"github.com/labstack/echo/v4"

	"github.com/jinyitao123/weave/internal/app/agentcatalog"
	orgstore "github.com/jinyitao123/weave/internal/app/org"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/testutil"
	org "github.com/jinyitao123/weave/internal/kernel/orgspec"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

func TestDevelopmentOperationCatalogMatchesWhatIsAccepted(t *testing.T) {
	documented := map[string]bool{}
	for _, topic := range developmentOperationTopics {
		if topic.Title == "" || topic.Covers == "" || len(topic.Rules) == 0 {
			t.Errorf("topic %s is incomplete", topic.Topic)
		}
		for _, operation := range topic.Operations {
			var example map[string]any
			if err := json.Unmarshal([]byte(operation.Example), &example); err != nil || example["kind"] != operation.Kind || operation.Summary == "" || documented[operation.Kind] {
				t.Errorf("operation %s: example %v, %v", operation.Kind, example, err)
			}
			documented[operation.Kind] = true
			// A documented kind reaches its own handling, never the "unsupported" refusal.
			doc := editTestDocument()
			_, err := applyDevelopmentOperations(&doc, []map[string]any{{"kind": operation.Kind}}, editTestCatalog)
			if err == nil || strings.Contains(err.Error(), "不支持") {
				t.Errorf("kind %s is documented but not handled: %v", operation.Kind, err)
			}
		}
	}
	if len(documented) != 16 {
		t.Fatalf("documented kinds = %d", len(documented))
	}
	doc := editTestDocument()
	if _, err := applyDevelopmentOperations(&doc, []map[string]any{{"kind": "archive"}}, editTestCatalog); err == nil || !strings.Contains(err.Error(), "不支持") {
		t.Fatalf("undocumented kind: %v", err)
	}
}

func TestDevelopmentIssuesNameWhatADraftLacks(t *testing.T) {
	doc := editTestDocument()
	doc.Objective = ""
	doc.Members[2].Configuration.Model = ""
	codes := func(issues []developmentIssue) map[string]string {
		found := map[string]string{}
		for _, issue := range issues {
			found[issue.Code] = issue.Blocks + ":" + issue.Target
		}
		return found
	}
	if got := codes(developmentIssues(doc, editTestCatalog, true)); !reflect.DeepEqual(got, map[string]string{
		"objective_missing": "none:线索跟进团队", "member_model_missing": "trial:核对员", "workflow_missing": "trial:线索跟进团队",
	}) {
		t.Fatalf("issues = %v", got)
	}
	doc = editTestDocument()
	editApply(t, &doc, `[
		{"kind":"flow_add","name":"线索跟进流程","description":"接线索，交付结论","member":"整理员"},
		{"kind":"capability","member":"整理员","capability":"线索转商机","selected":true}
	]`)
	if got := codes(developmentIssues(doc, editTestCatalog, true)); !reflect.DeepEqual(got, map[string]string{"business_completion_missing": "publish:线索跟进流程"}) {
		t.Fatalf("issues = %v", got)
	}
	editApply(t, &doc, `[
		{"kind":"result_protocol","flow":"线索跟进流程","enabled":true},
		{"kind":"business_completion","flow":"线索跟进流程","capabilities":["线索转商机"],"allowNeedsInput":true}
	]`)
	if issues := developmentIssues(doc, editTestCatalog, true); len(issues) != 0 {
		t.Fatalf("issues = %+v", issues)
	}
	// An action that left the catalog is reported; without a snapshot nothing is guessed.
	if got := codes(developmentIssues(doc, editTestCatalog[1:], true)); got["business_action_unavailable"] != "trial:整理员" || got["business_completion_invalid"] != "publish:线索跟进流程" {
		t.Fatalf("issues = %v", got)
	}
	if issues := developmentIssues(doc, nil, false); len(issues) != 0 {
		t.Fatalf("issues without a snapshot = %+v", issues)
	}
}

func TestTeamDevelopmentOperationsRealPG(t *testing.T) {
	ctx := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "ops-workspace", UserID: "developer"})
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES('ops-workspace','ops-workspace','Operations'); INSERT INTO weave_users(id,tenant_id,username,password,role) VALUES('developer','ops-workspace','developer','unused','admin')`); err != nil {
		t.Fatal(err)
	}
	agents := agentcatalog.New(pool)
	lead := registry.AgentRecord{Name: "lead", DisplayName: "负责人", Role: "avatar", Engine: "loom", Model: "fixture-model", GraphType: "standard", Spec: stdlib.AgentSpec{SystemPrompt: "Coordinate"}}
	worker := registry.AgentRecord{Name: "worker", DisplayName: "整理员", Role: "worker", Engine: "loom", Model: "fixture-model", GraphType: "standard", Spec: stdlib.AgentSpec{SystemPrompt: "Sort the material"}}
	for _, rec := range []*registry.AgentRecord{&lead, &worker} {
		if err := agents.Put(ctx, "ops-workspace", rec); err != nil {
			t.Fatal(err)
		}
	}
	created, err := orgstore.NewStore(pool).CreateActiveTeam(ctx, "ops-workspace", org.CreateActiveTeamInput{Name: "operations", Objective: "整理销售线索", LeadAvatarID: lead.ID, Workers: []org.InitialTeamWorker{{WorkerAgentID: worker.ID, Duty: "整理线索", AllowedKinds: []string{"consult"}, DefaultKind: "consult"}}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, _ := json.Marshal(editTestCatalog)
	if _, err = pool.Exec(ctx, `INSERT INTO weave_business_capability_catalog(workspace_id,version,capabilities,entries,fetched_by,fetched_at) VALUES('ops-workspace','1',$1,$2,'developer',now())`, string(catalog), len(editTestCatalog)); err != nil {
		t.Fatal(err)
	}
	server := &Server{Pool: pool, Registry: agents}
	call := func(method string, body any, handler echo.HandlerFunc, params ...string) (int, map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(method, "/", bytes.NewReader(raw)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		c := echo.New().NewContext(req, rr)
		c.SetParamNames("id", "topic")
		c.SetParamValues(append([]string{created.Team.ID}, params...)...)
		c.Set("tenant", "ops-workspace")
		c.Set("user_id", "developer")
		if err := handler(c); err != nil {
			httpError, isHTTP := err.(*echo.HTTPError)
			if !isHTTP {
				t.Fatalf("handler: %v", err)
			}
			return httpError.Code, map[string]any{"error": httpError.Message}
		}
		var decoded map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &decoded); err != nil {
			t.Fatalf("response: %v %s", err, rr.Body.String())
		}
		return rr.Code, decoded
	}
	listOf := func(value any) []any { items, _ := value.([]any); return items }
	apply := func(revision float64, requestID string, dryRun bool, operations string) (int, map[string]any) {
		t.Helper()
		return call("POST", map[string]any{"expected_revision": revision, "request_id": requestID, "dry_run": dryRun, "operations": json.RawMessage(operations)}, server.handleApplyTeamDevelopmentOperations)
	}

	if status, body := apply(1, uuid.NewString(), false, `[{"kind":"team","objective":"x"}]`); status != http.StatusNotFound || body["code"] != "development_not_found" {
		t.Fatalf("before the draft exists: %d %v", status, body)
	}
	status, draft := call("GET", nil, server.handleGetTeamDevelopment)
	if status != http.StatusOK {
		t.Fatalf("draft: %d %v", status, draft)
	}
	revision := draft["revision"].(float64)
	if issues := listOf(draft["issues"]); len(issues) == 0 || issues[len(issues)-1].(map[string]any)["code"] != "workflow_missing" {
		t.Fatalf("draft issues = %v", draft["issues"])
	}

	operations := `[
		{"kind":"member","member":"整理员","duty":"整理线索并转商机","instruction":"逐条核对线索材料"},
		{"kind":"member","member":"负责人","duty":"分派并汇总","instruction":"理解任务并分派"},
		{"kind":"flow_add","name":"线索跟进流程","description":"接销售线索材料，交付是否跟进的结论","member":"整理员"},
		{"kind":"capability","member":"整理员","capability":"线索转商机","selected":true},
		{"kind":"result_protocol","flow":"线索跟进流程","enabled":true},
		{"kind":"business_completion","flow":"线索跟进流程","capabilities":["线索转商机"],"allowNeedsInput":true}
	]`
	// A dry run reports the changes and writes nothing.
	status, preview := apply(revision, uuid.NewString(), true, operations)
	if status != http.StatusOK || preview["saved"] != false || preview["revision"] != revision || len(listOf(preview["changes"])) != 6 || len(listOf(preview["issues"])) != 0 {
		t.Fatalf("preview: %d %v", status, preview)
	}
	if _, unchanged := call("GET", nil, server.handleGetTeamDevelopment); unchanged["revision"] != revision || len(listOf(unchanged["document"].(map[string]any)["workflows"])) != 0 {
		t.Fatalf("a dry run wrote the draft: %v", unchanged["revision"])
	}

	requestID := uuid.NewString()
	status, saved := apply(revision, requestID, false, operations)
	if status != http.StatusOK || saved["saved"] != true || saved["revision"] != revision+1 || len(listOf(saved["issues"])) != 0 {
		t.Fatalf("save: %d %v", status, saved)
	}
	// The same request again returns the first outcome and applies nothing.
	status, replay := apply(revision, requestID, false, operations)
	if status != http.StatusOK || !reflect.DeepEqual(replay["changes"], saved["changes"]) || replay["revision"] != revision+1 {
		t.Fatalf("replay: %d %v", status, replay)
	}
	if status, conflict := apply(revision, requestID, false, `[{"kind":"team","objective":"另一组修改"}]`); status != http.StatusConflict || conflict["code"] != "request_conflict" {
		t.Fatalf("reused request id: %d %v", status, conflict)
	}
	if status, stale := apply(revision, uuid.NewString(), false, `[{"kind":"team","objective":"基于旧修订"}]`); status != http.StatusConflict || stale["code"] != "development_revision_stale" {
		t.Fatalf("stale revision: %d %v", status, stale)
	}
	status, refused := apply(revision+1, uuid.NewString(), false, `[{"kind":"team","objective":"先改目标"},{"kind":"step","flow":"线索跟进流程","step":"没有的步骤","name":"x"}]`)
	if status != http.StatusUnprocessableEntity || refused["code"] != "operation_invalid" || refused["index"] != float64(1) {
		t.Fatalf("refusal: %d %v", status, refused)
	}
	_, current := call("GET", nil, server.handleGetTeamDevelopment)
	document := current["document"].(map[string]any)
	if current["revision"] != revision+1 || document["objective"] != "整理销售线索" || len(listOf(document["workflows"])) != 1 || len(listOf(current["issues"])) != 0 {
		t.Fatalf("draft after the group: %v %v", current["revision"], current["issues"])
	}
	var rows, kinds int
	if err = pool.QueryRow(ctx, `SELECT count(*),COALESCE(sum((kinds->>'member')::int),0) FROM weave_team_development_requests WHERE workspace_id='ops-workspace' AND actor_id='developer' AND revision_before=$1 AND revision_after=$2`, int64(revision), int64(revision)+1).Scan(&rows, &kinds); err != nil || rows != 1 || kinds != 2 {
		t.Fatalf("request ledger: rows=%d kinds=%d err=%v", rows, kinds, err)
	}

	status, list := call("GET", nil, server.handleListDevelopmentOperations)
	if status != http.StatusOK || len(listOf(list["topics"])) != len(developmentOperationTopics) || strings.Contains(rrText(list), `"example"`) {
		t.Fatalf("catalog: %d %v", status, list)
	}
	status, topic := call("GET", nil, server.handleGetDevelopmentOperationTopic, "business_completion")
	if status != http.StatusOK || !strings.Contains(rrText(topic), `allowNeedsInput`) {
		t.Fatalf("topic: %d %v", status, topic)
	}
	if status, missing := call("GET", nil, server.handleGetDevelopmentOperationTopic, "engine"); status != http.StatusNotFound || missing["code"] != "operation_topic_not_found" {
		t.Fatalf("unknown topic: %d %v", status, missing)
	}
}

func rrText(value any) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}
