// Package teamevaluations constructs post-template evaluation runs from an
// immutable template lineage and the existing compiler pipeline. It owns no
// team, roster, workflow, or publication writes.
package teamevaluations

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/build/teamforge"
	"github.com/jinyitao123/weave/internal/kernel/org"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

const (
	placeholderPrefix  = "post_evaluation_placeholder"
	progressPathPrefix = "/v1/internal/team-build-runs/"
)

var (
	ErrUnavailable          = errors.New("team evaluation service unavailable")
	ErrIdempotencyConflict  = errors.New("team evaluation idempotency conflict")
	ErrConcurrentEvaluation = errors.New("team already has an active evaluation")
	ErrNotUnevaluated       = errors.New("team is not unevaluated")
	ErrTemplateLineage      = errors.New("team template lineage is unavailable")
)

type Problem struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type ValidationError struct{ Problems []Problem }

func (e *ValidationError) Error() string {
	if e == nil || len(e.Problems) == 0 {
		return "team evaluation request is invalid"
	}
	return e.Problems[0].Message
}

type Request struct {
	Contract              teambuild.EvaluationContract     `json:"contract"`
	IdempotencyKey        string                           `json:"idempotency_key"`
	Budget                Budget                           `json:"budget"`
	UnmeasuredUsageWaiver *teambuild.UnmeasuredUsageWaiver `json:"unmeasured_usage_waiver,omitempty"`
}

type Budget struct {
	MaxInputTokens  int64   `json:"max_input_tokens,omitempty"`
	MaxOutputTokens int64   `json:"max_output_tokens,omitempty"`
	MaxToolCalls    int64   `json:"max_tool_calls,omitempty"`
	MaxCostUSD      float64 `json:"max_cost_usd,omitempty"`
}

type Outcome struct {
	BuildRunID  string `json:"build_run_id"`
	Status      string `json:"status"`
	ProgressURL string `json:"progress_url"`
}

type BuildStore interface {
	SetBaselineSources(*org.Store, *registry.AgentRegistry, *workflow.Store)
	CreateBuildRun(context.Context, string, string, teambuild.CreateRunParams) (teambuild.TeamBuildRun, error)
	GetBuildRun(context.Context, string, string) (teambuild.TeamBuildRun, error)
	GetLatestBlueprintRevision(context.Context, string, string) (teambuild.BlueprintRevision, error)
	GetTemplateLineageRevision(context.Context, string, string) (teambuild.BlueprintRevision, error)
	PreviewCompilerBaseline(context.Context, string, string) (teambuild.CompilerBaselinePreview, error)
	PersistCompilerAuthorizationBundle(context.Context, string, string, teambuild.CompilerAuthorizationBundle) (teambuild.BlueprintRevision, error)
	AuthorizeBuildRun(context.Context, string, string, string, *teambuild.BaselineSnapshot, ...teambuild.AuthorizeOptions) (teambuild.TeamBuildRun, teambuild.BuildAuthorizationReceipt, error)
}

type Submitter interface {
	Submit(context.Context, string, string) error
}

type DeclarativeValidator func(
	context.Context,
	string,
	string,
	machine.TriggerConfig,
	machine.GraphDefinition,
) (machine.Report, error)

type Options struct {
	OrgStore             *org.Store
	Registry             *registry.AgentRegistry
	Workflows            *workflow.Store
	DeclarativeValidator DeclarativeValidator
	RunTTL               time.Duration
	Now                  func() time.Time
}

type Service struct {
	idempotency         IdempotencyStore
	builds              BuildStore
	submitter           Submitter
	org                 *org.Store
	registry            *registry.AgentRegistry
	workflows           *workflow.Store
	declarativeValidate DeclarativeValidator
	runTTL              time.Duration
	now                 func() time.Time
}

func New(idempotency IdempotencyStore, builds BuildStore, submitter Submitter, options Options) *Service {
	if options.RunTTL <= 0 {
		options.RunTTL = 30 * time.Minute
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Service{
		idempotency: idempotency, builds: builds, submitter: submitter,
		org: options.OrgStore, registry: options.Registry, workflows: options.Workflows,
		declarativeValidate: options.DeclarativeValidator,
		runTTL:              options.RunTTL, now: options.Now,
	}
}

func (s *Service) Evaluate(
	ctx context.Context,
	workspaceID, userID, teamID string,
	request Request,
) (Outcome, error) {
	if s == nil || s.idempotency == nil || s.builds == nil || s.submitter == nil ||
		s.org == nil || s.registry == nil || s.workflows == nil {
		return Outcome{}, ErrUnavailable
	}
	if strings.TrimSpace(workspaceID) == "" || strings.TrimSpace(userID) == "" || strings.TrimSpace(teamID) == "" {
		return Outcome{}, validation("/", "evaluation_identity_required", "workspace, authenticated user, and team are required")
	}
	key, err := uuid.Parse(strings.TrimSpace(request.IdempotencyKey))
	if err != nil {
		return Outcome{}, validation("/idempotency_key", "evaluation_idempotency_key_invalid", "idempotency_key must be a UUID")
	}
	if err := validateEvaluationBudget(request.Budget); err != nil {
		return Outcome{}, validation("/budget", "evaluation_budget_invalid", err.Error())
	}
	if waiver := request.UnmeasuredUsageWaiver; waiver != nil &&
		(!waiver.Accepted || strings.TrimSpace(waiver.Reason) == "") {
		return Outcome{}, validation(
			"/unmeasured_usage_waiver", "evaluation_usage_waiver_invalid",
			"unmeasured_usage_waiver requires accepted=true and a reason",
		)
	}
	if problems := placeholderProblems(request.Contract); len(problems) != 0 {
		return Outcome{}, &ValidationError{Problems: problems}
	}
	team, err := s.org.GetTeam(ctx, workspaceID, teamID)
	if err != nil {
		return Outcome{}, err
	}
	lineage, err := s.builds.GetTemplateLineageRevision(ctx, workspaceID, teamID)
	if err != nil {
		return Outcome{}, fmt.Errorf("%w: %v", ErrTemplateLineage, err)
	}
	blueprint, err := evaluationBlueprint(lineage, teamID)
	if err != nil {
		return Outcome{}, fmt.Errorf("%w: %v", ErrTemplateLineage, err)
	}
	brief := evaluationBrief(team, blueprint, request.Budget, request.UnmeasuredUsageWaiver)
	_, normalizedContract, contractHash, err := teambuild.ValidateBuildRunDrafts(brief, request.Contract)
	if err != nil {
		return Outcome{}, validation("/contract", "evaluation_contract_invalid", err.Error())
	}
	fingerprint, err := evaluationFingerprint(
		teamID, contractHash, request.Budget, request.UnmeasuredUsageWaiver,
	)
	if err != nil {
		return Outcome{}, fmt.Errorf("fingerprint team evaluation: %w", err)
	}
	buildRunID := deterministicBuildRunID(workspaceID, key)
	record, err := s.idempotency.Claim(ctx, IdempotencyRecord{
		WorkspaceID: workspaceID, Key: key, Fingerprint: fingerprint,
		BuildRunID: buildRunID, TeamID: teamID, CreatedBy: userID,
	})
	if err != nil {
		return Outcome{}, err
	}
	if record.Fingerprint != fingerprint || record.TeamID != teamID {
		return Outcome{}, ErrIdempotencyConflict
	}
	buildRunID = record.BuildRunID
	if team.Evaluation != org.TeamEvaluationUnevaluated {
		existing, getErr := s.builds.GetBuildRun(ctx, workspaceID, buildRunID)
		if getErr != nil {
			return Outcome{}, ErrNotUnevaluated
		}
		verified, verifyErr := verifyRunIdentity(existing, teamID, normalizedContract)
		if verifyErr != nil {
			return Outcome{}, verifyErr
		}
		return evaluationOutcome(verified), nil
	}
	s.builds.SetBaselineSources(s.org, s.registry, s.workflows)
	run, err := s.ensureBuildRun(ctx, workspaceID, record.CreatedBy, buildRunID, teamID, brief, normalizedContract)
	if err != nil {
		return Outcome{}, err
	}
	if run.Status != teambuild.StatusPlanning {
		return evaluationOutcome(run), nil
	}
	revision, err := s.ensureRevision(ctx, workspaceID, buildRunID, teamID, lineage, blueprint, run)
	if err != nil {
		return Outcome{}, err
	}
	token := teambuild.BlueprintRevisionToken{
		RevisionNo: revision.RevisionNo, BlueprintHash: revision.BlueprintHash,
		ChangeSetHash: revision.ChangeSetHash,
	}
	run, _, err = s.builds.AuthorizeBuildRun(ctx, workspaceID, buildRunID, record.CreatedBy, nil, teambuild.AuthorizeOptions{
		Authority: teambuild.AuthorizationContinueBuild, RevisionToken: &token,
	})
	if err != nil {
		authorizeErr := err
		// A replay may observe the winner after planning. Only accept an actual
		// successor state from storage.
		run, err = s.builds.GetBuildRun(ctx, workspaceID, buildRunID)
		if err != nil || run.Status == teambuild.StatusPlanning {
			return Outcome{}, fmt.Errorf("authorize team evaluation: %w", authorizeErr)
		}
	}
	if run.Status == teambuild.StatusAuthorized {
		if err := s.submitter.Submit(ctx, workspaceID, buildRunID); err != nil {
			return Outcome{}, fmt.Errorf("submit team evaluation: %w", err)
		}
	}
	return evaluationOutcome(run), nil
}

func (s *Service) ensureBuildRun(
	ctx context.Context,
	workspaceID, userID, buildRunID, teamID string,
	brief teambuild.BuildBrief,
	contract teambuild.EvaluationContract,
) (teambuild.TeamBuildRun, error) {
	run, err := s.builds.GetBuildRun(ctx, workspaceID, buildRunID)
	if err == nil {
		return verifyRunIdentity(run, teamID, contract)
	}
	if !errors.Is(err, teambuild.ErrBuildRunNotFound) {
		return teambuild.TeamBuildRun{}, err
	}
	run, err = s.builds.CreateBuildRun(ctx, workspaceID, buildRunID, teambuild.CreateRunParams{
		Brief: brief, Contract: contract, CreatedBy: userID,
		ExpiresAt: s.now().UTC().Add(s.runTTL), ExecutionStrategy: teambuild.ExecutionStrategyCompilerV1,
		EvaluationTeamID: teamID, EvaluationOnly: true,
	})
	if errors.Is(err, teambuild.ErrActiveTeamEvaluation) {
		return teambuild.TeamBuildRun{}, ErrConcurrentEvaluation
	}
	if err == nil {
		return run, nil
	}
	run, reloadErr := s.builds.GetBuildRun(ctx, workspaceID, buildRunID)
	if reloadErr != nil {
		return teambuild.TeamBuildRun{}, err
	}
	return verifyRunIdentity(run, teamID, contract)
}

func (s *Service) ensureRevision(
	ctx context.Context,
	workspaceID, buildRunID, teamID string,
	lineage teambuild.BlueprintRevision,
	blueprint teambuild.TeamBlueprintV1,
	run teambuild.TeamBuildRun,
) (teambuild.BlueprintRevision, error) {
	if existing, err := s.builds.GetLatestBlueprintRevision(ctx, workspaceID, buildRunID); err == nil {
		return existing, nil
	} else if !errors.Is(err, teambuild.ErrBuildRunNotFound) {
		return teambuild.BlueprintRevision{}, fmt.Errorf("read evaluation revision: %w", err)
	}
	preview, err := s.builds.PreviewCompilerBaseline(ctx, workspaceID, buildRunID)
	if err != nil {
		return teambuild.BlueprintRevision{}, fmt.Errorf("preview evaluation baseline: %w", err)
	}
	if preview.Snapshot == nil || preview.CapturedAt == nil {
		return teambuild.BlueprintRevision{}, errors.New("evaluation baseline snapshot is unavailable")
	}
	baseline, err := teamforge.AuthorizedBaselineV1(teambuild.TeamBuildRun{
		Mode: teambuild.ModeOptimize, Baseline: preview.Snapshot,
	}, blueprint)
	if err != nil {
		return teambuild.BlueprintRevision{}, fmt.Errorf("project evaluation baseline: %w", err)
	}
	workflowTarget, err := evaluationWorkflowTarget(*preview.Snapshot)
	if err != nil {
		return teambuild.BlueprintRevision{}, err
	}
	var changeSet teamforge.ChangeSetV1
	switch blueprint.Workflow.Mode {
	case teambuild.BlueprintWorkflowTemplate:
		changeSet, err = teamforge.CompileEvaluationChangeSetV1(baseline, blueprint, nil, workflowTarget)
	case teambuild.BlueprintWorkflowDeclarativeV1:
		if s.declarativeValidate == nil {
			return teambuild.BlueprintRevision{}, ErrUnavailable
		}
		spec, extractErr := sourceDeclarativeSpec(lineage.ChangeSetJSON)
		if extractErr != nil {
			return teambuild.BlueprintRevision{}, fmt.Errorf("derive declarative evaluation lineage: %w", extractErr)
		}
		bindings, bindErr := teamforge.ResolveDeclarativeWorkerBindingsV1(spec, blueprint, *preview.Snapshot)
		if bindErr != nil {
			return teambuild.BlueprintRevision{}, fmt.Errorf("bind declarative evaluation roster: %w", bindErr)
		}
		frozen, freezeErr := teamforge.FreezeDeclarativeWorkflowSpecV1(
			spec, bindings,
			teamforge.DeclarativeBuildBindingV1{
				BuildRunID: buildRunID, BriefHash: run.BriefHash, ContractHash: run.ContractHash,
				AssetScope: preview.Snapshot.AssetScope, BaselineHash: preview.BaselineHash,
			},
			func(trigger machine.TriggerConfig, graph machine.GraphDefinition) (machine.Report, error) {
				return s.declarativeValidate(ctx, workspaceID, teamID, trigger, graph)
			},
		)
		if freezeErr != nil {
			return teambuild.BlueprintRevision{}, fmt.Errorf("freeze declarative evaluation workflow: %w", freezeErr)
		}
		blueprint.Workflow.DeclarativeSpecHash = frozen.SpecHash
		baseline, err = teamforge.AuthorizedBaselineV1(teambuild.TeamBuildRun{
			Mode: teambuild.ModeOptimize, Baseline: preview.Snapshot,
		}, blueprint)
		if err == nil {
			changeSet, err = teamforge.CompileEvaluationChangeSetV1(baseline, blueprint, &frozen, workflowTarget)
		}
	default:
		return teambuild.BlueprintRevision{}, fmt.Errorf("unsupported template workflow lineage %q", blueprint.Workflow.Mode)
	}
	if err != nil {
		return teambuild.BlueprintRevision{}, fmt.Errorf("compile evaluation change set: %w", err)
	}
	blueprintJSON, err := json.Marshal(blueprint)
	if err != nil {
		return teambuild.BlueprintRevision{}, err
	}
	blueprintHash, err := blueprint.BlueprintHash()
	if err != nil {
		return teambuild.BlueprintRevision{}, err
	}
	changeSetJSON, err := changeSet.CanonicalBytes()
	if err != nil {
		return teambuild.BlueprintRevision{}, err
	}
	changeSetHash, err := changeSet.CanonicalHash()
	if err != nil {
		return teambuild.BlueprintRevision{}, err
	}
	return s.builds.PersistCompilerAuthorizationBundle(ctx, workspaceID, buildRunID, teambuild.CompilerAuthorizationBundle{
		RevisionNo: 1, BlueprintJSON: blueprintJSON, BlueprintHash: blueprintHash,
		ChangeSetJSON: changeSetJSON, ChangeSetHash: changeSetHash,
		BaselineHash: preview.BaselineHash, BaselineCapturedAt: preview.CapturedAt,
		EvaluationContractHash: run.ContractHash,
	})
}

func evaluationWorkflowTarget(snapshot teambuild.BaselineSnapshot) (string, error) {
	target := ""
	for _, workflowRef := range snapshot.Workflows {
		if workflowRef.Status != workflow.WorkflowStatusActive || workflowRef.Draft == nil {
			continue
		}
		if target != "" {
			return "", errors.New("evaluation baseline requires exactly one active draft workflow")
		}
		target = strings.TrimSpace(workflowRef.WorkflowID)
	}
	if target == "" {
		return "", errors.New("evaluation baseline has no active draft workflow")
	}
	return target, nil
}

func evaluationBlueprint(lineage teambuild.BlueprintRevision, teamID string) (teambuild.TeamBlueprintV1, error) {
	var blueprint teambuild.TeamBlueprintV1
	decoder := json.NewDecoder(strings.NewReader(string(lineage.BlueprintJSON)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&blueprint); err != nil {
		return blueprint, err
	}
	if blueprint.Mode != teambuild.ModeCreate || len(blueprint.Members) == 0 {
		return blueprint, errors.New("template lineage blueprint is not a create blueprint")
	}
	blueprint.Mode = teambuild.ModeOptimize
	blueprint.TeamID = teamID
	blueprint.NewTeamName = ""
	for index := range blueprint.Members {
		blueprint.Members[index].ManagementMode = teambuild.BlueprintManagementPreserveExisting
	}
	blueprint.RevisionPolicy.MaxRevisions = 1
	if err := teambuild.ValidateTeamBlueprintV1(blueprint); err != nil {
		return blueprint, err
	}
	return blueprint, nil
}

func evaluationBrief(
	team org.Team,
	blueprint teambuild.TeamBlueprintV1,
	budget Budget,
	waiver *teambuild.UnmeasuredUsageWaiver,
) teambuild.BuildBrief {
	direction := strings.TrimSpace(team.Objective)
	if direction == "" {
		direction = strings.TrimSpace(blueprint.Purpose)
	}
	return teambuild.BuildBrief{
		SchemaVersion: 1, Mode: teambuild.ModeOptimize,
		BusinessDirection:     direction,
		Task:                  "Evaluate the existing team against the submitted real contract without changing team assets.",
		TeamID:                team.ID,
		SuccessCriteria:       []string{"The frozen evaluation contract passes against the unchanged team assets."},
		Constraints:           []string{"evaluation_only", "preserve_existing_roster", "manual_admin_authorization"},
		Prohibitions:          []string{"Do not patch the blueprint or mutate team, roster, agent, or workflow assets during evaluation."},
		UnmeasuredUsageWaiver: cloneEvaluationUsageWaiver(waiver),
		AllowedAssets: teambuild.AssetScope{
			AllowedKinds: []string{"agent", "team", "workflow"},
			Refs:         []teambuild.AssetRef{{Kind: "team", ID: team.ID}},
		},
		RoundBudget: evaluationBuildBudget(budget),
		TotalBudget: evaluationBuildBudget(budget),
	}
}

func verifyRunIdentity(
	run teambuild.TeamBuildRun,
	teamID string,
	contract teambuild.EvaluationContract,
) (teambuild.TeamBuildRun, error) {
	contractHash, err := contract.Hash()
	if err != nil {
		return teambuild.TeamBuildRun{}, err
	}
	if run.ExecutionStrategy != teambuild.ExecutionStrategyCompilerV1 ||
		!run.EvaluationOnly || run.EvaluationTeamID != teamID || run.ContractHash != contractHash {
		return teambuild.TeamBuildRun{}, ErrIdempotencyConflict
	}
	return run, nil
}

func placeholderProblems(contract teambuild.EvaluationContract) []Problem {
	var problems []Problem
	placeholder := func(value string) bool {
		return strings.HasPrefix(strings.ToLower(strings.TrimSpace(value)), placeholderPrefix)
	}
	for index, scenario := range contract.PublicScenarios {
		if placeholder(scenario.ID) {
			problems = append(problems, Problem{Path: fmt.Sprintf("/contract/public_scenarios/%d/id", index), Code: "evaluation_placeholder_forbidden", Message: "post-evaluation placeholder scenario IDs are forbidden"})
		}
	}
	for index, scenario := range contract.PerturbationScenarios {
		if placeholder(scenario.ID) {
			problems = append(problems, Problem{Path: fmt.Sprintf("/contract/perturbation_scenarios/%d/id", index), Code: "evaluation_placeholder_forbidden", Message: "post-evaluation placeholder perturbation IDs are forbidden"})
		}
		if placeholder(scenario.BaseScenarioID) {
			problems = append(problems, Problem{Path: fmt.Sprintf("/contract/perturbation_scenarios/%d/base_scenario_id", index), Code: "evaluation_placeholder_forbidden", Message: "post-evaluation placeholder base scenario IDs are forbidden"})
		}
	}
	return problems
}

func sourceDeclarativeSpec(raw json.RawMessage) (teamforge.DeclarativeWorkflowSpecV1, error) {
	var changeSet teamforge.ChangeSetV1
	if err := json.Unmarshal(raw, &changeSet); err != nil {
		return teamforge.DeclarativeWorkflowSpecV1{}, err
	}
	for _, operation := range changeSet.Operations {
		if operation.Type != teamforge.OperationWorkflowCompile {
			continue
		}
		var input struct {
			Mode       string                                    `json:"mode"`
			FrozenSpec teamforge.FrozenDeclarativeWorkflowSpecV1 `json:"frozen_spec"`
		}
		if err := json.Unmarshal(operation.Input, &input); err != nil {
			return teamforge.DeclarativeWorkflowSpecV1{}, err
		}
		if input.Mode == teambuild.BlueprintWorkflowDeclarativeV1 {
			return input.FrozenSpec.Spec, nil
		}
	}
	return teamforge.DeclarativeWorkflowSpecV1{}, errors.New("template lineage has no declarative workflow operation")
}

func evaluationFingerprint(
	teamID, contractHash string,
	budget Budget,
	waiver *teambuild.UnmeasuredUsageWaiver,
) (string, error) {
	raw, err := json.Marshal(struct {
		TeamID                string                           `json:"team_id"`
		ContractHash          string                           `json:"contract_hash"`
		MaxCostUSD            float64                          `json:"max_cost_usd"`
		MaxInputTokens        int64                            `json:"max_input_tokens,omitempty"`
		MaxOutputTokens       int64                            `json:"max_output_tokens,omitempty"`
		MaxToolCalls          int64                            `json:"max_tool_calls,omitempty"`
		UnmeasuredUsageWaiver *teambuild.UnmeasuredUsageWaiver `json:"unmeasured_usage_waiver,omitempty"`
	}{
		TeamID: teamID, ContractHash: contractHash, MaxCostUSD: budget.MaxCostUSD,
		MaxInputTokens: budget.MaxInputTokens, MaxOutputTokens: budget.MaxOutputTokens,
		MaxToolCalls:          budget.MaxToolCalls,
		UnmeasuredUsageWaiver: cloneEvaluationUsageWaiver(waiver),
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func validateEvaluationBudget(budget Budget) error {
	if budget.MaxInputTokens < 0 || budget.MaxOutputTokens < 0 || budget.MaxToolCalls < 0 ||
		budget.MaxCostUSD < 0 || math.IsNaN(budget.MaxCostUSD) || math.IsInf(budget.MaxCostUSD, 0) {
		return errors.New("budget bounds must be finite and non-negative")
	}
	if budget.MaxInputTokens == 0 && budget.MaxOutputTokens == 0 &&
		budget.MaxToolCalls == 0 && budget.MaxCostUSD == 0 {
		return errors.New("budget must declare at least one positive bound")
	}
	return nil
}

func evaluationBuildBudget(budget Budget) teambuild.Budget {
	return teambuild.Budget{
		MaxInputTokens: budget.MaxInputTokens, MaxOutputTokens: budget.MaxOutputTokens,
		MaxToolCalls: budget.MaxToolCalls, MaxCostUSD: budget.MaxCostUSD,
	}
}

func cloneEvaluationUsageWaiver(value *teambuild.UnmeasuredUsageWaiver) *teambuild.UnmeasuredUsageWaiver {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func deterministicBuildRunID(workspaceID string, key uuid.UUID) string {
	sum := sha256.Sum256([]byte(workspaceID + "\x00team-evaluation\x00" + key.String()))
	return "br-eval-" + hex.EncodeToString(sum[:16])
}

func evaluationOutcome(run teambuild.TeamBuildRun) Outcome {
	return Outcome{
		BuildRunID: run.BuildRunID, Status: run.Status,
		ProgressURL: progressPathPrefix + run.BuildRunID + "/progress",
	}
}

func validation(path, code, message string) error {
	return &ValidationError{Problems: []Problem{{Path: path, Code: code, Message: message}}}
}
