package teamforge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/kernel/org"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

// ReceiptIssuer is the only authority source accepted by the deterministic
// asset applier. The persisted build run remains authoritative for every
// operation; compiler code cannot manufacture a write credential.
type ReceiptIssuer interface {
	ReissueReceipt(context.Context, string, string) (teambuild.BuildAuthorizationReceipt, error)
}

type CompiledAssetApplyRequest struct {
	WorkspaceID       string
	BuildRunID        string
	RevisionNo        int
	Blueprint         teambuild.TeamBlueprintV1
	Operation         ChangeOperationV1
	ExecutionStrategy string
}

type CompiledAssetApplyResult struct {
	Skip       bool
	OutputHash string
	Evidence   json.RawMessage
}

type CompiledAssetApplyError struct {
	Class     string
	Code      string
	Retryable bool
	Cause     error
	Evidence  json.RawMessage
}

func (e *CompiledAssetApplyError) Error() string {
	if e == nil {
		return ""
	}
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", e.Code, e.Cause)
	}
	return e.Code
}

func (e *CompiledAssetApplyError) Unwrap() error { return e.Cause }

// CompiledAssetApplier maps small frozen Blueprint documents onto the
// platform's existing receipt-gated write tools. It never writes storage
// directly and never asks an LLM to generate persisted configuration.
type CompiledAssetApplier struct {
	Issuer    ReceiptIssuer
	Validator ReceiptValidator
	Audit     AuditRecorder
	Reads     Deps
	Writes    WriteDeps

	// dispatchOverride is a test seam after deterministic call construction.
	dispatchOverride func(context.Context, string, contract.ToolCall) (*contract.ToolResult, error)
}

func (a *CompiledAssetApplier) Apply(ctx context.Context, request CompiledAssetApplyRequest) (CompiledAssetApplyResult, error) {
	if a == nil || a.Issuer == nil || a.Validator == nil {
		return CompiledAssetApplyResult{}, compiledAssetFailure("runtime_infrastructure_failure", "compiled_asset_dependencies_unavailable", true, nil)
	}
	if err := teambuild.ValidateTeamBlueprintV1(request.Blueprint); err != nil {
		return CompiledAssetApplyResult{}, compiledAssetFailure("compile_failure", "compiled_asset_blueprint_invalid", false, err)
	}
	receipt, err := a.Issuer.ReissueReceipt(ctx, request.WorkspaceID, request.BuildRunID)
	if err != nil {
		return CompiledAssetApplyResult{}, compiledAssetFailure("governance_failure", "compiled_asset_receipt_unavailable", false, err)
	}

	var kind, tool string
	var args any
	switch request.Operation.Type {
	case OperationAgentCreate, OperationAgentUpdate:
		kind = "agent"
		if request.Operation.Type == OperationAgentCreate {
			tool = ToolCreateAgent
		} else {
			tool = ToolUpdateAgent
		}
		if request.Operation.Type == OperationAgentUpdate {
			if versionErr := a.checkAgentExpectedVersion(ctx, request); versionErr != nil {
				return CompiledAssetApplyResult{}, versionErr
			}
		}
		args, err = compiledAgentArgs(request)
	case OperationTeamCreate:
		kind, tool = "team", ToolCreateTeam
		args, err = a.compiledTeamCreateArgs(ctx, request)
	case OperationRosterSet:
		if result, ok, skipErr := a.skipExactRoster(ctx, request); skipErr != nil {
			return CompiledAssetApplyResult{}, skipErr
		} else if ok {
			return result, nil
		}
		kind, tool = "team", ToolSetRoster
		args, err = a.compiledRosterArgs(ctx, request)
	case OperationTeamUpdate:
		return a.applyTeamUpdate(ctx, receipt, request)
	case OperationAgentGraphCompile:
		return CompiledAssetApplyResult{}, compiledAssetFailure("compile_failure", "agent_graph_compiler_not_connected", false, errors.New("custom internal graph resolver is not connected"))
	default:
		return CompiledAssetApplyResult{}, compiledAssetFailure("compile_failure", "compiled_asset_operation_unsupported", false, fmt.Errorf("operation %s", request.Operation.Type))
	}
	if err != nil {
		return CompiledAssetApplyResult{}, compiledAssetFailure("compile_failure", "compiled_asset_input_invalid", false, err)
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return CompiledAssetApplyResult{}, compiledAssetFailure("compile_failure", "compiled_asset_envelope_invalid", false, err)
	}
	call := contract.ToolCall{ID: request.Operation.OperationID, Name: tool, Args: string(raw)}
	result, err := a.dispatch(ctx, kind, receipt, request, call)
	if err != nil {
		return CompiledAssetApplyResult{}, compiledAssetFailure("runtime_infrastructure_failure", "compiled_asset_dispatch_failed", true, err)
	}
	if result == nil || result.IsError {
		detail := "write tool returned no result"
		if result != nil {
			detail = result.Content
		}
		evidence, _ := json.Marshal(map[string]any{
			"tool_error":     true,
			"operation_id":   request.Operation.OperationID,
			"operation_type": request.Operation.Type,
			"target":         request.Operation.Target,
			"tool":           tool,
			"tool_result":    detail,
		})
		return CompiledAssetApplyResult{}, &CompiledAssetApplyError{Class: "compile_failure", Code: "compiled_asset_write_rejected", Cause: errors.New(detail), Evidence: evidence}
	}
	outputHash := compiledAssetHash([]byte(result.Content))
	evidence, err := json.Marshal(map[string]any{
		"operation_id": request.Operation.OperationID, "operation_type": request.Operation.Type,
		"target": request.Operation.Target, "output_hash": outputHash,
		"tool": tool, "tool_result": json.RawMessage(result.Content),
	})
	if err != nil {
		return CompiledAssetApplyResult{}, err
	}
	return CompiledAssetApplyResult{OutputHash: outputHash, Evidence: evidence}, nil
}

func (a *CompiledAssetApplier) dispatch(ctx context.Context, kind string, receipt teambuild.BuildAuthorizationReceipt, request CompiledAssetApplyRequest, call contract.ToolCall) (*contract.ToolResult, error) {
	if a.dispatchOverride != nil {
		return a.dispatchOverride(ctx, kind, call)
	}
	if kind == "agent" {
		return NewWriteTools(request.WorkspaceID, "platform-compiler", receipt, a.Validator, a.Audit, a.Writes).Dispatch(ctx, call)
	}
	allowCreate := request.Blueprint.Mode == teambuild.ModeCreate
	evaluation := org.TeamEvaluationEvaluated
	if request.ExecutionStrategy == teambuild.ExecutionStrategyTemplateInstantiate {
		evaluation = org.TeamEvaluationUnevaluated
	}
	return newTeamWriteToolsModeWithEvaluation(
		request.WorkspaceID, "platform-compiler", receipt, a.Validator, a.Audit,
		a.Reads, a.Writes, allowCreate, evaluation,
	).Dispatch(ctx, call)
}

func (a *CompiledAssetApplier) applyTeamUpdate(ctx context.Context, receipt teambuild.BuildAuthorizationReceipt, request CompiledAssetApplyRequest) (CompiledAssetApplyResult, error) {
	if request.Blueprint.Mode != teambuild.ModeOptimize {
		return CompiledAssetApplyResult{}, compiledAssetFailure("compile_failure", "team_update_requires_optimize_mode", false, nil)
	}
	if a.Writes.TeamDesign == nil {
		return CompiledAssetApplyResult{}, compiledAssetFailure("runtime_infrastructure_failure", "team_update_writer_unavailable", true, nil)
	}
	var input teamOperationInputV1
	if err := json.Unmarshal(request.Operation.Input, &input); err != nil {
		return CompiledAssetApplyResult{}, compiledAssetFailure("compile_failure", "team_update_input_invalid", false, err)
	}
	team, err := a.resolveTeam(ctx, request.WorkspaceID, request.Operation.Target)
	if err != nil {
		return CompiledAssetApplyResult{}, compiledAssetFailure("compile_failure", "team_update_target_unresolved", false, err)
	}
	if input.TeamID != "" && strings.TrimSpace(input.TeamID) != team.ID {
		return CompiledAssetApplyResult{}, compiledAssetFailure("compile_failure", "team_update_target_mismatch", false, fmt.Errorf("input team_id %q does not match target %q", input.TeamID, team.ID))
	}
	if strings.TrimSpace(input.LeadRef) != "" && strings.TrimSpace(input.LeadRef) != strings.TrimSpace(request.Blueprint.LeadRef) {
		return CompiledAssetApplyResult{}, compiledAssetFailure("compile_failure", "team_update_lead_ref_mismatch", false, fmt.Errorf("input lead_ref %q does not match blueprint lead_ref %q", input.LeadRef, request.Blueprint.LeadRef))
	}
	if err := a.Validator.ValidateReceipt(ctx, request.WorkspaceID, receipt, teambuild.AssetRef{
		Kind: "team",
		ID:   team.ID,
		Name: team.ID,
	}); err != nil {
		return CompiledAssetApplyResult{}, compiledAssetFailure("governance_failure", "team_update_receipt_denied", false, err)
	}
	update := org.UpdateTeamDesignInput{
		Objective:       request.Blueprint.Purpose,
		PrimaryScenario: request.Blueprint.Purpose,
		SuccessCriteria: compiledSuccessCriteria(request.Blueprint),
	}
	updated, err := a.Writes.TeamDesign.UpdateTeamDesign(ctx, request.WorkspaceID, team.ID, update)
	if err != nil {
		return CompiledAssetApplyResult{}, compiledAssetFailure("runtime_infrastructure_failure", "team_update_write_failed", true, err)
	}
	evidence, err := json.Marshal(map[string]any{
		"operation_id":     request.Operation.OperationID,
		"operation_type":   request.Operation.Type,
		"target":           request.Operation.Target,
		"team_id":          updated.ID,
		"objective":        updated.Objective,
		"primary_scenario": updated.PrimaryScenario,
		"success_criteria": updated.SuccessCriteria,
		"updated_at":       updated.UpdatedAt,
	})
	if err != nil {
		return CompiledAssetApplyResult{}, err
	}
	return CompiledAssetApplyResult{OutputHash: compiledAssetHash(evidence), Evidence: evidence}, nil
}

func compiledAgentArgs(request CompiledAssetApplyRequest) (map[string]any, error) {
	var input agentOperationInputV1
	if err := json.Unmarshal(request.Operation.Input, &input); err != nil {
		return nil, err
	}
	member := input.Desired
	if member.ManagementMode != teambuild.BlueprintManagementManaged {
		return nil, errors.New("only managed members may produce agent operations")
	}
	if strings.TrimSpace(request.Operation.Target) != strings.TrimSpace(member.Name) {
		return nil, errors.New("agent operation target must equal the frozen member name")
	}
	engine := strings.TrimSpace(member.ExecutionPolicy.Engine)
	if member.ExecutionPolicy.EngineClass == teambuild.BlueprintEngineStandard {
		engine = "loom"
	}
	prompt := compiledMemberPrompt(request.Blueprint.Purpose, member)
	args := map[string]any{
		"build_run_id": request.BuildRunID,
		"name":         strings.TrimSpace(member.Name), "display_name": strings.TrimSpace(member.DisplayName),
		"role": member.Role, "model": strings.TrimSpace(member.ModelRef),
		"engine": engine, "identity": map[string]any{"core": prompt}, "system_prompt": prompt,
		"tags": []string{"team-build-managed"},
	}
	if member.ExecutionPolicy.EngineClass == teambuild.BlueprintEngineCLI {
		args["runtime_id"] = strings.TrimSpace(member.ExecutionPolicy.RuntimeRef)
	}
	return args, nil
}

func (a *CompiledAssetApplier) checkAgentExpectedVersion(ctx context.Context, request CompiledAssetApplyRequest) error {
	if request.Operation.ExpectedVersion == nil {
		return compiledAssetFailure("compile_failure", "agent_update_expected_version_missing", false, nil)
	}
	record, err := resolveAgentRecord(ctx, a.Writes.AgentLoad, request.WorkspaceID, request.Operation.Target)
	if err != nil {
		return compiledAssetFailure("runtime_infrastructure_failure", "agent_update_target_unavailable", true, err)
	}
	if int64(record.Version) != *request.Operation.ExpectedVersion {
		return compiledAssetFailure("runtime_infrastructure_failure", "agent_update_version_conflict", true,
			fmt.Errorf("expected version %d, got %d", *request.Operation.ExpectedVersion, record.Version))
	}
	return nil
}

func compiledMemberPrompt(purpose string, member teambuild.BlueprintMemberV1) string {
	return fmt.Sprintf("You are %s in a platform-managed team.\nTeam purpose: %s\nResponsibilities:\n- %s\nCapabilities:\n- %s\nComplete only the current workflow node's assigned responsibilities using available tools, return evidence, and state blockers honestly. Do not spawn or delegate to native CLI sub-agents: Weave owns team delegation, parallelism, review, retries, and delivery through the published workflow.",
		member.Role, normalizeChangeText(purpose), strings.Join(sortedNormalized(member.Responsibilities), "\n- "), strings.Join(sortedNormalized(member.Capabilities), "\n- "))
}

func (a *CompiledAssetApplier) compiledTeamCreateArgs(ctx context.Context, request CompiledAssetApplyRequest) (map[string]any, error) {
	if request.Blueprint.Mode != teambuild.ModeCreate {
		return nil, errors.New("team_create requires create mode")
	}
	if strings.TrimSpace(request.Operation.Target) != strings.TrimSpace(request.Blueprint.NewTeamName) {
		return nil, errors.New("team_create target must equal the frozen new team name")
	}
	lead, workers, err := a.resolveTeamMembers(ctx, request)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"build_run_id": request.BuildRunID, "name": request.Blueprint.NewTeamName,
		"objective": request.Blueprint.Purpose, "primary_scenario": request.Blueprint.Purpose,
		"success_criteria": compiledSuccessCriteria(request.Blueprint),
		"lead_avatar_id":   lead.ID, "workers": workers,
	}, nil
}

func (a *CompiledAssetApplier) compiledRosterArgs(ctx context.Context, request CompiledAssetApplyRequest) (map[string]any, error) {
	lead, workers, err := a.resolveTeamMembers(ctx, request)
	if err != nil {
		return nil, err
	}
	team, err := a.resolveTeam(ctx, request.WorkspaceID, request.Operation.Target)
	if err != nil {
		return nil, err
	}
	revision := request.RevisionNo
	if revision < 1 {
		revision = 1
	}
	return map[string]any{
		"build_run_id": request.BuildRunID, "round_no": revision, "team_id": team.ID,
		"lead_agent_id": lead.ID, "workers": workers,
	}, nil
}

func (a *CompiledAssetApplier) resolveTeamMembers(ctx context.Context, request CompiledAssetApplyRequest) (*registry.AgentRecord, []map[string]any, error) {
	byRef := make(map[string]*registry.AgentRecord, len(request.Blueprint.Members))
	byBlueprint := make(map[string]teambuild.BlueprintMemberV1, len(request.Blueprint.Members))
	for _, member := range request.Blueprint.Members {
		record, err := resolveAgentRecord(ctx, a.Writes.AgentLoad, request.WorkspaceID, member.Name)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve member %s: %w", member.StableRef, err)
		}
		byRef[member.StableRef] = record
		byBlueprint[member.StableRef] = member
	}
	lead := byRef[request.Blueprint.LeadRef]
	if lead == nil || lead.Role != teambuild.BlueprintMemberRoleAvatar {
		return nil, nil, errors.New("resolved lead is unavailable or not an avatar")
	}
	refs := make([]string, 0, len(byRef))
	for ref, member := range byBlueprint {
		if member.Role == teambuild.BlueprintMemberRoleWorker {
			refs = append(refs, ref)
		}
	}
	sort.Strings(refs)
	workers := make([]map[string]any, 0, len(refs))
	for _, ref := range refs {
		member, record := byBlueprint[ref], byRef[ref]
		workers = append(workers, map[string]any{
			"worker_agent_id": record.ID, "duty": strings.Join(sortedNormalized(member.Responsibilities), "; "),
			"when_to_use":         "Dispatch when the team needs: " + strings.Join(sortedNormalized(member.Capabilities), ", "),
			"context_instruction": compiledMemberPrompt(request.Blueprint.Purpose, member),
			"allowed_kinds":       []string{"consult", "dispatch"}, "default_kind": "dispatch",
			"result_requirement": resultRequirement(request.Blueprint, ref),
		})
	}
	return lead, workers, nil
}

func (a *CompiledAssetApplier) skipExactRoster(ctx context.Context, request CompiledAssetApplyRequest) (CompiledAssetApplyResult, bool, error) {
	team, err := a.resolveTeam(ctx, request.WorkspaceID, request.Operation.Target)
	if err != nil {
		return CompiledAssetApplyResult{}, false, nil
	}
	// An exact roster is not sufficient for a freshly created team: create-mode
	// deliberately leaves the aggregate in building, and tf_set_roster is the
	// receipt-gated transition that activates it. Create-mode roster_set is a
	// lifecycle operation and must never be optimized away; only an already-
	// active optimize target may take the idempotent shortcut.
	if request.Blueprint.Mode == teambuild.ModeCreate || team.Status != "active" {
		return CompiledAssetApplyResult{}, false, nil
	}
	lead, desired, err := a.resolveTeamMembers(ctx, request)
	if err != nil {
		return CompiledAssetApplyResult{}, false, compiledAssetFailure("compile_failure", "roster_member_resolution_failed", false, err)
	}
	if team.LeadAvatarID != lead.ID || a.Reads.Roster == nil {
		return CompiledAssetApplyResult{}, false, nil
	}
	current, err := a.Reads.Roster.ListByTeam(ctx, request.WorkspaceID, team.ID)
	if err != nil {
		return CompiledAssetApplyResult{}, false, compiledAssetFailure("runtime_infrastructure_failure", "roster_read_failed", true, err)
	}
	want := make([]string, 0, len(desired))
	currentByID := make(map[string]registry.TeamWorker, len(current))
	for _, worker := range current {
		currentByID[worker.WorkerAgentID] = worker
	}
	for _, worker := range desired {
		workerID := worker["worker_agent_id"].(string)
		want = append(want, workerID)
		actual, exists := currentByID[workerID]
		if !exists || !actual.Enabled ||
			actual.Duty != worker["duty"].(string) ||
			actual.WhenToUse != worker["when_to_use"].(string) ||
			actual.ContextInstruction != worker["context_instruction"].(string) ||
			!equalSortedStrings(actual.AllowedKinds, worker["allowed_kinds"].([]string)) ||
			actual.DefaultKind != worker["default_kind"].(string) ||
			actual.ResultRequirement != worker["result_requirement"].(string) {
			return CompiledAssetApplyResult{}, false, nil
		}
	}
	got := make([]string, 0, len(current))
	for _, worker := range current {
		if worker.Enabled {
			got = append(got, worker.WorkerAgentID)
		}
	}
	sort.Strings(want)
	sort.Strings(got)
	if strings.Join(want, "\x00") != strings.Join(got, "\x00") {
		return CompiledAssetApplyResult{}, false, nil
	}
	evidence, _ := json.Marshal(map[string]any{"team_id": team.ID, "roster_exact": true, "worker_ids": want})
	return CompiledAssetApplyResult{Skip: true, OutputHash: compiledAssetHash(evidence), Evidence: evidence}, true, nil
}

func (a *CompiledAssetApplier) resolveTeam(ctx context.Context, workspaceID, target string) (org.Team, error) {
	if a.Reads.Teams == nil {
		return org.Team{}, errors.New("team reader unavailable")
	}
	teams, err := a.Reads.Teams.ListTeams(ctx, workspaceID)
	if err != nil {
		return org.Team{}, err
	}
	for _, team := range teams {
		if team.ID == strings.TrimSpace(target) || team.Name == strings.TrimSpace(target) {
			return team, nil
		}
	}
	return org.Team{}, fmt.Errorf("team %q not found", target)
}

func resolveAgentRecord(ctx context.Context, loader AgentLoader, workspaceID, target string) (*registry.AgentRecord, error) {
	if loader == nil {
		return nil, errors.New("agent loader unavailable")
	}
	record, err := loader.Get(ctx, workspaceID, strings.TrimSpace(target))
	if err == nil {
		return record, nil
	}
	values, listErr := loader.List(ctx, workspaceID)
	if listErr != nil {
		return nil, err
	}
	for index := range values {
		if values[index].ID == strings.TrimSpace(target) {
			return &values[index], nil
		}
	}
	return nil, err
}

func compiledSuccessCriteria(blueprint teambuild.TeamBlueprintV1) string {
	params := blueprint.Workflow.TemplateParameters
	if params == nil || len(params.ResultRequirements) == 0 {
		return "Return a verifiable result that satisfies the frozen workflow contract."
	}
	refs := make([]string, 0, len(params.ResultRequirements))
	for ref := range params.ResultRequirements {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	items := make([]string, 0, len(refs))
	for _, ref := range refs {
		items = append(items, ref+": "+strings.TrimSpace(params.ResultRequirements[ref]))
	}
	return strings.Join(items, "\n")
}

func resultRequirement(blueprint teambuild.TeamBlueprintV1, ref string) string {
	if blueprint.Workflow.TemplateParameters != nil {
		if value := strings.TrimSpace(blueprint.Workflow.TemplateParameters.ResultRequirements[ref]); value != "" {
			return value
		}
	}
	return "Return a verifiable result with evidence."
}

func sortedNormalized(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if normalized := normalizeChangeText(value); normalized != "" {
			result = append(result, normalized)
		}
	}
	sort.Strings(result)
	return result
}

func compiledAssetHash(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func compiledAssetFailure(class, code string, retryable bool, cause error) *CompiledAssetApplyError {
	return &CompiledAssetApplyError{Class: class, Code: code, Retryable: retryable, Cause: cause}
}
