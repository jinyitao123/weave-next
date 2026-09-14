package teamconstruction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/build/teameval"
	"github.com/jinyitao123/weave/internal/build/teamforge"
	"github.com/jinyitao123/weave/internal/build/teamorch"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

// HandleOperation is the production deterministic operation adapter. The
// first compiler-v1 closure deliberately uses only existing platform CAS
// surfaces; unsupported operation contracts fail closed instead of invoking
// a meta-agent or pretending the write succeeded.
func (p *ProductionPhases) HandleOperation(ctx context.Context, operation teamorch.OperationContext) (teamorch.OperationResult, error) {
	if p == nil || p.Deps.Build == nil {
		return teamorch.OperationResult{}, &teamorch.OperationError{Class: teameval.FailureClassRuntimeInfrastructure,
			Code: "compiler_phase_dependencies_unavailable", Retryable: true}
	}
	if operation.Run.EvaluationOnly {
		switch operation.Operation.Type {
		case teamforge.OperationWorkflowCompile, teamforge.OperationCandidateRun, teamforge.OperationPublish:
		default:
			return teamorch.OperationResult{}, &teamorch.OperationError{Class: teameval.FailureClassGovernance,
				Code: "evaluation_asset_mutation_forbidden", Evidence: json.RawMessage(`{"evaluation_only":true}`)}
		}
	}
	switch operation.Operation.Type {
	case teamforge.OperationAgentCreate, teamforge.OperationAgentUpdate,
		teamforge.OperationTeamCreate, teamforge.OperationTeamUpdate,
		teamforge.OperationRosterSet, teamforge.OperationAgentGraphCompile:
		return p.handleCompiledAsset(ctx, operation)
	case teamforge.OperationWorkflowCompile:
		return p.handleCompilerWorkflow(ctx, operation)
	case teamforge.OperationCandidateRun:
		return p.handleCompilerCandidate(ctx, operation)
	case teamforge.OperationPublish:
		return p.handleCompilerPublish(ctx, operation)
	default:
		return teamorch.OperationResult{}, &teamorch.OperationError{Class: teameval.FailureClassCompile,
			Code:     "compiler_operation_handler_unavailable_" + string(operation.Operation.Type),
			Evidence: json.RawMessage(`{"fail_closed":true}`)}
	}
}

func (p *ProductionPhases) handleCompiledAsset(ctx context.Context, operation teamorch.OperationContext) (teamorch.OperationResult, error) {
	applier := &teamforge.CompiledAssetApplier{
		Issuer: p.Deps.Build, Validator: p.Deps.Build, Audit: p.Deps.Audit,
		Reads: p.teamForgeDeps(), Writes: p.teamForgeWriteDeps(),
	}
	result, err := applier.Apply(ctx, teamforge.CompiledAssetApplyRequest{
		WorkspaceID: operation.WorkspaceID, BuildRunID: operation.BuildRunID,
		RevisionNo: operation.Revision.RevisionNo, Blueprint: operation.Blueprint,
		Operation: operation.Operation, ExecutionStrategy: operation.Run.EffectiveExecutionStrategy(),
	})
	if err != nil {
		var assetErr *teamforge.CompiledAssetApplyError
		if errors.As(err, &assetErr) {
			return teamorch.OperationResult{}, &teamorch.OperationError{
				Class: teameval.FailureClass(assetErr.Class), Code: assetErr.Code,
				Retryable: assetErr.Retryable, Evidence: assetErr.Evidence, Cause: assetErr.Cause,
			}
		}
		return teamorch.OperationResult{}, err
	}
	return teamorch.OperationResult{Skip: result.Skip, OutputHash: result.OutputHash, Evidence: result.Evidence}, nil
}

type compilerWorkflowTemplateInput struct {
	Mode         string `json:"mode"`
	CompiledHash string `json:"compiled_hash"`
}

type compilerWorkflowDeclarativeInput struct {
	Mode           string                                    `json:"mode"`
	SourceSpecHash string                                    `json:"source_spec_hash"`
	SpecHash       string                                    `json:"spec_hash"`
	CompiledHash   string                                    `json:"compiled_hash"`
	FrozenSpec     teamforge.FrozenDeclarativeWorkflowSpecV1 `json:"frozen_spec"`
}

type compilerWorkflowVersionEvidence struct {
	Compiler         string `json:"compiler"`
	WorkflowID       string `json:"workflow_id"`
	WorkflowVersion  int    `json:"workflow_version"`
	WorkflowRef      string `json:"workflow_ref"`
	SpecHash         string `json:"spec_hash"`
	CompiledHash     string `json:"compiled_hash"`
	PlannedShapeHash string `json:"planned_shape_hash,omitempty"`
}

type compilerWorkflowBuildInput struct {
	BuildRunID string                       `json:"build_run_id"`
	WorkflowID string                       `json:"workflow_id"`
	Create     *compilerWorkflowCreateInput `json:"create,omitempty"`
	Blueprint  teamforge.WorkflowBlueprint  `json:"blueprint"`
}

type compilerWorkflowCreateInput struct {
	Name        string `json:"name"`
	TeamID      string `json:"team_id"`
	Description string `json:"description"`
}

func workflowOperationNeedsCreate(operation teamforge.ChangeOperationV1) bool {
	return operation.ExpectedVersion == nil || operation.RollbackRef == "workflow:none"
}

func (p *ProductionPhases) handleCompilerWorkflow(ctx context.Context, operation teamorch.OperationContext) (teamorch.OperationResult, error) {
	if operation.Operation.Compiler == teamforge.CompilerDeclarativeV1 {
		return p.handleCompilerDeclarativeWorkflow(ctx, operation)
	}
	var input compilerWorkflowTemplateInput
	if err := json.Unmarshal(operation.Operation.Input, &input); err != nil {
		return teamorch.OperationResult{}, &teamorch.OperationError{Class: teameval.FailureClassCompile,
			Code: "workflow_compile_input_invalid", Cause: err}
	}
	if input.Mode != teambuild.BlueprintWorkflowTemplate || operation.Operation.Compiler != teamforge.CompilerWorkflowV1 {
		return teamorch.OperationResult{}, &teamorch.OperationError{Class: teameval.FailureClassCompile,
			Code: "workflow_custom_compiler_not_connected", Evidence: json.RawMessage(`{"fail_closed":true}`)}
	}
	members := make(map[string]teambuild.BlueprintMemberV1, len(operation.Blueprint.Members))
	for _, member := range operation.Blueprint.Members {
		members[strings.TrimSpace(member.StableRef)] = member
	}
	boundBlueprint, boundHash, err := teamforge.BindWorkflowTemplateOperationV1(operation.Operation.Input,
		func(ref string) (string, int64, error) {
			member, ok := members[strings.TrimSpace(ref)]
			if !ok {
				return "", 0, fmt.Errorf("workflow member ref %q is not frozen in blueprint", ref)
			}
			record, loadErr := p.Deps.Agents.Get(ctx, operation.WorkspaceID, member.Name)
			if loadErr != nil {
				return "", 0, loadErr
			}
			return record.ID, int64(record.Version), nil
		})
	if err != nil {
		return teamorch.OperationResult{}, &teamorch.OperationError{Class: teameval.FailureClassCompile,
			Code: "workflow_compile_member_binding_failed", Cause: err}
	}
	if operation.Run.EvaluationOnly {
		return p.verifyEvaluationWorkflowDraft(ctx, operation, boundHash, "")
	}
	receipt, err := p.Deps.Build.ReissueReceipt(ctx, operation.WorkspaceID, operation.BuildRunID)
	if err != nil {
		return teamorch.OperationResult{}, &teamorch.OperationError{Class: teameval.FailureClassGovernance,
			Code: "workflow_compile_receipt_unavailable", Cause: err}
	}
	workflowID := strings.TrimSpace(operation.Operation.Target)
	callInput := compilerWorkflowBuildInput{
		BuildRunID: operation.BuildRunID,
		WorkflowID: workflowID,
		Blueprint:  boundBlueprint,
	}
	var createTeamID string
	if workflowOperationNeedsCreate(operation.Operation) {
		team, resolveErr := p.targetTeam(ctx, operation.WorkspaceID, operation.Run)
		if resolveErr != nil || team == nil {
			return teamorch.OperationResult{}, &teamorch.OperationError{Class: teameval.FailureClassCompile,
				Code: "workflow_compile_team_unavailable", Cause: resolveErr}
		}
		createTeamID = team.ID
		callInput.Create = &compilerWorkflowCreateInput{
			Name:        workflowID,
			TeamID:      team.ID,
			Description: operation.Blueprint.Purpose,
		}
	}
	args, err := json.Marshal(callInput)
	if err != nil {
		return teamorch.OperationResult{}, &teamorch.OperationError{Class: teameval.FailureClassCompile,
			Code: "workflow_compile_envelope_invalid", Cause: err}
	}
	newWorkflowTools := teamforge.NewWorkflowBuildTools
	if operation.Run.EffectiveExecutionStrategy() == teambuild.ExecutionStrategyTemplateInstantiate {
		newWorkflowTools = teamforge.NewTemplateWorkflowBuildTools
	}
	dispatcher := newWorkflowTools(
		operation.WorkspaceID, "platform-compiler", receipt, p.Deps.Build, p.Deps.Audit,
		p.teamForgeDeps(), p.teamForgeWriteDeps(), p.Deps.Drafts.WorkflowDrafts(operation.WorkspaceID, operation.BuildRunID),
	)
	toolResult, err := dispatcher.Dispatch(ctx, contract.ToolCall{
		ID: operation.Operation.OperationID, Name: teamforge.ToolWorkflowBlueprintBuild, Args: string(args),
	})
	if err != nil {
		return teamorch.OperationResult{}, &teamorch.OperationError{Class: teameval.FailureClassRuntimeInfrastructure,
			Code: "workflow_compile_dispatch_failed", Retryable: true, Cause: err}
	}
	if toolResult == nil || toolResult.IsError {
		detail := "workflow compiler returned no result"
		if toolResult != nil {
			detail = toolResult.Content
		}
		evidence, _ := json.Marshal(map[string]any{
			"compiler":       operation.Operation.Compiler,
			"workflow_id":    workflowID,
			"planned_create": callInput.Create != nil,
			"create_team_id": createTeamID,
			"tool_result":    detail,
		})
		return teamorch.OperationResult{}, &teamorch.OperationError{Class: teameval.FailureClassCompile,
			Code: "workflow_compile_rejected", Evidence: evidence, Cause: errors.New(detail)}
	}
	var committed struct {
		WorkflowID string `json:"workflow_id"`
		Committed  bool   `json:"committed"`
		Version    int    `json:"version"`
	}
	if err := json.Unmarshal([]byte(toolResult.Content), &committed); err != nil ||
		!committed.Committed || committed.WorkflowID != workflowID || committed.Version < 1 {
		if err == nil {
			err = errors.New("workflow compiler result does not identify a committed target draft")
		}
		return teamorch.OperationResult{}, &teamorch.OperationError{Class: teameval.FailureClassCompile,
			Code: "workflow_compile_evidence_invalid", Cause: err}
	}
	evidence, err := json.Marshal(struct {
		compilerWorkflowVersionEvidence
		PlannedShapeHash string          `json:"planned_shape_hash"`
		ToolResult       json.RawMessage `json:"tool_result"`
	}{
		compilerWorkflowVersionEvidence: compilerWorkflowVersionEvidence{
			Compiler: operation.Operation.Compiler, WorkflowID: committed.WorkflowID,
			WorkflowVersion: committed.Version,
			WorkflowRef:     fmt.Sprintf("workflow-version:%d", committed.Version),
			CompiledHash:    boundHash,
		},
		PlannedShapeHash: input.CompiledHash,
		ToolResult:       json.RawMessage(toolResult.Content),
	})
	if err != nil {
		return teamorch.OperationResult{}, err
	}
	return teamorch.OperationResult{OutputHash: boundHash, Evidence: evidence}, nil
}

func (p *ProductionPhases) handleCompilerDeclarativeWorkflow(
	ctx context.Context,
	operation teamorch.OperationContext,
) (teamorch.OperationResult, error) {
	var input compilerWorkflowDeclarativeInput
	if err := json.Unmarshal(operation.Operation.Input, &input); err != nil {
		return teamorch.OperationResult{}, &teamorch.OperationError{Class: teameval.FailureClassCompile,
			Code: "workflow_compile_input_invalid", Cause: err}
	}
	if input.Mode != teambuild.BlueprintWorkflowDeclarativeV1 ||
		operation.Blueprint.Workflow.Mode != teambuild.BlueprintWorkflowDeclarativeV1 {
		return teamorch.OperationResult{}, &teamorch.OperationError{Class: teameval.FailureClassCompile,
			Code: "workflow_declarative_mode_mismatch"}
	}
	if err := teamforge.ValidateFrozenDeclarativeWorkflowSpecV1(input.FrozenSpec); err != nil {
		return teamorch.OperationResult{}, &teamorch.OperationError{Class: teameval.FailureClassCompile,
			Code: "workflow_declarative_frozen_spec_invalid", Cause: err}
	}
	if !compilerDeclarativeSpecHashesMatch(input, operation.Blueprint.Workflow.DeclarativeSpecHash) {
		return teamorch.OperationResult{}, &teamorch.OperationError{Class: teameval.FailureClassCompile,
			Code: "workflow_declarative_spec_hash_mismatch"}
	}
	compiledHash, err := declarativeCompiledGraphHash(input.FrozenSpec)
	if err != nil || compiledHash != input.CompiledHash {
		return teamorch.OperationResult{}, &teamorch.OperationError{Class: teameval.FailureClassCompile,
			Code: "workflow_declarative_compiled_hash_mismatch", Cause: err}
	}
	binding := input.FrozenSpec.BuildBinding
	if binding.BuildRunID != operation.BuildRunID ||
		binding.BriefHash != operation.Run.BriefHash ||
		binding.ContractHash != operation.Run.ContractHash ||
		binding.BaselineHash != operation.Revision.BaselineHash ||
		!reflect.DeepEqual(binding.AssetScope, operation.Run.AssetScope) {
		return teamorch.OperationResult{}, &teamorch.OperationError{Class: teameval.FailureClassCompile,
			Code: "workflow_declarative_build_binding_mismatch"}
	}

	materializedSpec, err := resolveCreateDeclarativeMaterializationSpec(
		ctx, p.Deps.Agents, operation.WorkspaceID, operation.Blueprint, input.FrozenSpec,
	)
	if err != nil {
		return teamorch.OperationResult{}, &teamorch.OperationError{Class: teameval.FailureClassCompile,
			Code: "workflow_compile_member_binding_failed", Cause: err}
	}
	materializedHash, err := declarativeCompiledGraphHash(materializedSpec)
	if err != nil {
		return teamorch.OperationResult{}, &teamorch.OperationError{Class: teameval.FailureClassCompile,
			Code: "workflow_declarative_materialized_hash_failed", Cause: err}
	}
	if operation.Run.EvaluationOnly {
		return p.verifyEvaluationWorkflowDraft(ctx, operation, materializedHash, materializedSpec.SpecHash)
	}

	workflowID := strings.TrimSpace(operation.Operation.Target)
	version, skip, err := p.materializeDeclarativeWorkflow(ctx, operation, workflowID, materializedSpec)
	if err != nil {
		return teamorch.OperationResult{}, &teamorch.OperationError{Class: teameval.FailureClassRuntimeInfrastructure,
			Code: "workflow_declarative_materialize_failed", Retryable: true, Cause: err}
	}
	workflowRef := fmt.Sprintf("workflow-version:%d", version.Version)
	return declarativeWorkflowCompileResult(skip, compilerWorkflowVersionEvidence{
		Compiler: operation.Operation.Compiler, WorkflowID: workflowID,
		WorkflowVersion: version.Version, WorkflowRef: workflowRef,
		SpecHash: input.SpecHash, CompiledHash: materializedHash,
		PlannedShapeHash: compiledHash,
	})
}

// verifyEvaluationWorkflowDraft proves the lineage-derived workflow is still
// the exact existing draft without writing it. The compiler operation remains
// in the DAG to carry the declarative frozen spec and to bind candidate
// execution to a concrete version, but evaluation_only turns it into a strict
// verification step.
func (p *ProductionPhases) verifyEvaluationWorkflowDraft(
	ctx context.Context,
	operation teamorch.OperationContext,
	wantCompiledHash, specHash string,
) (teamorch.OperationResult, error) {
	workflowID := strings.TrimSpace(operation.Operation.Target)
	versionNo, err := p.latestDraftVersion(ctx, operation.WorkspaceID, workflowID)
	if err != nil {
		return teamorch.OperationResult{}, &teamorch.OperationError{Class: teameval.FailureClassGovernance,
			Code: "evaluation_workflow_draft_unavailable", Cause: err}
	}
	version, err := p.Deps.Workflows.GetVersion(ctx, operation.WorkspaceID, workflowID, versionNo)
	if err != nil {
		return teamorch.OperationResult{}, &teamorch.OperationError{Class: teameval.FailureClassRuntimeInfrastructure,
			Code: "evaluation_workflow_draft_read_failed", Retryable: true, Cause: err}
	}
	actualHash, err := workflowDraftContentHash(version.TriggerConfig, version.GraphDefinition)
	if err != nil {
		return teamorch.OperationResult{}, err
	}
	if actualHash != wantCompiledHash {
		return teamorch.OperationResult{}, &teamorch.OperationError{Class: teameval.FailureClassGovernance,
			Code:     "evaluation_workflow_draft_changed",
			Evidence: json.RawMessage(`{"evaluation_only":true,"asset_changed":true}`)}
	}
	evidence, err := json.Marshal(compilerWorkflowVersionEvidence{
		Compiler: operation.Operation.Compiler, WorkflowID: workflowID,
		WorkflowVersion: versionNo, WorkflowRef: fmt.Sprintf("workflow-version:%d", versionNo),
		SpecHash: specHash, CompiledHash: actualHash, PlannedShapeHash: wantCompiledHash,
	})
	if err != nil {
		return teamorch.OperationResult{}, err
	}
	return teamorch.OperationResult{Skip: true, OutputHash: actualHash, Evidence: evidence}, nil
}

type declarativeAgentLoader interface {
	Get(context.Context, string, string) (*registry.AgentRecord, error)
	GetVersion(context.Context, string, string, int) (*registry.AgentRecord, error)
}

func resolveCreateDeclarativeMaterializationSpec(
	ctx context.Context,
	agents declarativeAgentLoader,
	workspaceID string,
	blueprint teambuild.TeamBlueprintV1,
	spec teamforge.FrozenDeclarativeWorkflowSpecV1,
) (teamforge.FrozenDeclarativeWorkflowSpecV1, error) {
	if blueprint.Mode != teambuild.ModeCreate && blueprint.Mode != teambuild.ModeOptimize {
		return spec, nil
	}
	members := make(map[string]teambuild.BlueprintMemberV1, len(blueprint.Members))
	for _, member := range blueprint.Members {
		members[strings.TrimSpace(member.StableRef)] = member
	}
	bindings := append([]teamforge.DeclarativeWorkerBindingV1(nil), spec.WorkerBindings...)
	for index := range bindings {
		binding := &bindings[index]
		member, ok := members[strings.TrimSpace(binding.StableRef)]
		if !ok || member.Role != teambuild.BlueprintMemberRoleWorker ||
			(blueprint.Mode == teambuild.ModeCreate && member.ManagementMode != teambuild.BlueprintManagementManaged) {
			return teamforge.FrozenDeclarativeWorkflowSpecV1{},
				fmt.Errorf("declarative worker ref %q is not a managed Blueprint worker", binding.StableRef)
		}
		name := strings.TrimSpace(member.Name)
		bindingAgentID := strings.TrimSpace(binding.AgentID)
		if name == "" {
			return teamforge.FrozenDeclarativeWorkflowSpecV1{},
				fmt.Errorf("declarative worker ref %q does not match its frozen Blueprint target", binding.StableRef)
		}
		record, err := agents.Get(ctx, workspaceID, name)
		if err != nil {
			return teamforge.FrozenDeclarativeWorkflowSpecV1{}, err
		}
		switch blueprint.Mode {
		case teambuild.ModeCreate:
			if bindingAgentID != name {
				return teamforge.FrozenDeclarativeWorkflowSpecV1{},
					fmt.Errorf("declarative worker ref %q does not match its frozen Blueprint target", binding.StableRef)
			}
		case teambuild.ModeOptimize:
			if bindingAgentID != strings.TrimSpace(record.ID) && bindingAgentID != name {
				return teamforge.FrozenDeclarativeWorkflowSpecV1{},
					fmt.Errorf("declarative worker ref %q does not match its frozen Blueprint target", binding.StableRef)
			}
		}
		if strings.TrimSpace(record.ID) == "" || record.Name != name || int64(record.Version) != binding.AgentVersion {
			return teamforge.FrozenDeclarativeWorkflowSpecV1{},
				fmt.Errorf("declarative worker ref %q did not materialize as the frozen AgentVersion", binding.StableRef)
		}
		exact, err := agents.GetVersion(ctx, workspaceID, record.ID, record.Version)
		if err != nil {
			return teamforge.FrozenDeclarativeWorkflowSpecV1{}, err
		}
		if exact.ID != record.ID || exact.Name != name || exact.Version != record.Version {
			return teamforge.FrozenDeclarativeWorkflowSpecV1{},
				fmt.Errorf("declarative worker ref %q exact AgentVersion identity mismatch", binding.StableRef)
		}
		binding.AgentID = record.ID
	}
	return teamforge.RebindFrozenDeclarativeWorkerBindingsV1(spec, bindings)
}

func compilerDeclarativeSpecHashesMatch(input compilerWorkflowDeclarativeInput, blueprintSourceHash string) bool {
	sourceHash := strings.TrimSpace(input.SourceSpecHash)
	if sourceHash == "" {
		// Compatibility for unchanged declarative operations compiled before
		// AgentVersion rebinding introduced a distinct effective spec hash.
		sourceHash = strings.TrimSpace(input.SpecHash)
	}
	return strings.TrimSpace(input.SpecHash) == strings.TrimSpace(input.FrozenSpec.SpecHash) &&
		sourceHash == strings.TrimSpace(blueprintSourceHash)
}

func declarativeWorkflowCompileResult(
	skip bool,
	evidenceValue compilerWorkflowVersionEvidence,
) (teamorch.OperationResult, error) {
	evidence, err := json.Marshal(evidenceValue)
	if err != nil {
		return teamorch.OperationResult{}, err
	}
	return teamorch.OperationResult{Skip: skip, OutputHash: evidenceValue.CompiledHash, Evidence: evidence}, nil
}

func declarativeCompiledGraphHash(spec teamforge.FrozenDeclarativeWorkflowSpecV1) (string, error) {
	return workflowDraftContentHash(spec.TriggerConfig, spec.GraphDefinition)
}

func workflowDraftContentHash(trigger, graph json.RawMessage) (string, error) {
	raw, err := json.Marshal(struct {
		Trigger json.RawMessage `json:"trigger_config"`
		Graph   json.RawMessage `json:"graph_definition"`
	}{Trigger: trigger, Graph: graph})
	if err != nil {
		return "", err
	}
	return frozen.HashCanonicalJSON(raw)
}

func (p *ProductionPhases) materializeDeclarativeWorkflow(
	ctx context.Context,
	operation teamorch.OperationContext,
	workflowID string,
	spec teamforge.FrozenDeclarativeWorkflowSpecV1,
) (*workflow.TeamWorkflowVersion, bool, error) {
	if workflowID == "" {
		return nil, false, errors.New("workflow target is required")
	}
	workflowRow, err := p.Deps.Workflows.Get(ctx, operation.WorkspaceID, workflowID)
	if errors.Is(err, workflow.ErrNotFound) {
		team, resolveErr := p.targetTeam(ctx, operation.WorkspaceID, operation.Run)
		if resolveErr != nil || team == nil {
			return nil, false, fmt.Errorf("resolve workflow team: %w", resolveErr)
		}
		version, createErr := p.Deps.Workflows.Create(ctx, &workflow.TeamWorkflow{
			WorkspaceID: operation.WorkspaceID, ID: workflowID, TeamID: team.ID,
			Name: workflowID, Description: operation.Blueprint.Purpose,
			Status: workflow.WorkflowStatusActive,
		}, workflow.DraftInput{
			TriggerConfig: spec.TriggerConfig, GraphDefinition: spec.GraphDefinition,
			CreatedBy: teamorch.DefaultActor,
		})
		return version, false, createErr
	}
	if err != nil {
		return nil, false, err
	}
	team, err := p.targetTeam(ctx, operation.WorkspaceID, operation.Run)
	if err != nil || team == nil {
		return nil, false, fmt.Errorf("resolve workflow team: %w", err)
	}
	if workflowRow.TeamID != team.ID || workflowRow.Status == workflow.WorkflowStatusArchived {
		return nil, false, errors.New("workflow target is unavailable for declarative materialization")
	}

	versions, err := p.Deps.Workflows.ListVersionsByWorkflows(ctx, operation.WorkspaceID, []string{workflowID})
	if err != nil {
		return nil, false, err
	}
	var draft *workflow.TeamWorkflowVersion
	for index := range versions {
		if versions[index].Status == workflow.VersionStatusDraft &&
			(draft == nil || versions[index].Version > draft.Version) {
			copy := versions[index]
			draft = &copy
		}
	}
	if draft == nil {
		draft, err = p.Deps.Workflows.CreateDraft(ctx, operation.WorkspaceID, workflowID, teamorch.DefaultActor)
		if err != nil {
			return nil, false, err
		}
	}
	matches, err := declarativeDraftMatches(*draft, spec)
	if err != nil {
		return nil, false, err
	}
	if matches {
		return draft, true, nil
	}
	updated, err := p.Deps.Workflows.UpdateDraft(ctx, operation.WorkspaceID, workflowID,
		draft.Version, draft.UpdatedAt, workflow.DraftInput{
			TriggerConfig: spec.TriggerConfig, GraphDefinition: spec.GraphDefinition,
			CreatedBy: teamorch.DefaultActor,
		})
	return updated, false, err
}

func declarativeDraftMatches(
	draft workflow.TeamWorkflowVersion,
	spec teamforge.FrozenDeclarativeWorkflowSpecV1,
) (bool, error) {
	draftHash, err := declarativeCompiledGraphHash(teamforge.FrozenDeclarativeWorkflowSpecV1{
		TriggerConfig: draft.TriggerConfig, GraphDefinition: draft.GraphDefinition,
	})
	if err != nil {
		return false, err
	}
	specHash, err := declarativeCompiledGraphHash(spec)
	return err == nil && draftHash == specHash, err
}

func (p *ProductionPhases) handleCompilerCandidate(ctx context.Context, operation teamorch.OperationContext) (teamorch.OperationResult, error) {
	workflowID, workflowVersion, err := compilerCandidateWorkflowVersion(operation)
	if err != nil {
		return teamorch.OperationResult{}, &teamorch.OperationError{Class: teameval.FailureClassCompile,
			Code: "candidate_workflow_version_unavailable", Cause: err}
	}
	evaluation, err := p.Evaluate(ctx, teamorch.RoundContext{
		WorkspaceID: operation.WorkspaceID, BuildRunID: operation.BuildRunID,
		RoundNo: operation.Revision.RevisionNo, CandidateAttempt: operation.PhysicalAttempt,
		FrozenWorkflowID: workflowID, FrozenWorkflowVersion: workflowVersion,
		Run: operation.Run,
	})
	if err != nil {
		return teamorch.OperationResult{}, &teamorch.OperationError{Class: teameval.FailureClassRuntimeInfrastructure,
			Code: "candidate_run_failed", Retryable: true, Cause: err}
	}
	evidence, marshalErr := json.Marshal(evaluation)
	if marshalErr != nil {
		return teamorch.OperationResult{}, marshalErr
	}
	if evaluation.Diagnosis.Class == teameval.FailureClassPass {
		return teamorch.OperationResult{OutputHash: evaluation.CandidateRef, Evidence: evidence, Evaluation: &evaluation}, nil
	}
	code := strings.TrimSpace(evaluation.Diagnosis.OriginalErrorCode)
	if code == "" {
		code = string(evaluation.Diagnosis.Class)
	}
	return teamorch.OperationResult{Evaluation: &evaluation}, &teamorch.OperationError{
		Class: evaluation.Diagnosis.Class, Code: code,
		Retryable: candidateFailureRetryable(evaluation.Diagnosis, operation.PhysicalAttempt),
		Evidence:  evidence, Evaluation: &evaluation,
	}
}

func compilerCandidateWorkflowVersion(operation teamorch.OperationContext) (string, int, error) {
	var workflowCompile *teamforge.ChangeOperationV1
	for index := range operation.ChangeSet.Operations {
		change := &operation.ChangeSet.Operations[index]
		if change.Type == teamforge.OperationWorkflowCompile {
			workflowCompile = change
			break
		}
	}
	if workflowCompile == nil {
		return "", 0, nil
	}
	for _, step := range operation.Steps {
		if step.OperationID != workflowCompile.OperationID ||
			step.OperationType != string(teamforge.OperationWorkflowCompile) ||
			(step.Status != teambuild.OperationStatusSucceeded && step.Status != teambuild.OperationStatusSkipped) {
			continue
		}
		return compilerWorkflowVersionFromEvidence(
			step.EvidenceJSON, workflowCompile.Compiler, workflowCompile.Target,
		)
	}
	return "", 0, errors.New("workflow_compile success evidence is missing")
}

func compilerWorkflowVersionFromEvidence(
	raw json.RawMessage,
	expectedCompiler, expectedWorkflowID string,
) (string, int, error) {
	var envelope struct {
		compilerWorkflowVersionEvidence
		ToolResult json.RawMessage `json:"tool_result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return "", 0, err
	}
	evidence := envelope.compilerWorkflowVersionEvidence
	if evidence.WorkflowID == "" && evidence.WorkflowVersion == 0 &&
		evidence.Compiler == teamforge.CompilerWorkflowV1 && len(envelope.ToolResult) != 0 {
		var legacy struct {
			WorkflowID string `json:"workflow_id"`
			Committed  bool   `json:"committed"`
			Version    int    `json:"version"`
		}
		if err := json.Unmarshal(envelope.ToolResult, &legacy); err != nil {
			return "", 0, err
		}
		if legacy.Committed {
			evidence.WorkflowID = legacy.WorkflowID
			evidence.WorkflowVersion = legacy.Version
			evidence.WorkflowRef = fmt.Sprintf("workflow-version:%d", legacy.Version)
		}
	}
	workflowID := strings.TrimSpace(evidence.WorkflowID)
	if evidence.Compiler != expectedCompiler || workflowID == "" ||
		workflowID != strings.TrimSpace(expectedWorkflowID) || evidence.WorkflowVersion < 1 ||
		evidence.WorkflowRef != fmt.Sprintf("workflow-version:%d", evidence.WorkflowVersion) {
		return "", 0, errors.New("workflow_compile evidence is invalid")
	}
	return workflowID, evidence.WorkflowVersion, nil
}

func candidateFailureRetryable(diagnosis teameval.TypedDiagnosis, attempt int) bool {
	if diagnosis.RevisionAction != teameval.RevisionActionResumeSameRevision {
		return false
	}
	if diagnosis.OriginalErrorCode == string(teamrun.ErrorCodeExecutionUnrecoverable) {
		return attempt > 0 && attempt < 3
	}
	return true
}

func (p *ProductionPhases) handleCompilerPublish(ctx context.Context, operation teamorch.OperationContext) (teamorch.OperationResult, error) {
	run, err := p.Deps.Build.GetBuildRun(ctx, operation.WorkspaceID, operation.BuildRunID)
	if err != nil {
		return teamorch.OperationResult{}, &teamorch.OperationError{Class: teameval.FailureClassRuntimeInfrastructure,
			Code: "publish_run_reload_failed", Retryable: true, Cause: err}
	}
	if run.Status == teambuild.StatusPassed && run.FinalRef != nil {
		return teamorch.OperationResult{Skip: true, OutputHash: run.FinalRef.Ref,
			Evidence: json.RawMessage(`{"publication":"already_completed"}`)}, nil
	}
	if run.Status == teambuild.StatusRoundRunning {
		if _, err := p.Deps.Build.TransitionStatus(ctx, operation.WorkspaceID, operation.BuildRunID,
			teambuild.StatusRoundRunning, teambuild.StatusPublishing, teamorch.DefaultActor,
			fmt.Sprintf("compiler revision %d candidate passed", operation.Revision.RevisionNo)); err != nil {
			return teamorch.OperationResult{}, &teamorch.OperationError{Class: teameval.FailureClassRuntimeInfrastructure,
				Code: "publish_transition_failed", Retryable: true, Cause: err}
		}
	} else if run.Status != teambuild.StatusPublishing {
		return teamorch.OperationResult{}, &teamorch.OperationError{Class: teameval.FailureClassGovernance,
			Code: "publish_run_status_invalid", Cause: fmt.Errorf("status=%s", run.Status)}
	}
	if err := p.restoreCompilerCandidate(ctx, operation); err != nil {
		return teamorch.OperationResult{}, &teamorch.OperationError{Class: teameval.FailureClassRuntimeInfrastructure,
			Code: "publish_candidate_restore_failed", Retryable: true, Cause: err}
	}
	if err := p.PublishStep(ctx, operation.WorkspaceID, operation.BuildRunID); err != nil {
		if errors.Is(err, teamorch.ErrCompilerPublishBudgetExhausted) {
			return teamorch.OperationResult{}, &teamorch.OperationError{
				Class: teameval.FailureClassBudgetExhausted,
				Code:  string(teameval.FailureClassBudgetExhausted),
				Evidence: json.RawMessage(
					`{"publication":"blocked","reason":"budget_exhausted"}`,
				),
			}
		}
		return teamorch.OperationResult{}, &teamorch.OperationError{Class: teameval.FailureClassRuntimeInfrastructure,
			Code: "publish_operation_failed", Retryable: true, Cause: err}
	}
	p.mu.Lock()
	candidate := p.lastCandidate
	p.mu.Unlock()
	return teamorch.OperationResult{OutputHash: candidate.ContentHash,
		Evidence: json.RawMessage(`{"publication":"completed"}`)}, nil
}

func (p *ProductionPhases) restoreCompilerCandidate(ctx context.Context, operation teamorch.OperationContext) error {
	p.mu.Lock()
	if p.lastCandidate != nil {
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()
	var candidateHash string
	for _, step := range operation.Steps {
		if step.OperationType == string(teamforge.OperationCandidateRun) && step.Status == teambuild.OperationStatusSucceeded {
			candidateHash = step.OutputHash
		}
	}
	if candidateHash == "" {
		return errors.New("candidate_run success evidence is unavailable")
	}
	team, err := p.targetTeam(ctx, operation.WorkspaceID, operation.Run)
	if err != nil || team == nil {
		return fmt.Errorf("resolve candidate team: %w", err)
	}
	workflowID, err := p.targetWorkflowID(ctx, operation.WorkspaceID, operation.Run, team)
	if err != nil {
		return err
	}
	record, err := (&pgPublicationRequests{pool: p.Deps.Pool}).findCandidateForBuild(ctx, operation.WorkspaceID, operation.BuildRunID, operation.Revision.RevisionNo, teambuild.SourceRoleFixedWorkflowRoot, "", candidateHash)
	if err != nil {
		return err
	}
	if record.Request.Candidate.WorkflowID != workflowID {
		return errors.New("retained candidate workflow identity changed")
	}
	candidate, err := restoreProductCandidate(record)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.lastCandidate = candidate
	p.mu.Unlock()
	return nil
}
