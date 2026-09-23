package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jinyitao123/weave/internal/app/agentcatalog"
	orgstore "github.com/jinyitao123/weave/internal/app/org"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/testutil"
	org "github.com/jinyitao123/weave/internal/kernel/orgspec"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/labstack/echo/v4"
)

func TestTeamMemberConfigDraftCanBeSavedRepeatedlyWithRevisionCheck(t *testing.T) {
	ctx := context.Background()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	prefix := "member-draft-" + uuid.NewString()[:8]
	workspaceID := "workspace-" + prefix
	agents := agentcatalog.New(pool)
	lead := registry.AgentRecord{Name: prefix + "-lead", DisplayName: "负责人", Role: "avatar"}
	worker := registry.AgentRecord{Name: prefix + "-worker", DisplayName: "执行成员", Role: "worker"}
	for _, agent := range []*registry.AgentRecord{&lead, &worker} {
		if err := agents.Put(ctx, workspaceID, agent); err != nil {
			t.Fatalf("create agent: %v", err)
		}
	}
	created, err := orgstore.NewStore(pool).CreateActiveTeam(ctx, workspaceID, org.CreateActiveTeamInput{
		Name: prefix + "-team", Objective: "完成合同交接", LeadAvatarID: lead.ID,
		Workers: []org.InitialTeamWorker{{WorkerAgentID: worker.ID, Duty: "整理材料", AllowedKinds: []string{"dispatch"}, DefaultKind: "dispatch"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Pool: pool, Registry: agents}
	save := func(revision int, duty string) (int, teamMemberConfigDraftResponse) {
		t.Helper()
		raw, _ := json.Marshal(saveTeamMemberConfigDraftRequest{
			Revision:      revision,
			Configuration: teamMemberAgentConfiguration{DisplayName: "执行成员", Role: "worker", Engine: "loom", Skills: []teamMemberInlineSkill{{Name: "合同复核", Description: "检查合同条款", Body: "逐条检查合同并列出风险。"}}, BusinessCapabilityIDs: []string{"forge:action:sales_contract.ContractSubmit"}, BusinessCapabilityBindings: []frozen.BusinessCapabilityBinding{{CapabilityID: "forge:action:sales_contract.ContractSubmit", Parameters: []frozen.BusinessCapabilityParameterBinding{{Name: "material_file_id", Source: frozen.BusinessSourceMaterialID}}}}},
			Relationship:  teamMemberRelationshipDraft{Duty: duty, AllowedKinds: []string{"dispatch"}, DefaultKind: "dispatch", Enabled: true},
		})
		request := httptest.NewRequest(http.MethodPut, "/", bytes.NewReader(raw)).WithContext(ctx)
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		echoContext := echo.New().NewContext(request, recorder)
		echoContext.SetParamNames("id", "agent")
		echoContext.SetParamValues(created.Team.ID, worker.ID)
		echoContext.Set("tenant", workspaceID)
		echoContext.Set("user_id", "developer")
		if err := server.handlePutTeamMemberConfigDraft(echoContext); err != nil {
			t.Fatal(err)
		}
		var response teamMemberConfigDraftResponse
		if recorder.Code == http.StatusOK {
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
		}
		return recorder.Code, response
	}

	if status, result := save(0, "第一次保存"); status != http.StatusOK || result.Revision != 1 {
		t.Fatalf("first save status=%d revision=%d", status, result.Revision)
	}
	if status, result := save(1, "第二次保存"); status != http.StatusOK || result.Revision != 2 || result.Relationship.Duty != "第二次保存" {
		t.Fatalf("second save status=%d revision=%d duty=%q", status, result.Revision, result.Relationship.Duty)
	}
	if status, _ := save(1, "过期覆盖"); status != http.StatusConflict {
		t.Fatalf("stale save status=%d want %d", status, http.StatusConflict)
	}

	applyBody, _ := json.Marshal(applyTeamMemberConfigDraftRequest{Revision: 2})
	applyRequest := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(applyBody)).WithContext(ctx)
	applyRequest.Header.Set("Content-Type", "application/json")
	applyRecorder := httptest.NewRecorder()
	applyContext := echo.New().NewContext(applyRequest, applyRecorder)
	applyContext.SetParamNames("id", "agent")
	applyContext.SetParamValues(created.Team.ID, worker.ID)
	applyContext.Set("tenant", workspaceID)
	applyContext.Set("user_id", "developer")
	if err := server.handleApplyTeamMemberConfigDraft(applyContext); err != nil {
		t.Fatal(err)
	}
	if applyRecorder.Code != http.StatusOK {
		t.Fatalf("apply status=%d body=%s", applyRecorder.Code, applyRecorder.Body.String())
	}
	updated, err := agents.Get(ctx, workspaceID, worker.Name)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Version != 2 {
		t.Fatalf("agent version=%d want 2", updated.Version)
	}
	if len(updated.Spec.Skills) != 1 || updated.Spec.Skills[0].Name != "合同复核" {
		t.Fatalf("applied skills=%+v", updated.Spec.Skills)
	}
	if len(updated.BusinessCapabilityIDs) != 1 || updated.BusinessCapabilityIDs[0] != "forge:action:sales_contract.ContractSubmit" {
		t.Fatalf("applied business capabilities=%+v", updated.BusinessCapabilityIDs)
	}
	if len(updated.BusinessCapabilityBindings) != 1 || updated.BusinessCapabilityBindings[0].Parameters[0].Source != frozen.BusinessSourceMaterialID {
		t.Fatalf("applied business capability bindings=%+v", updated.BusinessCapabilityBindings)
	}
	relation, err := agentcatalog.NewTeamWorkerRepository(pool).Get(ctx, workspaceID, created.Team.ID, worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	if relation.Duty != "第二次保存" {
		t.Fatalf("applied duty=%q", relation.Duty)
	}
	var remaining, appliedRevision, appliedBaseVersion int
	if err := pool.QueryRow(ctx, `SELECT count(*), max(revision), max(base_agent_version) FROM weave_team_member_config_drafts WHERE workspace_id=$1 AND team_id=$2 AND agent_id=$3`, workspaceID, created.Team.ID, worker.ID).Scan(&remaining, &appliedRevision, &appliedBaseVersion); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 || appliedRevision != 0 || appliedBaseVersion != 2 {
		t.Fatalf("applied baseline rows=%d revision=%d base=%d", remaining, appliedRevision, appliedBaseVersion)
	}
	if status, result := save(0, "应用后再次编辑"); status != http.StatusOK || result.Revision != 1 {
		t.Fatalf("edit applied baseline status=%d revision=%d", status, result.Revision)
	}

	leadConfiguration := teamMemberAgentConfiguration{DisplayName: "负责人", Role: "avatar", Engine: "codex", RuntimeID: "local-runtime", SystemPrompt: "先理解目标，再协调成员并核对结果。"}
	leadRelationship := teamMemberRelationshipDraft{Duty: "负责合同交接的组织与最终验收。", Enabled: true}
	configurationJSON, _ := json.Marshal(leadConfiguration)
	relationshipJSON, _ := json.Marshal(leadRelationship)
	if _, err := pool.Exec(ctx, `INSERT INTO weave_team_member_config_drafts(workspace_id,team_id,agent_id,base_agent_version,revision,configuration,relationship,updated_by) VALUES($1,$2,$3,$4,1,$5::jsonb,$6::jsonb,'developer') ON CONFLICT(workspace_id,team_id,agent_id) DO UPDATE SET revision=1,configuration=EXCLUDED.configuration,relationship=EXCLUDED.relationship`, workspaceID, created.Team.ID, lead.ID, lead.Version, string(configurationJSON), string(relationshipJSON)); err != nil {
		t.Fatal(err)
	}
	if err := server.applyTeamMemberConfigDraft(ctx, workspaceID, created.Team.ID, lead.ID, "developer", 1); err != nil {
		t.Fatal(err)
	}
	updatedLead, err := agents.Get(ctx, workspaceID, lead.Name)
	if err != nil {
		t.Fatal(err)
	}
	if updatedLead.Spec.Identity.Core != leadRelationship.Duty || updatedLead.Spec.Identity.Raw != leadConfiguration.SystemPrompt || updatedLead.Spec.SystemPrompt != leadConfiguration.SystemPrompt {
		t.Fatalf("lead identity=%+v prompt=%q", updatedLead.Spec.Identity, updatedLead.Spec.SystemPrompt)
	}
}
