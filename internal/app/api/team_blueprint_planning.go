package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/app/metateam"
	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/build/teamforge"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/labstack/echo/v4"
)

const teamBlueprintPlanToolName = "tf_blueprint_plan"

const teamBlueprintPlanErrorBudget = 3
const teamDeclarativePlanErrorBudget = 6

var compilerCreateIdentifierPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

var teamBlueprintPlanInputSchema = json.RawMessage(`{
  "type":"object",
  "additionalProperties":false,
  "oneOf":[
    {"required":["blueprint"]},
    {"required":["compact_blueprint"]},
    {"required":["arguments"]}
  ],
  "properties":{
    "build_run_id":{"type":"string"},
    "arguments":{"type":"object","description":"Compatibility envelope. If present, its content must be the normal tf_blueprint_plan input object."},
    "compact_blueprint":{
      "type":"object",
      "additionalProperties":false,
      "required":["members","lead_ref","template","template_parameters"],
      "description":"Preferred compact input. The platform expands schema_version, mode, team_id/new_team_name, and revision_policy defaults from the frozen BuildRun. Every member must explicitly choose management_mode and execution_policy. In optimize, use managed when executor or member configuration must change; preserve_existing forbids Agent mutation and cannot satisfy a requested executor change.",
      "properties":{
        "purpose":{"type":"string","description":"Team design purpose. In optimize mode, provide a durable capability goal when the current objective contains task-specific pollution; omit only when preserving the current objective is intended."},
        "members":{
          "type":"array","minItems":1,
          "items":{
            "type":"object","additionalProperties":false,
            "required":["name","display_name","role","management_mode","responsibilities","capabilities","execution_policy"],
            "properties":{
              "stable_ref":{"type":"string","pattern":"^[a-z][a-z0-9_-]*$","description":"Optional; optimize defaults to name."},
              "name":{"type":"string"},
		      "display_name":{"type":"string","description":"Chinese user-facing member name; name remains the URL-safe system identifier."},
              "role":{"type":"string","enum":["avatar","worker"]},
              "management_mode":{"type":"string","enum":["managed","preserve_existing"],"description":"Create requires managed. Optimize must use managed for any Agent update, including loom-to-CLI; preserve_existing guarantees no Agent mutation."},
              "responsibilities":{"type":"array","minItems":1,"items":{"type":"string"}},
              "capabilities":{"type":"array","minItems":1,"items":{"type":"string"}},
			  "model_ref":{"type":"string","description":"Optional in compact input. Create normalizes an omitted or engine-incompatible value to an enabled compatible provider model, or leaves it empty to use the authenticated runtime/platform default when no compatible provider exists. Optimize omission preserves the exact model frozen in the baseline AgentVersion."},
              "execution_policy":{
                "type":"object","additionalProperties":false,
                "required":["engine_class","execution_mode"],
                "description":"Explicit per-member executor. For a local Codex worker use engine_class=cli, execution_mode=runtime, engine=codex, and the exact runtime_ref returned by platform capabilities.",
                "properties":{
                  "engine_class":{"type":"string","enum":["standard","cli"]},
                  "execution_mode":{"type":"string","enum":["toolloop","runtime","internal_graph"]},
                  "engine":{"type":"string","enum":["codex","claude","opencode"]},
                  "runtime_ref":{"type":"string"},
                  "internal_graph_ref":{"type":"string"}
                }
              }
            }
          }
        },
        "lead_ref":{"type":"string"},
        "template":{"type":"string","enum":["delivery_rework_loop","parallel_review","creative_critique_loop","research_synthesis"]},
        "template_parameters":{
          "type":"object","additionalProperties":false,
          "required":["lead_instruction","result_requirements"],
          "properties":{
            "lead_instruction":{"type":"string"},
            "primary_ref":{"type":"string"},
            "reviewer_ref":{"type":"string"},
            "parallel_worker_refs":{"type":"array","items":{"type":"string"}},
            "finalizer_ref":{"type":"string"},
            "max_iterations":{"type":"integer","minimum":1},
            "result_requirements":{"type":"object","minProperties":1,"propertyNames":{"pattern":"^[a-z][a-z0-9_-]*$"},"additionalProperties":{"type":"string"}}
          }
        },
        "allowed_patch_paths":{"type":"array","items":{"type":"string"}},
        "max_revisions":{"type":"integer","minimum":1,"maximum":3}
      }
    },
    "blueprint":{
      "type":"object",
      "additionalProperties":false,
      "required":["schema_version","mode","purpose","members","lead_ref","workflow","revision_policy"],
      "properties":{
        "schema_version":{"const":1},
        "mode":{"type":"string","enum":["create","optimize"]},
        "team_id":{"type":"string"},
        "new_team_name":{"type":"string"},
        "purpose":{"type":"string"},
        "members":{
          "type":"array","minItems":1,
          "items":{
            "type":"object","additionalProperties":false,
            "required":["stable_ref","name","display_name","role","management_mode","responsibilities","capabilities","execution_policy"],
            "properties":{
              "stable_ref":{"type":"string","pattern":"^[a-z][a-z0-9_-]*$","description":"Stable business identity such as dev-coder; never use an asset UUID. In optimize mode use the existing agent name as stable_ref."},
              "name":{"type":"string"},
		      "display_name":{"type":"string","description":"Chinese user-facing member name; name remains the URL-safe system identifier."},
              "role":{"type":"string","enum":["avatar","worker"]},
              "management_mode":{"type":"string","enum":["managed","preserve_existing"]},
              "responsibilities":{"type":"array","minItems":1,"items":{"type":"string"}},
              "capabilities":{"type":"array","minItems":1,"items":{"type":"string"}},
              "model_ref":{"type":"string"},
              "execution_policy":{
                "type":"object","additionalProperties":false,
                "required":["engine_class","execution_mode"],
                "properties":{
                  "engine_class":{"type":"string","enum":["standard","cli"]},
                  "execution_mode":{"type":"string","enum":["toolloop","runtime","internal_graph"]},
                  "engine":{"type":"string","enum":["codex","claude","opencode"]},
                  "runtime_ref":{"type":"string"},
                  "internal_graph_ref":{"type":"string"}
                }
              }
            }
          }
        },
        "lead_ref":{"type":"string"},
        "workflow":{
          "type":"object","additionalProperties":false,
          "required":["mode","template","template_parameters"],
          "properties":{
            "mode":{"const":"template"},
            "template":{"type":"string","enum":["delivery_rework_loop","parallel_review","creative_critique_loop","research_synthesis"]},
            "template_parameters":{
              "type":"object","additionalProperties":false,
              "required":["lead_instruction","result_requirements"],
              "properties":{
                "lead_instruction":{"type":"string"},
                "primary_ref":{"type":"string","description":"Must equal one members[].stable_ref."},
                "reviewer_ref":{"type":"string","description":"Must equal one members[].stable_ref."},
                "parallel_worker_refs":{"type":"array","items":{"type":"string"}},
                "finalizer_ref":{"type":"string"},
                "max_iterations":{"type":"integer","minimum":1},
                "result_requirements":{"type":"object","minProperties":1,"propertyNames":{"pattern":"^[a-z][a-z0-9_-]*$"},"additionalProperties":{"type":"string"},"description":"Every key must equal the stable_ref of a member actually called by the template; do not use node labels such as primary, reviewer, or deliver."}
              }
            }
          }
        },
        "revision_policy":{
          "type":"object","additionalProperties":false,
          "required":["max_revisions","allowed_patch_paths"],
          "properties":{
            "max_revisions":{"type":"integer","minimum":1,"maximum":3},
            "allowed_patch_paths":{"type":"array","minItems":1,"items":{"type":"string","pattern":"^/(purpose|lead_ref|members/[a-z][a-z0-9_-]*/(responsibilities|capabilities|model_ref|execution_policy)|workflow/template_parameters/(lead_instruction|primary_ref|reviewer_ref|parallel_worker_refs|finalizer_ref|max_iterations|result_requirements/[a-z][a-z0-9_-]*))$","description":"Canonical JSON Pointer to one exact mutable business-quality leaf, for example /purpose, /members/dev-coder/responsibilities, or /workflow/template_parameters/result_requirements/dev-verifier. Container names, asset refs, wildcards, management_mode, revision_policy, workflow.mode, and workflow.template are forbidden."}}
          }
        }
      }
    }
  }
}`)

type planTeamBlueprintRequest struct {
	Blueprint teambuild.TeamBlueprintV1 `json:"blueprint"`
}

type compactTeamBlueprintPlanV1 struct {
	Purpose            string                                          `json:"purpose,omitempty"`
	Members            []compactBlueprintMemberV1                      `json:"members"`
	LeadRef            string                                          `json:"lead_ref"`
	Template           string                                          `json:"template"`
	TemplateParameters teambuild.BlueprintWorkflowTemplateParametersV1 `json:"template_parameters"`
	AllowedPatchPaths  []string                                        `json:"allowed_patch_paths,omitempty"`
	MaxRevisions       int                                             `json:"max_revisions,omitempty"`
}

type compactBlueprintMemberV1 struct {
	StableRef        string                               `json:"stable_ref,omitempty"`
	Name             string                               `json:"name"`
	DisplayName      string                               `json:"display_name"`
	Role             string                               `json:"role"`
	ManagementMode   string                               `json:"management_mode"`
	Responsibilities []string                             `json:"responsibilities"`
	Capabilities     []string                             `json:"capabilities"`
	ModelRef         string                               `json:"model_ref,omitempty"`
	ExecutionPolicy  teambuild.BlueprintExecutionPolicyV1 `json:"execution_policy"`
}

type blueprintPlanOperation struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Target string `json:"target"`
}

type blueprintPlanResult struct {
	RevisionNo          int                              `json:"revision_no"`
	Existing            bool                             `json:"existing"`
	BlueprintHash       string                           `json:"blueprint_hash"`
	ChangeSetHash       string                           `json:"change_set_hash"`
	DeclarativeSpecHash string                           `json:"declarative_spec_hash,omitempty"`
	RevisionToken       teambuild.BlueprintRevisionToken `json:"revision_token"`
	BaselineHash        string                           `json:"baseline_hash"`
	Operations          []blueprintPlanOperation         `json:"operations"`
}

type blueprintPlanningError struct {
	Code     string
	Message  string
	Problems []teambuild.BlueprintProblem
}

func (e *blueprintPlanningError) Error() string {
	if e == nil {
		return "blueprint planning failed"
	}
	return e.Message
}

// planTeamBlueprint is the only planning entry point that turns model-authored
// desired state into executable work. The baseline and ChangeSet are always
// produced by the server; neither is accepted from the caller.
func (s *Server) planTeamBlueprint(
	ctx context.Context,
	workspaceID, buildRunID string,
	blueprint teambuild.TeamBlueprintV1,
) (blueprintPlanResult, error) {
	if s == nil || s.TeamBuild == nil {
		return blueprintPlanResult{}, &blueprintPlanningError{
			Code: "team_build_unavailable", Message: "team build store unavailable",
		}
	}
	run, err := s.TeamBuild.GetBuildRun(ctx, workspaceID, buildRunID)
	if err != nil {
		return blueprintPlanResult{}, err
	}
	if run.Status != teambuild.StatusPlanning {
		return blueprintPlanResult{}, teambuild.ErrBuildRunNotPlanning
	}
	if err := validateBlueprintAgainstRun(run, blueprint); err != nil {
		return blueprintPlanResult{}, err
	}

	blueprintHash, err := blueprint.BlueprintHash()
	if err != nil {
		return blueprintPlanResult{}, planningValidationError(err)
	}
	latest, latestErr := s.TeamBuild.GetLatestBlueprintRevision(ctx, workspaceID, buildRunID)
	if latestErr == nil && latest.BlueprintHash == blueprintHash {
		return blueprintPlanResultFromRevision(latest, true)
	}
	if latestErr != nil && !errors.Is(latestErr, teambuild.ErrBuildRunNotFound) {
		return blueprintPlanResult{}, latestErr
	}
	revisionNo := 1
	if latestErr == nil {
		revisionNo = latest.RevisionNo + 1
	}
	if revisionNo > blueprint.RevisionPolicy.MaxRevisions {
		return blueprintPlanResult{}, &blueprintPlanningError{
			Code:    "blueprint_revision_limit_reached",
			Message: "blueprint revision policy does not allow another revision",
		}
	}

	s.TeamBuild.SetBaselineSources(s.OrgStore, s.Registry, s.Workflow, s.WorkflowArtifacts)
	preview, err := s.TeamBuild.PreviewCompilerBaseline(ctx, workspaceID, buildRunID)
	if err != nil {
		return blueprintPlanResult{}, err
	}
	baseline, err := projectCompilerBaseline(run, blueprint, preview)
	if err != nil {
		return blueprintPlanResult{}, err
	}
	changeSet, err := teamforge.CompileChangeSetV1(baseline, blueprint)
	if err != nil {
		return blueprintPlanResult{}, &blueprintPlanningError{
			Code: "blueprint_compile_failed", Message: err.Error(),
		}
	}
	blueprintJSON, err := json.Marshal(blueprint)
	if err != nil {
		return blueprintPlanResult{}, fmt.Errorf("marshal team blueprint: %w", err)
	}
	changeSetJSON, err := changeSet.CanonicalBytes()
	if err != nil {
		return blueprintPlanResult{}, fmt.Errorf("marshal compiled change set: %w", err)
	}
	changeSetHash, err := changeSet.CanonicalHash()
	if err != nil {
		return blueprintPlanResult{}, fmt.Errorf("hash compiled change set: %w", err)
	}
	revision, err := s.TeamBuild.PersistCompilerAuthorizationBundle(ctx, workspaceID, buildRunID, teambuild.CompilerAuthorizationBundle{
		RevisionNo:             revisionNo,
		BlueprintJSON:          blueprintJSON,
		BlueprintHash:          blueprintHash,
		ChangeSetJSON:          changeSetJSON,
		ChangeSetHash:          changeSetHash,
		BaselineHash:           preview.BaselineHash,
		BaselineCapturedAt:     preview.CapturedAt,
		EvaluationContractHash: run.ContractHash,
	})
	if errors.Is(err, teambuild.ErrBlueprintRevisionNotAppendSafe) {
		// Concurrent identical submission is idempotent. A different revision
		// remains a conflict and must be replanned from the new latest state.
		current, getErr := s.TeamBuild.GetLatestBlueprintRevision(ctx, workspaceID, buildRunID)
		if getErr == nil && current.BlueprintHash == blueprintHash {
			return blueprintPlanResultFromRevision(current, true)
		}
	}
	if err != nil {
		return blueprintPlanResult{}, err
	}
	return blueprintPlanResultFromRevision(revision, false)
}

func validateBlueprintAgainstRun(run teambuild.TeamBuildRun, blueprint teambuild.TeamBlueprintV1) error {
	if err := teambuild.ValidateTeamBlueprintV1(blueprint); err != nil {
		return planningValidationError(err)
	}
	if blueprint.Workflow.Mode == teambuild.BlueprintWorkflowCustom {
		return &blueprintPlanningError{
			Code:    "template_gap_confirmation_unavailable",
			Message: "custom workflow planning requires an independent administrator authority API",
		}
	}
	problems := make([]teambuild.BlueprintProblem, 0)
	add := func(path, code, message string) {
		problems = append(problems, teambuild.BlueprintProblem{Path: path, Code: code, Message: message})
	}
	if strings.TrimSpace(blueprint.Mode) != strings.TrimSpace(run.Mode) {
		add("/mode", "blueprint_run_mode_mismatch", "blueprint mode must match the build run")
	}
	switch run.Mode {
	case teambuild.ModeCreate:
		if strings.TrimSpace(blueprint.NewTeamName) != strings.TrimSpace(run.Brief.NewTeamName) {
			add("/new_team_name", "blueprint_run_identity_mismatch", "new_team_name must match the frozen build brief")
		}
		prefix := run.Brief.AllowedAssets.NamePrefix
		validateCreateTarget := func(path, value string) {
			if len(value) > 64 || !compilerCreateIdentifierPattern.MatchString(value) {
				add(path, "blueprint_create_identifier_invalid", "create identifiers must match ^[a-z0-9][a-z0-9_-]*$ and be at most 64 characters")
				return
			}
			if prefix == "" || !strings.HasPrefix(value, prefix) {
				add(path, "blueprint_create_identifier_out_of_scope", "create identifiers must use the frozen allowed_assets name_prefix")
			}
		}
		validateCreateTarget("/new_team_name", strings.TrimSpace(blueprint.NewTeamName))
		for index, member := range blueprint.Members {
			validateCreateTarget(fmt.Sprintf("/members/%d/name", index), strings.TrimSpace(member.Name))
		}
		validateCreateTarget("/workflow", strings.TrimSpace(blueprint.NewTeamName)+"-workflow")
	case teambuild.ModeOptimize:
		if strings.TrimSpace(blueprint.TeamID) != strings.TrimSpace(run.Brief.TeamID) {
			add("/team_id", "blueprint_run_identity_mismatch", "team_id must match the frozen build brief")
		}
	}
	if len(problems) != 0 {
		sort.SliceStable(problems, func(i, j int) bool { return problems[i].Path < problems[j].Path })
		return &blueprintPlanningError{Code: "blueprint_validation_failed", Message: "team blueprint does not match the frozen build run", Problems: problems}
	}
	return nil
}

func planningValidationError(err error) error {
	var validation *teambuild.BlueprintValidationError
	if errors.As(err, &validation) {
		return &blueprintPlanningError{
			Code: "blueprint_validation_failed", Message: validation.Error(),
			Problems: append([]teambuild.BlueprintProblem(nil), validation.Problems...),
		}
	}
	return err
}

func projectCompilerBaseline(
	run teambuild.TeamBuildRun,
	blueprint teambuild.TeamBlueprintV1,
	preview teambuild.CompilerBaselinePreview,
) (teamforge.TeamBuildBaselineV1, error) {
	if run.Mode == teambuild.ModeCreate {
		if preview.Snapshot != nil || preview.CapturedAt != nil || preview.BaselineHash != teamforge.EmptyCreateBaselineHashV1 {
			return teamforge.TeamBuildBaselineV1{}, &blueprintPlanningError{Code: "compiler_baseline_invalid", Message: "create baseline is not the canonical empty baseline"}
		}
		return teamforge.EmptyCreateBaselineV1(), nil
	}
	if preview.Snapshot == nil || preview.CapturedAt == nil || preview.BaselineHash == "" || preview.Snapshot.ContentHash != preview.BaselineHash {
		return teamforge.TeamBuildBaselineV1{}, &blueprintPlanningError{Code: "compiler_baseline_invalid", Message: "optimize baseline snapshot is unavailable or unbound"}
	}
	snapshot := preview.Snapshot
	if snapshot.Team.TeamID != blueprint.TeamID {
		return teamforge.TeamBuildBaselineV1{}, &blueprintPlanningError{Code: "compiler_baseline_identity_mismatch", Message: "baseline team does not match blueprint team_id"}
	}
	pinsByName := make(map[string]teambuild.BaselineAgentPin, len(snapshot.AgentPins))
	for _, pin := range snapshot.AgentPins {
		if _, duplicate := pinsByName[pin.Name]; duplicate {
			return teamforge.TeamBuildBaselineV1{}, &blueprintPlanningError{Code: "compiler_baseline_ambiguous", Message: "baseline has duplicate agent names"}
		}
		pinsByName[pin.Name] = pin
	}
	stableByAgentID := make(map[string]string, len(blueprint.Members))
	members := make([]teamforge.BaselineMemberV1, 0, len(blueprint.Members))
	for _, member := range blueprint.Members {
		pin, ok := pinsByName[member.Name]
		if !ok {
			return teamforge.TeamBuildBaselineV1{}, &blueprintPlanningError{Code: "compiler_baseline_member_missing", Message: fmt.Sprintf("baseline has no exact agent pin for %q", member.Name)}
		}
		if _, duplicate := stableByAgentID[pin.AgentID]; duplicate {
			return teamforge.TeamBuildBaselineV1{}, &blueprintPlanningError{Code: "compiler_baseline_ambiguous", Message: "multiple blueprint members resolve to one baseline agent"}
		}
		current := member
		policy, err := pin.ExecutionPolicy()
		if err != nil {
			return teamforge.TeamBuildBaselineV1{}, &blueprintPlanningError{Code: "compiler_baseline_agent_invalid", Message: fmt.Sprintf("baseline agent %q executor is invalid: %v", pin.AgentID, err)}
		}
		current.ExecutionPolicy = policy
		if model := strings.TrimSpace(pin.Model); model != "" {
			current.ModelRef = model
		}
		stableByAgentID[pin.AgentID] = member.StableRef
		members = append(members, teamforge.BaselineMemberV1{
			StableRef: member.StableRef, Target: pin.AgentID, Version: int64(pin.Version), Desired: current,
		})
	}
	if len(stableByAgentID) != len(snapshot.AgentPins) {
		return teamforge.TeamBuildBaselineV1{}, &blueprintPlanningError{Code: "compiler_baseline_scope_mismatch", Message: "blueprint members do not exactly cover the frozen agent pins"}
	}
	leadRef, ok := stableByAgentID[snapshot.Team.LeadAvatarID]
	if !ok || leadRef != blueprint.LeadRef {
		return teamforge.TeamBuildBaselineV1{}, &blueprintPlanningError{Code: "compiler_baseline_lead_mismatch", Message: "blueprint lead_ref does not resolve to the frozen lead avatar"}
	}
	rosterRefs := make([]string, 0, len(snapshot.Roster))
	for _, entry := range snapshot.Roster {
		if !entry.Enabled {
			continue
		}
		stableRef, exists := stableByAgentID[entry.AgentID]
		if !exists {
			return teamforge.TeamBuildBaselineV1{}, &blueprintPlanningError{Code: "compiler_baseline_roster_mismatch", Message: "frozen roster contains an agent outside the blueprint"}
		}
		rosterRefs = append(rosterRefs, stableRef)
	}
	sort.Strings(rosterRefs)
	if len(rosterRefs) != len(blueprint.Members) {
		return teamforge.TeamBuildBaselineV1{}, &blueprintPlanningError{Code: "compiler_baseline_roster_mismatch", Message: "blueprint members do not exactly cover the enabled frozen roster"}
	}
	rosterInputHash, err := teamforge.RosterContractHashFromSnapshotV1(snapshot.Roster, stableByAgentID)
	if err != nil {
		return teamforge.TeamBuildBaselineV1{}, &blueprintPlanningError{Code: "compiler_baseline_roster_invalid", Message: err.Error()}
	}
	published := make([]teambuild.BaselineWorkflowRef, 0, len(snapshot.Workflows))
	for _, workflow := range snapshot.Workflows {
		if workflow.Published != nil {
			published = append(published, workflow)
		}
	}
	if len(published) > 1 {
		return teamforge.TeamBuildBaselineV1{}, &blueprintPlanningError{Code: "compiler_baseline_workflow_ambiguous", Message: "optimize planning requires at most one published workflow in the frozen scope"}
	}
	var workflowBaseline *teamforge.BaselineWorkflowV1
	if len(published) == 1 {
		workflow := published[0]
		workflowBaseline = &teamforge.BaselineWorkflowV1{Target: workflow.WorkflowID, Version: int64(workflow.Published.Version), InputHash: workflow.Published.ContentHash}
	}
	return teamforge.TeamBuildBaselineV1{
		SchemaVersion:      teamforge.ChangeSetSchemaVersionV1,
		SourceSnapshotHash: snapshot.ContentHash,
		Mode:               teambuild.ModeOptimize,
		TeamID:             snapshot.Team.TeamID,
		Team:               &teamforge.BaselineTeamV1{Target: snapshot.Team.TeamID, Version: 1, Purpose: snapshot.Team.Objective, LeadRef: leadRef},
		Members:            members,
		Roster:             &teamforge.BaselineRosterV1{Version: 1, LeadRef: leadRef, MemberRefs: rosterRefs, InputHash: rosterInputHash},
		Workflow:           workflowBaseline,
	}, nil
}

func blueprintPlanResultFromRevision(revision teambuild.BlueprintRevision, existing bool) (blueprintPlanResult, error) {
	var changeSet teamforge.ChangeSetV1
	if err := json.Unmarshal(revision.ChangeSetJSON, &changeSet); err != nil {
		return blueprintPlanResult{}, fmt.Errorf("decode persisted change set projection: %w", err)
	}
	operations := make([]blueprintPlanOperation, 0, len(changeSet.Operations))
	for _, operation := range changeSet.Operations {
		operations = append(operations, blueprintPlanOperation{Type: string(operation.Type), ID: operation.OperationID, Target: operation.Target})
	}
	return blueprintPlanResult{
		RevisionNo: revision.RevisionNo, Existing: existing,
		BlueprintHash: revision.BlueprintHash, ChangeSetHash: revision.ChangeSetHash,
		RevisionToken: blueprintRevisionTokenFromRevision(revision),
		BaselineHash:  revision.BaselineHash, Operations: operations,
	}, nil
}

func (s *Server) handlePlanTeamBlueprint(c echo.Context) error {
	var request planTeamBlueprintRequest
	if err := decodeWorkflowBody(c, &request); err != nil {
		return workflowSchemaError(c)
	}
	result, err := s.planTeamBlueprint(c.Request().Context(), getTenant(c), c.Param("id"), request.Blueprint)
	if err != nil {
		return mapBlueprintPlanningError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}

func mapBlueprintPlanningError(c echo.Context, err error) error {
	var planning *blueprintPlanningError
	if errors.As(err, &planning) {
		status := http.StatusUnprocessableEntity
		if planning.Code == "team_build_unavailable" {
			status = http.StatusServiceUnavailable
		}
		body := map[string]any{"error": planning.Message, "code": planning.Code}
		if len(planning.Problems) != 0 {
			body["problems"] = planning.Problems
		}
		return c.JSON(status, body)
	}
	return mapTeamBuildRunControlError(c, err)
}

type teamBlueprintPlanningDispatcher struct {
	server         *Server
	workspaceID    string
	conversationID string
	buildRunID     string
	agent          string
	audit          teamforge.AuditRecorder
	mu             sync.Mutex
	errorCounts    map[string]int
}

func (d *teamBlueprintPlanningDispatcher) ListTools(context.Context) ([]contract.ToolDef, error) {
	tools := []contract.ToolDef{
		{
			Name:        teamBlueprintPlanToolName,
			Description: "提交完整 TeamBlueprint；平台读取冻结 baseline、确定性编译并持久化 ChangeSet。optimize 可重编团队目标、成员、roster/workflow；成员执行器或配置需要变化时 management_mode 必须为 managed，preserve_existing 明确禁止 Agent mutation，不能用于 loom→CLI。purpose 应表达长期团队能力目标，禁止写入具体业务任务。lead_ref 必须保留现有 lead。optimize 的 stable_ref 使用现有 Agent 名称而非 UUID；所有成员引用和 result_requirements 键都使用 stable_ref。allowed_patch_paths 必须是以 / 开头、含具体 stable_ref 的精确 JSON Pointer。不得提交 ChangeSet、baseline 或 authority。" + teamforge.BlueprintTemplateParameterGuidance,
			InputSchema: teamBlueprintPlanInputSchema,
		},
		{
			Name:        teamDeclarativeWorkflowPlanToolName,
			Description: "提交 declarative_v1 工作流。固定 N 并行→join→主执行者/评审者有限返工→deliver 的形态必须优先提交 kind=parallel_join_review_loop 的 pattern；平台确定性生成节点、边、ValueRef、iteration 与 latch，禁止为该形态手写 spec。只有其他受支持拓扑才提交完整 spec。平台严格解码并封闭字段，通过已冻结 TeamBlueprint roster 将 worker stable_ref 绑定到精确 AgentVersion（create 绑定 ChangeSet 将创建的 v1，optimize 绑定冻结的现有版本），运行 machine validator，再把冻结 spec 与 GraphDefinition 编译进 ChangeSet。生产节点 instruction/result_requirement 必须描述长期通用能力并服从 run_input；EvaluationContract 的候选题目、样例实体、数量、字数和测试专用约束不得硬编码进生产 spec。dispatch 仅用于 parallel 的直接分支 worker，且只能以一条 join 出边结束；顺序或 loop body 中的 worker 必须用 consult，并以 success 出边继续，loop latch 则以 back 回到 loop。loop 禁止声明 output；平台派生 {iteration_count, limit_reached, latch_result}，循环外必须从 loop 节点读取 /latch_result 或 /latch_result/<latch 必填字段>，不得直接读取 body 节点。串行主图允许 kind=human 的 wait，必须填写 resume_schema 与 task，带 timeout_seconds 时必须有 timeout 路由；fanout 分支内禁止 human wait。仍不支持 handoff、嵌套 loop/parallel 或动态 fanout。不得填写 agent_id/agent_version。",
			InputSchema: teamDeclarativeWorkflowPlanInputSchema,
		},
	}
	if d.agent == metateam.GraphDesignerName {
		return tools[1:], nil
	}
	return tools, nil
}

func (d *teamBlueprintPlanningDispatcher) Dispatch(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	result := &contract.ToolResult{CallID: call.ID, ToolName: call.Name}
	defer func() {
		d.recordAudit(ctx, call, result, nil)
		if result != nil && result.IsError {
			if d.errorCounts == nil {
				d.errorCounts = make(map[string]int)
			}
			d.errorCounts[call.Name]++
		}
	}()
	if (call.Name != teamBlueprintPlanToolName && call.Name != teamDeclarativeWorkflowPlanToolName) ||
		(d.agent == metateam.GraphDesignerName && call.Name != teamDeclarativeWorkflowPlanToolName) {
		result.IsError = true
		result.Content = `{"code":"unknown_tool","error":"unknown tool"}`
		return result, nil
	}
	if d.errorCounts[call.Name] >= teamPlanningErrorBudget(call.Name) {
		d.blockRunAfterPlanningBudget(ctx)
		result.IsError = true
		result.Content = mustJSON(map[string]any{
			"code": "blueprint_plan_budget_exhausted", "error": "planning submission failed too many times in this turn",
			"guidance": "停止重试规划提交工具。本次 BuildRun 已锁定。请向用户汇报受阻原因与修正方案，由用户决定是否重新规划。",
		})
		return result, nil
	}
	if call.Name == teamDeclarativeWorkflowPlanToolName {
		return d.dispatchDeclarativeWorkflowPlan(ctx, call, result)
	}
	request, err := decodeTeamBlueprintPlanToolRequest(call.Args)
	if err != nil {
		result.IsError = true
		code := "blueprint_request_invalid"
		guidance := "Submit exactly one compact JSON object matching the tool schema."
		if errors.Is(err, io.ErrUnexpectedEOF) || strings.Contains(err.Error(), "unexpected EOF") {
			code = "blueprint_request_truncated"
			guidance = "The TeamBlueprint tool arguments ended before valid JSON completed. Retry once with a compact blueprint: keep responsibilities, capabilities, lead_instruction, and result_requirements short; use delivery_rework_loop for Lead/Coder/Verifier rework; do not include markdown, ChangeSet, baseline, custom graph details, or explanatory prose."
		}
		result.Content = mustJSON(map[string]any{"code": code, "error": err.Error(), "guidance": guidance})
		return result, nil
	}
	if request.BuildRunID != "" && request.BuildRunID != d.buildRunID {
		if resolveErr := d.resolveBuildRunID(ctx); resolveErr != nil {
			result.IsError = true
			result.Content = mustJSON(map[string]any{"code": "build_run_unavailable", "error": resolveErr.Error(), "guidance": "Call tf_submit_brief first, then retry tf_blueprint_plan with the returned build_run_id."})
			return result, nil
		}
	}
	if request.BuildRunID != "" && request.BuildRunID != d.buildRunID {
		result.IsError = true
		result.Content = mustJSON(map[string]any{"code": "build_run_mismatch", "error": "build_run_id does not match the conversation-bound run"})
		return result, nil
	}
	if request.BuildRunID == "" {
		if resolveErr := d.resolveBuildRunID(ctx); resolveErr != nil {
			result.IsError = true
			result.Content = mustJSON(map[string]any{"code": "build_run_unavailable", "error": resolveErr.Error(), "guidance": "Call tf_submit_brief first, then retry tf_blueprint_plan with the returned build_run_id."})
			return result, nil
		}
	}
	blueprint := request.Blueprint
	if request.CompactBlueprint != nil {
		expanded, expandErr := d.server.expandCompactTeamBlueprint(ctx, d.workspaceID, d.buildRunID, *request.CompactBlueprint)
		if expandErr != nil {
			result.IsError = true
			result.Content = mustJSON(map[string]any{"code": "compact_blueprint_expand_failed", "error": expandErr.Error()})
			return result, nil
		}
		blueprint = expanded
	}
	planned, err := d.server.planTeamBlueprint(ctx, d.workspaceID, d.buildRunID, blueprint)
	if err != nil {
		result.IsError = true
		var planning *blueprintPlanningError
		if errors.As(err, &planning) {
			result.Content = mustJSON(map[string]any{
				"code": planning.Code, "error": planning.Message,
				"problems":       planning.Problems,
				"contract_hints": blueprintPlanningContractHints(blueprint),
			})
		} else {
			result.Content = mustJSON(map[string]any{"code": "blueprint_planning_failed", "error": err.Error()})
		}
		return result, nil
	}
	result.Content = mustJSON(planned)
	return result, nil
}

func teamPlanningErrorBudget(toolName string) int {
	if toolName == teamDeclarativeWorkflowPlanToolName {
		return teamDeclarativePlanErrorBudget
	}
	return teamBlueprintPlanErrorBudget
}

func (d *teamBlueprintPlanningDispatcher) resolveBuildRunID(ctx context.Context) error {
	if strings.TrimSpace(d.buildRunID) != "" {
		return nil
	}
	if d == nil || d.server == nil || d.server.TeamBuild == nil || strings.TrimSpace(d.conversationID) == "" {
		return teambuild.ErrBuildRunNotFound
	}
	run, err := d.server.TeamBuild.GetActiveBuildRunByConversation(ctx, d.workspaceID, d.conversationID)
	if err != nil {
		return err
	}
	if run.Status != teambuild.StatusPlanning {
		return fmt.Errorf("conversation build run is %s, want planning", run.Status)
	}
	d.buildRunID = run.BuildRunID
	return nil
}

func (d *teamBlueprintPlanningDispatcher) blockRunAfterPlanningBudget(ctx context.Context) {
	if d == nil || d.server == nil || d.server.TeamBuild == nil {
		return
	}
	if err := d.resolveBuildRunID(ctx); err != nil {
		return
	}
	run, err := d.server.TeamBuild.GetBuildRun(ctx, d.workspaceID, d.buildRunID)
	if err != nil || run.Status != teambuild.StatusPlanning {
		return
	}
	_, _ = d.server.TeamBuild.TransitionStatus(
		ctx, d.workspaceID, d.buildRunID,
		teambuild.StatusPlanning, teambuild.StatusBlocked,
		d.agent, "blueprint_plan_budget_exhausted",
	)
}

func (d *teamBlueprintPlanningDispatcher) recordAudit(ctx context.Context, call contract.ToolCall, result *contract.ToolResult, err error) {
	if d == nil || d.audit == nil {
		return
	}
	status := "ok"
	detail := "ok"
	switch {
	case err != nil:
		status = "error"
		detail = err.Error()
	case result != nil && result.IsError:
		status = "error"
		detail = result.Content
	}
	if d.buildRunID != "" {
		detail = "build_run_id=" + d.buildRunID + " " + detail
	}
	detail = truncateRunes(detail, 200)
	_ = d.audit.Record(ctx, d.workspaceID, d.agent, call.Name, status, detail)
}

func truncateRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

type teamBlueprintPlanToolRequest struct {
	BuildRunID       string                      `json:"build_run_id,omitempty"`
	Blueprint        teambuild.TeamBlueprintV1   `json:"blueprint,omitempty"`
	CompactBlueprint *compactTeamBlueprintPlanV1 `json:"compact_blueprint,omitempty"`
}

func decodeTeamBlueprintPlanToolRequest(raw string) (teamBlueprintPlanToolRequest, error) {
	if unwrapped, ok, err := unwrapTeamBlueprintPlanArguments(raw); ok || err != nil {
		if err != nil {
			return teamBlueprintPlanToolRequest{}, err
		}
		return decodeTeamBlueprintPlanToolRequest(unwrapped)
	}
	var request teamBlueprintPlanToolRequest
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err == nil {
		return request, nil
	}
	if compact, ok, err := decodeCompactBlueprintVariant(json.RawMessage(raw)); ok || err != nil {
		if err != nil {
			return teamBlueprintPlanToolRequest{}, err
		}
		request.CompactBlueprint = &compact
		return request, nil
	}
	// Tool callers occasionally follow the contract text and submit the
	// TeamBlueprint itself instead of wrapping it in {"blueprint": ...}. Keep
	// the nested blueprint strict, but accept this harmless envelope variant so
	// planning does not fail before field-level contract hints can help.
	var blueprint teambuild.TeamBlueprintV1
	decoder = json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&blueprint); err != nil {
		return teamBlueprintPlanToolRequest{}, err
	}
	request.Blueprint = blueprint
	return request, nil
}

func decodeCompactBlueprintVariant(raw json.RawMessage) (compactTeamBlueprintPlanV1, bool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return compactTeamBlueprintPlanV1{}, false, nil
	}
	if nested, ok := fields["compact_blueprint"]; ok {
		var compact compactTeamBlueprintPlanV1
		decoder := json.NewDecoder(bytes.NewReader(nested))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&compact); err != nil {
			return compactTeamBlueprintPlanV1{}, true, err
		}
		return compact, true, nil
	}
	if nested, ok := fields["blueprint"]; ok {
		return decodeCompactBlueprintVariant(nested)
	}
	if _, hasTemplate := fields["template"]; hasTemplate {
		var compact compactTeamBlueprintPlanV1
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&compact); err != nil {
			return compactTeamBlueprintPlanV1{}, true, err
		}
		return compact, true, nil
	}
	return compactTeamBlueprintPlanV1{}, false, nil
}

func unwrapTeamBlueprintPlanArguments(raw string) (string, bool, error) {
	var envelope struct {
		Arguments json.RawMessage `json:"arguments"`
	}
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	if err := decoder.Decode(&envelope); err != nil || len(envelope.Arguments) == 0 {
		return "", false, nil
	}
	trimmed := bytes.TrimSpace(envelope.Arguments)
	if len(trimmed) == 0 {
		return "", true, errors.New("arguments envelope is empty")
	}
	if trimmed[0] == '"' {
		var asString string
		if err := json.Unmarshal(trimmed, &asString); err != nil {
			return "", true, fmt.Errorf("invalid arguments string envelope: %w", err)
		}
		return asString, true, nil
	}
	return string(trimmed), true, nil
}

func (s *Server) expandCompactTeamBlueprint(
	ctx context.Context,
	workspaceID, buildRunID string,
	compact compactTeamBlueprintPlanV1,
) (teambuild.TeamBlueprintV1, error) {
	if s == nil || s.TeamBuild == nil {
		return teambuild.TeamBlueprintV1{}, errors.New("team build store unavailable")
	}
	run, err := s.TeamBuild.GetBuildRun(ctx, workspaceID, buildRunID)
	if err != nil {
		return teambuild.TeamBlueprintV1{}, err
	}
	purpose := strings.TrimSpace(compact.Purpose)
	teamID := strings.TrimSpace(run.Brief.TeamID)
	newTeamName := strings.TrimSpace(run.Brief.NewTeamName)
	pinsByName := map[string]teambuild.BaselineAgentPin{}
	if run.Mode == teambuild.ModeOptimize {
		s.TeamBuild.SetBaselineSources(s.OrgStore, s.Registry, s.Workflow, s.WorkflowArtifacts)
		preview, err := s.TeamBuild.PreviewCompilerBaseline(ctx, workspaceID, buildRunID)
		if err != nil {
			return teambuild.TeamBlueprintV1{}, err
		}
		if preview.Snapshot == nil {
			return teambuild.TeamBlueprintV1{}, errors.New("optimize compact blueprint requires a baseline preview")
		}
		if purpose == "" {
			purpose = strings.TrimSpace(preview.Snapshot.Team.Objective)
		}
		for _, pin := range preview.Snapshot.AgentPins {
			pinsByName[pin.Name] = pin
		}
	}
	if purpose == "" {
		purpose = firstNonBlank(run.Brief.BusinessDirection, run.Brief.Task)
	}
	mode := strings.TrimSpace(run.Mode)
	var providerModels []credentials.ProviderHead
	if mode == teambuild.ModeCreate {
		providerModels, _ = s.compactBlueprintModelPolicy(ctx, workspaceID)
	}
	alias := map[string]string{}
	members := make([]teambuild.BlueprintMemberV1, 0, len(compact.Members))
	for _, member := range compact.Members {
		name := strings.TrimSpace(member.Name)
		stableRef := strings.TrimSpace(member.StableRef)
		if mode == teambuild.ModeOptimize {
			stableRef = name
		} else if stableRef == "" {
			stableRef = name
		}
		if submitted := strings.TrimSpace(member.StableRef); submitted != "" {
			alias[submitted] = stableRef
		}
		alias[name] = stableRef
		pin, pinExists := pinsByName[name]
		modelRef, err := compactBlueprintMemberModelRef(
			mode, name, stableRef, member.ModelRef, member.ExecutionPolicy,
			providerModels, pin, pinExists,
		)
		if err != nil {
			return teambuild.TeamBlueprintV1{}, err
		}
		members = append(members, teambuild.BlueprintMemberV1{
			StableRef: stableRef, Name: name, DisplayName: strings.TrimSpace(member.DisplayName), Role: strings.TrimSpace(member.Role),
			ManagementMode:   strings.TrimSpace(member.ManagementMode),
			Responsibilities: compactStringList(member.Responsibilities),
			Capabilities:     compactStringList(member.Capabilities),
			ModelRef:         modelRef,
			ExecutionPolicy:  member.ExecutionPolicy,
		})
	}
	params := compact.TemplateParameters
	params.PrimaryRef = rewriteCompactRef(params.PrimaryRef, alias)
	params.ReviewerRef = rewriteCompactRef(params.ReviewerRef, alias)
	params.FinalizerRef = rewriteCompactRef(params.FinalizerRef, alias)
	for i := range params.ParallelWorkerRefs {
		params.ParallelWorkerRefs[i] = rewriteCompactRef(params.ParallelWorkerRefs[i], alias)
	}
	if len(params.ResultRequirements) != 0 {
		rewritten := make(map[string]string, len(params.ResultRequirements))
		for key, value := range params.ResultRequirements {
			rewritten[rewriteCompactRef(key, alias)] = value
		}
		params.ResultRequirements = rewritten
	}
	leadRef := rewriteCompactRef(compact.LeadRef, alias)
	paths := rewriteCompactPatchPaths(compact.AllowedPatchPaths, alias)
	if len(paths) == 0 {
		paths = defaultCompactPatchPaths(members, params)
	}
	maxRevisions := compact.MaxRevisions
	if maxRevisions <= 0 {
		maxRevisions = 2
	}
	return teambuild.TeamBlueprintV1{
		SchemaVersion: teambuild.BlueprintSchemaVersionV1,
		Mode:          mode, TeamID: teamID, NewTeamName: newTeamName,
		Purpose: purpose, Members: members, LeadRef: leadRef,
		Workflow: teambuild.BlueprintWorkflowV1{
			Mode: teambuild.BlueprintWorkflowTemplate, Template: strings.TrimSpace(compact.Template), TemplateParameters: &params,
		},
		RevisionPolicy: teambuild.BlueprintRevisionPolicyV1{
			MaxRevisions: maxRevisions, AllowedPatchPaths: paths,
		},
	}, nil
}

func compactBlueprintMemberModelRef(
	mode, name, stableRef, submitted string,
	policy teambuild.BlueprintExecutionPolicyV1,
	providers []credentials.ProviderHead,
	pin teambuild.BaselineAgentPin,
	pinExists bool,
) (string, error) {
	modelRef := strings.TrimSpace(submitted)
	if mode == teambuild.ModeCreate {
		if !modelAllowedForExecutionPolicy(providers, modelRef, policy) {
			modelRef = defaultModelForExecutionPolicy(providers, policy)
		}
		return modelRef, nil
	}
	if modelRef != "" {
		return modelRef, nil
	}
	if mode != teambuild.ModeOptimize || !pinExists {
		return "", fmt.Errorf("optimize compact blueprint has no frozen agent pin for %q", name)
	}
	// An empty baseline model is a real frozen value: CLI agents may use their
	// authenticated Runtime default, and a preserve_existing member must not be
	// forced through a Provider-only migration just to rebuild its workflow.
	// Whether a model-less Loom member is executable remains a candidate/runtime
	// admission decision; compact expansion must preserve the baseline exactly.
	return strings.TrimSpace(pin.Model), nil
}

func (s *Server) compactBlueprintModelPolicy(ctx context.Context, workspaceID string) ([]credentials.ProviderHead, map[string]bool) {
	allowed := map[string]bool{}
	if s == nil || s.Credentials == nil {
		return nil, allowed
	}
	providers, err := s.Credentials.ListMetadata(ctx, workspaceID)
	if err != nil {
		return nil, allowed
	}
	return providers, enabledProviderModelSet(providers)
}

func defaultModelForExecutionPolicy(
	providers []credentials.ProviderHead,
	policy teambuild.BlueprintExecutionPolicyV1,
) string {
	providerID := ""
	switch strings.TrimSpace(policy.Engine) {
	case "codex", "opencode":
		providerID = "system/openai"
	case "claude":
		providerID = "system/anthropic"
	}
	if providerID != "" {
		for _, provider := range providers {
			if provider.ID == providerID && provider.Enabled {
				for _, model := range provider.Models {
					if trimmed := strings.TrimSpace(model); trimmed != "" {
						return trimmed
					}
				}
			}
		}
	}
	return firstEnabledProviderModel(providers)
}

func modelAllowedForExecutionPolicy(
	providers []credentials.ProviderHead,
	modelRef string,
	policy teambuild.BlueprintExecutionPolicyV1,
) bool {
	modelRef = strings.TrimSpace(modelRef)
	if modelRef == "" {
		return false
	}
	requiredProviderID := ""
	switch strings.TrimSpace(policy.Engine) {
	case "codex", "opencode":
		requiredProviderID = "system/openai"
	case "claude":
		requiredProviderID = "system/anthropic"
	}
	for _, provider := range providers {
		if !provider.Enabled || requiredProviderID != "" && provider.ID != requiredProviderID {
			continue
		}
		for _, model := range provider.Models {
			if strings.TrimSpace(model) == modelRef {
				return true
			}
		}
	}
	return false
}

func firstEnabledProviderModel(providers []credentials.ProviderHead) string {
	for _, provider := range providers {
		if !provider.Enabled {
			continue
		}
		for _, model := range provider.Models {
			if trimmed := strings.TrimSpace(model); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

func enabledProviderModelSet(providers []credentials.ProviderHead) map[string]bool {
	out := map[string]bool{}
	for _, provider := range providers {
		if !provider.Enabled {
			continue
		}
		for _, model := range provider.Models {
			if trimmed := strings.TrimSpace(model); trimmed != "" {
				out[trimmed] = true
			}
		}
	}
	return out
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func compactStringList(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func rewriteCompactRef(value string, alias map[string]string) string {
	trimmed := strings.TrimSpace(value)
	if mapped, ok := alias[trimmed]; ok {
		return mapped
	}
	return trimmed
}

func rewriteCompactPatchPaths(paths []string, alias map[string]string) []string {
	out := make([]string, 0, len(paths))
	seen := map[string]bool{}
	for _, path := range paths {
		rewritten := strings.TrimSpace(path)
		for from, to := range alias {
			rewritten = strings.ReplaceAll(rewritten, "/members/"+from+"/", "/members/"+to+"/")
			rewritten = strings.ReplaceAll(rewritten, "/result_requirements/"+from, "/result_requirements/"+to)
		}
		if rewritten != "" && !seen[rewritten] {
			seen[rewritten] = true
			out = append(out, rewritten)
		}
	}
	return out
}

func defaultCompactPatchPaths(members []teambuild.BlueprintMemberV1, params teambuild.BlueprintWorkflowTemplateParametersV1) []string {
	paths := []string{"/purpose", "/workflow/template_parameters/lead_instruction", "/workflow/template_parameters/max_iterations"}
	for _, member := range members {
		ref := strings.TrimSpace(member.StableRef)
		if ref == "" {
			continue
		}
		paths = append(paths, "/members/"+ref+"/responsibilities", "/members/"+ref+"/capabilities")
		if _, ok := params.ResultRequirements[ref]; ok {
			paths = append(paths, "/workflow/template_parameters/result_requirements/"+ref)
		}
	}
	return paths
}

func blueprintPlanningContractHints(blueprint teambuild.TeamBlueprintV1) map[string]any {
	examples := []string{"/purpose", "/workflow/template_parameters/lead_instruction", "/workflow/template_parameters/max_iterations"}
	for _, member := range blueprint.Members {
		ref := strings.TrimSpace(member.StableRef)
		if compilerCreateIdentifierPattern.MatchString(ref) {
			examples = append(examples,
				"/members/"+ref+"/responsibilities",
				"/workflow/template_parameters/result_requirements/"+ref,
			)
		}
	}
	return map[string]any{
		"stable_ref":          "use a lowercase business identity such as dev-coder; optimize uses the existing agent name, never an asset UUID",
		"member_references":   "lead_ref, primary_ref, reviewer_ref, finalizer_ref, parallel_worker_refs, and result_requirements keys must equal members[].stable_ref",
		"management_mode":     "optimize requires preserve_existing for every member; create requires managed",
		"optimize_team":       "optimize may update purpose, roster, and workflow: purpose must be a durable team capability goal, not a concrete business task; keep the existing lead",
		"allowed_patch_paths": "use canonical JSON Pointers to exact mutable leaves; do not use containers, asset refs, wildcards, management_mode, revision_policy, workflow.mode, or workflow.template",
		"examples":            examples,
	}
}

func mustJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return `{"code":"json_encode_failed","error":"result encoding failed"}`
	}
	return string(encoded)
}

func (s *Server) blueprintPlanningTool(workspaceID, buildRunID, agentName string) contract.ToolDispatcher {
	if (agentName != metateam.TeamArchitectName && agentName != metateam.GraphDesignerName) ||
		s == nil || s.TeamBuild == nil {
		return nil
	}
	return &teamBlueprintPlanningDispatcher{
		server: s, workspaceID: workspaceID, buildRunID: buildRunID,
		agent: agentName, audit: s.Audit,
	}
}

func (s *Server) deferredBlueprintPlanningTool(workspaceID, conversationID, agentName string) contract.ToolDispatcher {
	if agentName != metateam.TeamArchitectName || s == nil || s.TeamBuild == nil {
		return nil
	}
	return &teamBlueprintPlanningDispatcher{
		server: s, workspaceID: workspaceID, conversationID: conversationID,
		agent: agentName, audit: s.Audit,
	}
}

var _ contract.ToolDispatcher = (*teamBlueprintPlanningDispatcher)(nil)
