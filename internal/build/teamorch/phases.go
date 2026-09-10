package teamorch

// Production round phases (ticket T12): the build phase drives the meta-team
// employees through mcphost.AgentRunner with the teamforge tool set decided
// by teamforge.DecideToolSet; the evaluate phase freezes a publication
// candidate, runs it through the teamrun executor, gathers evidence, and
// evaluates every hard gate; the publish step releases the passed candidate
// through the same content-hash CAS path the platform publication store
// owns.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/pgstore"
	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/fanout"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/base/storeext"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
	"github.com/jinyitao123/weave/internal/base/teamrun"
	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/build/teameval"
	"github.com/jinyitao123/weave/internal/build/teamforge"
	"github.com/jinyitao123/weave/internal/kernel/audit"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/delivery"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	"github.com/jinyitao123/weave/internal/kernel/mcphost"
	"github.com/jinyitao123/weave/internal/kernel/mcpregistry"
	"github.com/jinyitao123/weave/internal/kernel/org"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/runtimellm"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/jinyitao123/weave/internal/kernel/schedule"
	"github.com/jinyitao123/weave/internal/kernel/skills"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

// PhaseDeps aggregates every store and seam the production phases need. The
// assembly deliberately mirrors internal/api/teamforge_wiring.go's deps
// semantics (read side teamForgeDeps, write side teamForgeWriteDeps) so the
// controller's agent runs receive exactly the same teamforge tool surface as
// the conversation wiring — keep the two in sync when the tool surface
// changes.
type PhaseDeps struct {
	// Pool begins the candidate-build and credential transactions.
	Pool *pgxpool.Pool
	// Store is the loom namespace store used by AgentRunner and the
	// teamforge/teameval namespace readers (skills, run evidence).
	Store *pgstore.PGStore

	Build        *teambuild.Store
	Agents       *registry.AgentRegistry
	TeamWorkers  *registry.TeamWorkerRepository
	Teams        *org.Store
	Workflows    *workflow.Store
	MCPs         *mcpregistry.Store
	Providers    *credentials.Store
	Runtimes     *runtimes.Store
	Tasks        *taskqueue.Store
	Deliverables *deliverable.Store
	Snapshots    *snapshot.Store
	Audit        *audit.Store
	Delivery     *delivery.Store
	Skills       *skills.Store
	Schedules    *schedule.Store
	Descriptors  *compiler.DescriptorRegistry
	Fanout       *fanout.Store
	Drafts       *teamforge.DraftRegistry
	// ConstructionRoles optionally replaces the build-controlled config and
	// graph executors; nil selects the production executor owned here.
	ConstructionRoles teambuild.ConstructionRoleExecutor
	// SemanticJudge optionally replaces the current metateam-backed executor.
	// Nil keeps the M2b compatibility adapter; M2c supplies the build-native
	// implementation through this seam.
	SemanticJudge teambuild.SemanticJudgeExecutor
	// BlueprintPatchPlanner optionally replaces the build-controlled planner;
	// nil selects the production executor owned by this package.
	BlueprintPatchPlanner teambuild.BlueprintPatchPlannerExecutor
	// CLIExecutor is the same remote runtime path used by ordinary published
	// TeamWorkflow runs. Candidate evaluation must not silently drop it.
	CLIExecutor mcphost.RemoteEngineExecutor

	// LLM drives the meta-team agent runs. Production callers inject the
	// routed LLM; tests inject testutil.ScriptedLLM. LLMResolver takes
	// precedence in production so every workspace receives its own immutable
	// provider snapshot instead of sharing another tenant's routing state.
	LLM         contract.LLM
	LLMResolver interface {
		ForWorkspace(ctx context.Context, workspaceID string) (contract.LLM, error)
	}
	// HostFactoryForSnapshot replaces the runtime host factory for candidate
	// test runs (teamrun.WorkflowSerialRuntime.HostFactoryForSnapshot). Nil
	// keeps the production factory for every run.
	HostFactoryForSnapshot func(buildRunID, candidateHash string) workflow.RuntimeHostFactory
}

// ProductionPhases is the production implementation of the controller's
// Phases interface plus the publish step that runs after the controller
// reports publishing.
type ProductionPhases struct {
	Deps PhaseDeps

	builder *workflow.CandidateBuilder

	mu            sync.Mutex
	lastCandidate *workflow.PublicationCandidate
}

// NewPhases validates the dependency set and constructs the production
// phases. It fails fast when a required store is missing so wiring mistakes
// surface at startup instead of mid-round.
func NewPhases(deps PhaseDeps) (*ProductionPhases, error) {
	if deps.Pool == nil || deps.Store == nil || deps.Build == nil ||
		deps.Agents == nil || deps.TeamWorkers == nil || deps.Teams == nil ||
		deps.Workflows == nil || deps.MCPs == nil || deps.Providers == nil ||
		deps.Runtimes == nil || deps.Tasks == nil ||
		deps.Deliverables == nil || deps.Snapshots == nil || deps.Audit == nil ||
		deps.Delivery == nil || deps.Skills == nil || deps.Schedules == nil ||
		deps.Descriptors == nil || deps.Fanout == nil || deps.Drafts == nil ||
		(deps.LLM == nil && deps.LLMResolver == nil) {
		return nil, errors.New("team forge phases: dependencies are not fully configured")
	}
	builder := workflow.NewCandidateBuilder(
		deps.Workflows,
		deps.Agents,
		deps.Delivery,
		deps.Skills,
		deps.Providers,
		deps.Schedules,
		deps.Descriptors,
	)
	return &ProductionPhases{Deps: deps, builder: builder}, nil
}

func (p *ProductionPhases) llmForWorkspace(
	ctx context.Context, workspaceID string,
) (contract.LLM, error) {
	if p != nil && p.Deps.LLMResolver != nil {
		llm, err := p.Deps.LLMResolver.ForWorkspace(ctx, workspaceID)
		if err != nil {
			return nil, fmt.Errorf("resolve workspace LLM: %w", err)
		}
		if llm == nil {
			return nil, errors.New("resolve workspace LLM: resolver returned nil")
		}
		return llm, nil
	}
	if p != nil && p.Deps.LLM != nil {
		return p.Deps.LLM, nil
	}
	return nil, errors.New("workspace LLM unavailable")
}

// ErrBlueprintPatchPlannerBudgetExhausted is returned before starting the
// planner when no budget balance remains, or after charging the planner when
// that final charge reaches/exceeds a frozen limit. The controller maps both
// cases to the stable budget_exhausted terminal class and never appends a
// Blueprint revision.
var ErrBlueprintPatchPlannerBudgetExhausted = errors.New("blueprint patch planner budget exhausted")

// ErrCompilerPublishBudgetExhausted reports a G5 rejection that has already
// committed publishing -> blocked atomically. Callers must preserve the
// budget failure class instead of wrapping it as retryable infrastructure.
var ErrCompilerPublishBudgetExhausted = errors.New("compiler publish budget exhausted")

// PlanBlueprintPatch invokes the dedicated read-only patch planner only after
// the controller has persisted a complete business-quality report. The
// planner returns one strict document; all application, compilation, CAS and
// storage authority stays in the platform controller/store.
func (p *ProductionPhases) PlanBlueprintPatch(
	ctx context.Context,
	round RoundContext,
	report teambuild.EvaluationReport,
	diagnosis teameval.TypedDiagnosis,
) (teambuild.BlueprintPatchV1, error) {
	if p == nil || p.Deps.Build == nil {
		return teambuild.BlueprintPatchV1{}, errors.New("blueprint patch planner dependencies unavailable")
	}
	if diagnosis.Class != teameval.FailureClassBusinessQuality ||
		diagnosis.RevisionAction != teameval.RevisionActionRequestBlueprintPatch {
		return teambuild.BlueprintPatchV1{}, errors.New("blueprint patch planner requires a business-quality diagnosis")
	}
	reportHash, err := report.Hash()
	if err != nil {
		return teambuild.BlueprintPatchV1{}, fmt.Errorf("hash source report: %w", err)
	}
	persisted, err := p.Deps.Build.GetRoundReport(ctx, round.WorkspaceID, round.BuildRunID, round.RoundNo)
	if err != nil || persisted.ReportHash != reportHash {
		return teambuild.BlueprintPatchV1{}, errors.New("blueprint patch planner source report is not the current immutable report")
	}
	revision, err := p.Deps.Build.GetLatestBlueprintRevision(ctx, round.WorkspaceID, round.BuildRunID)
	if err != nil || revision.RevisionNo != round.RoundNo {
		return teambuild.BlueprintPatchV1{}, errors.New("blueprint patch planner revision is not current")
	}
	var blueprint teambuild.TeamBlueprintV1
	if err := json.Unmarshal(revision.BlueprintJSON, &blueprint); err != nil {
		return teambuild.BlueprintPatchV1{}, fmt.Errorf("decode current blueprint: %w", err)
	}
	valueHashes := make(map[string]string, len(blueprint.RevisionPolicy.AllowedPatchPaths))
	for _, path := range blueprint.RevisionPolicy.AllowedPatchPaths {
		probe := teambuild.BlueprintPatchV1{Changes: []teambuild.BlueprintFieldPatchV1{{Path: path}}}
		if err := validateCompilerPatchExecutable(blueprint, probe); err != nil {
			continue
		}
		hash, err := teambuild.BlueprintPatchValueHashV1(blueprint, path)
		if err != nil {
			return teambuild.BlueprintPatchV1{}, fmt.Errorf("hash authorized patch path %s: %w", path, err)
		}
		valueHashes[path] = hash
	}
	if len(valueHashes) == 0 {
		return teambuild.BlueprintPatchV1{}, errors.New("blueprint_patch_path_not_executable: revision policy has no connected deterministic patch path")
	}
	_, err = p.Deps.Build.GetBlueprintPatchPlanningAttempt(
		ctx, round.WorkspaceID, round.BuildRunID, revision.RevisionNo,
		teambuild.SourceRoleBlueprintPatchPlanner,
	)
	attemptExists := err == nil
	if !attemptExists && !errors.Is(err, teambuild.ErrBlueprintPatchPlanningAttemptNotFound) {
		return teambuild.BlueprintPatchV1{}, fmt.Errorf("inspect blueprint patch planning attempt: %w", err)
	}
	if !attemptExists {
		preDecision, err := p.Deps.Build.EvaluateBudget(
			ctx, round.WorkspaceID, round.BuildRunID, round.RoundNo,
			round.Run.RoundBudget, round.Run.TotalBudget,
		)
		if err != nil {
			return teambuild.BlueprintPatchV1{}, fmt.Errorf("evaluate blueprint patch budget before planner: %w", err)
		}
		if len(preDecision.ExceededDims) > 0 {
			return teambuild.BlueprintPatchV1{}, fmt.Errorf("%w: no balance before planner", ErrBlueprintPatchPlannerBudgetExhausted)
		}
	}
	payload, err := json.Marshal(struct {
		Instruction       string                     `json:"instruction"`
		SourceReportHash  string                     `json:"source_report_hash"`
		Report            teambuild.EvaluationReport `json:"evaluation_report"`
		Diagnosis         teameval.TypedDiagnosis    `json:"typed_diagnosis"`
		Blueprint         teambuild.TeamBlueprintV1  `json:"current_blueprint"`
		ExpectedValueHash map[string]string          `json:"expected_value_hash_by_path"`
	}{
		Instruction:      "Return exactly one BlueprintPatchV1 JSON object and no markdown or prose. Use only an allowed exact path and its supplied expected_value_hash. Do not change scope, governance, revision_policy, workflow mode/template, member identity, or management mode.",
		SourceReportHash: reportHash, Report: report, Diagnosis: diagnosis,
		Blueprint: blueprint, ExpectedValueHash: valueHashes,
	})
	if err != nil {
		return teambuild.BlueprintPatchV1{}, err
	}
	executor := p.Deps.BlueprintPatchPlanner
	if executor == nil {
		executor = buildBlueprintPatchPlannerExecutor{phases: p, round: round}
	}
	planned, err := executor.Execute(ctx, teambuild.BlueprintPatchPlannerRequest{
		WorkspaceID: round.WorkspaceID, BuildRunID: round.BuildRunID,
		RevisionNo: revision.RevisionNo, Run: round.Run,
		Payload: payload, SourceReportHash: reportHash,
	})
	if err != nil {
		return teambuild.BlueprintPatchV1{}, err
	}
	decision, err := p.Deps.Build.EvaluateBudget(ctx, round.WorkspaceID, round.BuildRunID, round.RoundNo,
		round.Run.RoundBudget, round.Run.TotalBudget)
	if err != nil {
		return teambuild.BlueprintPatchV1{}, fmt.Errorf("evaluate blueprint patch budget: %w", err)
	}
	if len(decision.ExceededDims) > 0 {
		return teambuild.BlueprintPatchV1{}, fmt.Errorf("%w: planner charge consumed remaining balance", ErrBlueprintPatchPlannerBudgetExhausted)
	}
	patch, err := decodeBlueprintPatchPlannerOutput(planned.Output)
	if err != nil {
		return teambuild.BlueprintPatchV1{}, err
	}
	if patch.SourceReportHash != reportHash || patch.FailureClass != teambuild.BlueprintPatchFailureBusinessQuality {
		return teambuild.BlueprintPatchV1{}, errors.New("blueprint patch planner output is not bound to the current report")
	}
	if err := teambuild.ValidateBlueprintPatchV1(patch, blueprint.RevisionPolicy); err != nil {
		return teambuild.BlueprintPatchV1{}, fmt.Errorf("blueprint patch planner output is unauthorized: %w", err)
	}
	return patch, nil
}

type buildBlueprintPatchPlannerExecutor struct {
	phases *ProductionPhases
	round  RoundContext
}

func (e buildBlueprintPatchPlannerExecutor) Execute(
	ctx context.Context,
	request teambuild.BlueprintPatchPlannerRequest,
) (teambuild.BlueprintPatchPlannerResult, error) {
	p := e.phases
	if p == nil || p.Deps.Build == nil || request.WorkspaceID != e.round.WorkspaceID ||
		request.BuildRunID != e.round.BuildRunID || request.RevisionNo != e.round.RoundNo ||
		request.SourceReportHash == "" {
		return teambuild.BlueprintPatchPlannerResult{}, errors.New("blueprint patch planner request identity is invalid")
	}
	attempt, err := p.Deps.Build.GetBlueprintPatchPlanningAttempt(
		ctx, request.WorkspaceID, request.BuildRunID, request.RevisionNo,
		teambuild.SourceRoleBlueprintPatchPlanner,
	)
	if errors.Is(err, teambuild.ErrBlueprintPatchPlanningAttemptNotFound) {
		_, runErr := p.runControlledAgent(
			ctx, e.round, blueprintPatchPlannerRecord(request.WorkspaceID), string(request.Payload),
			teambuild.SourceRoleBlueprintPatchPlanner, request.SourceReportHash,
		)
		attempt, err = p.Deps.Build.GetBlueprintPatchPlanningAttempt(
			ctx, request.WorkspaceID, request.BuildRunID, request.RevisionNo,
			teambuild.SourceRoleBlueprintPatchPlanner,
		)
		if err != nil {
			if runErr != nil {
				return teambuild.BlueprintPatchPlannerResult{}, fmt.Errorf("run blueprint patch planner: %w", runErr)
			}
			return teambuild.BlueprintPatchPlannerResult{}, fmt.Errorf("load blueprint patch planner attempt: %w", err)
		}
	} else if err != nil {
		return teambuild.BlueprintPatchPlannerResult{}, err
	}
	if attempt.SourceReportHash != request.SourceReportHash ||
		attempt.SourceRole != teambuild.SourceRoleBlueprintPatchPlanner {
		return teambuild.BlueprintPatchPlannerResult{}, errors.New("blueprint patch planning attempt is not bound to the current report")
	}
	if attempt.OutputText == nil || attempt.OutputHash == nil {
		return teambuild.BlueprintPatchPlannerResult{}, fmt.Errorf("%w: blueprint patch planner output is not recorded", ErrUsageSourcePending)
	}
	source := teambuild.BuildUsageSource{
		WorkspaceID: attempt.WorkspaceID, BuildRunID: attempt.BuildRunID,
		RoundNo: attempt.RevisionNo, SourceKind: teambuild.UsageSourceKindBuildAgent,
		SourceRole: attempt.SourceRole, SourceRunID: attempt.SourceRunID,
	}
	if err := p.backfillUsageSource(ctx, e.round, source); err != nil {
		return teambuild.BlueprintPatchPlannerResult{}, fmt.Errorf("account blueprint patch planner: %w", err)
	}
	usage, err := p.Deps.Build.GetRoundBudgetUsage(
		ctx, request.WorkspaceID, request.BuildRunID, request.RevisionNo,
	)
	if err != nil {
		return teambuild.BlueprintPatchPlannerResult{}, fmt.Errorf("load blueprint patch planner usage ledger: %w", err)
	}
	return teambuild.BlueprintPatchPlannerResult{
		AttemptID: fmt.Sprintf("%s/%s/%d/%s", request.WorkspaceID, request.BuildRunID, request.RevisionNo, teambuild.SourceRoleBlueprintPatchPlanner),
		RunID:     attempt.SourceRunID, Output: *attempt.OutputText,
		Usage: usage, UsageSource: source,
	}, nil
}

var _ teambuild.BlueprintPatchPlannerExecutor = buildBlueprintPatchPlannerExecutor{}

func decodeBlueprintPatchPlannerOutput(output string) (teambuild.BlueprintPatchV1, error) {
	if err := rejectDuplicateJSONKeys(strings.NewReader(output)); err != nil {
		return teambuild.BlueprintPatchV1{}, fmt.Errorf("decode blueprint patch planner output: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(output))
	decoder.DisallowUnknownFields()
	var patch teambuild.BlueprintPatchV1
	if err := decoder.Decode(&patch); err != nil {
		return teambuild.BlueprintPatchV1{}, fmt.Errorf("decode blueprint patch planner output: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return teambuild.BlueprintPatchV1{}, errors.New("decode blueprint patch planner output: trailing content is forbidden")
	}
	return patch, nil
}

func rejectDuplicateJSONKeys(reader io.Reader) error {
	decoder := json.NewDecoder(reader)
	var consumeValue func() error
	consumeValue = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := make(map[string]struct{})
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("object key is not a string")
				}
				if _, duplicate := seen[key]; duplicate {
					return fmt.Errorf("duplicate object key %q", key)
				}
				seen[key] = struct{}{}
				if err := consumeValue(); err != nil {
					return err
				}
			}
			closing, err := decoder.Token()
			if err != nil {
				return err
			}
			if closing != json.Delim('}') {
				return errors.New("object is not closed")
			}
		case '[':
			for decoder.More() {
				if err := consumeValue(); err != nil {
					return err
				}
			}
			closing, err := decoder.Token()
			if err != nil {
				return err
			}
			if closing != json.Delim(']') {
				return errors.New("array is not closed")
			}
		default:
			return fmt.Errorf("unexpected delimiter %q", delim)
		}
		return nil
	}
	if err := consumeValue(); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err != nil {
			return err
		}
		return errors.New("trailing content is forbidden")
	}
	return nil
}

var (
	// ErrUsageSourcePending reports a usage source whose runtime run has not
	// yet produced a final terminal marker. The associated role must not be
	// started again: the runtime owns recovery, and Build waits for the
	// marker instead of launching a duplicate run.
	ErrUsageSourcePending = errors.New("usage source pending final terminal marker")
	// ErrUsageMarkerMissing reports a meta-agent run that ended without a
	// final runtime terminal marker (e.g. a CLI/remote agent with no runtime
	// marker). Usage fails closed instead of fabricating ledger facts.
	ErrUsageMarkerMissing = errors.New("usage source has no final terminal marker")
)

// sourceRoleBaselineWorkflowRoot is the usage-source role of the optimize
// baseline candidate run (T15C). It is charged into round 1's ledger like
// every round-bound candidate, but it is never mistaken for the round's own
// candidate run by Evaluate's reuse logic.
const sourceRoleBaselineWorkflowRoot = "baseline_workflow_root"

type evaluationScenario struct {
	ID               string
	Input            string
	Expected         string
	InputVersion     string
	PerturbationRule string
}

type candidateScenarioRun struct {
	Scenario     evaluationScenario
	RunID        string
	Status       string
	Output       string
	StageOutputs []candidateStageOutput
}

type candidateStageOutput struct {
	TaskID         string `json:"task_id"`
	Source         string `json:"source"`
	Kind           string `json:"kind"`
	Agent          string `json:"agent,omitempty"`
	TerminalStatus string `json:"terminal_status"`
	Output         string `json:"output"`
}

type semanticEvaluationPackageV1 struct {
	SchemaVersion int                            `json:"schema_version"`
	ContractHash  string                         `json:"contract_hash"`
	Contract      teambuild.EvaluationContract   `json:"evaluation_contract"`
	Scenarios     []semanticEvaluationScenarioV1 `json:"scenarios"`
}

type semanticEvaluationScenarioV1 struct {
	ScenarioID     string                 `json:"scenario_id"`
	InputVersion   string                 `json:"input_version"`
	TerminalStatus string                 `json:"terminal_status"`
	Input          string                 `json:"input"`
	Expected       string                 `json:"expected"`
	Output         string                 `json:"output"`
	StageOutputs   []candidateStageOutput `json:"stage_outputs,omitempty"`
}

// evaluationScenarios returns only concrete, contract-frozen cases. An
// abstract perturbation rule is a generation constraint, not executable
// business input; appending it to a public prompt asks the candidate to invent
// its own test and leaves the original expected value contradictory.
// Hidden scenarios are not synthesized here because the v1 contract carries
// only their count and constraints, not frozen evaluator-only inputs.
func evaluationScenarios(contract teambuild.EvaluationContract) []evaluationScenario {
	capacity := len(contract.PublicScenarios) + len(contract.PerturbationScenarios)
	scenarios := make([]evaluationScenario, 0, capacity)
	for _, public := range contract.PublicScenarios {
		scenarios = append(scenarios, evaluationScenario{
			ID: strings.TrimSpace(public.ID), Input: strings.TrimSpace(public.Input),
			Expected: strings.TrimSpace(public.Expected), InputVersion: "v1",
		})
	}
	for _, frozen := range contract.PerturbationScenarios {
		rule := strings.TrimSpace(frozen.Rule)
		scenarios = append(scenarios, evaluationScenario{
			ID: strings.TrimSpace(frozen.ID), Input: strings.TrimSpace(frozen.Input),
			Expected:         strings.TrimSpace(frozen.Expected),
			InputVersion:     "v1+perturbation:" + shortID(frozen.BaseScenarioID, rule, frozen.ID),
			PerturbationRule: rule,
		})
	}
	return scenarios
}

func scenarioSourceRole(base string, index int, scenarioID string) string {
	if index == 0 {
		return base
	}
	return base + ":scenario:" + shortID(fmt.Sprintf("%d", index), scenarioID)
}

// baselineReportNamespacePrefix and baselineReportKey address the immutable
// baseline report inside the loom_store namespace (the same storeext surface
// the candidate-run terminal records use). The baseline report is a
// run-level fact, not a round ledger row: it must never collide with round 1's
// report in weave_team_build_run_reports, so it is stored separately.
const baselineReportNamespacePrefix = "team-forge:"

func baselineReportKey(buildRunID string) string {
	return "baseline-report:" + buildRunID
}

// Build runs the round's construction through the meta-team employees. The
// workspace admin has already authorized an immutable BuildBrief and
// EvaluationContract, so the platform deterministically compiles those
// documents into the execution instruction; it never spends another LLM run
// asking the architect to rediscover or rewrite the authorized plan. The config
// engineer assembles Agent/Team/Roster and the graph designer builds employee
// graphs and the team workflow. Each employee's teamforge tool set is decided by
// teamforge.DecideToolSet so the role-specific write authority is exactly
// the platform's conversation wiring.
//
// T14B-1 usage attribution: every built-in Loom meta-agent run is durably
// associated with its runtime run id by the admission-after hook before any
// graph/LLM/tool work, its final terminal marker is charged into the T14A
// budget ledger after the run, and after every role's accounting the budget
// is evaluated so a strictly exceeded dimension stops the next role and
// returns nil — the controller's post-Build gate then transitions
// the run to blocked. Build starts by reconciling the round's usage sources:
// an association with a final marker but no ledger row is backfilled
// idempotently, while an association without a final marker returns
// ErrUsageSourcePending and never re-runs that role.
func (p *ProductionPhases) Build(ctx context.Context, round RoundContext) error {
	if err := p.reconcileBuildUsage(ctx, round); err != nil {
		return fmt.Errorf("build phase: reconcile usage round %d: %w", round.RoundNo, err)
	}
	accounted, err := p.accountedBuildRoles(ctx, round)
	if err != nil {
		return fmt.Errorf("build phase: inspect usage sources round %d: %w", round.RoundNo, err)
	}
	// Reconciliation may have backfilled a completed role from an older plan
	// shape (for example the former architect step). Its charge still consumes
	// this round's immutable budget even when that role is absent from today's
	// plan, so gate once before admitting any new role.
	initialDecision, err := p.Deps.Build.EvaluateBudget(
		ctx,
		round.WorkspaceID,
		round.BuildRunID,
		round.RoundNo,
		round.Run.RoundBudget,
		round.Run.TotalBudget,
	)
	if err != nil {
		return fmt.Errorf("build phase: evaluate reconciled budget round %d: %w", round.RoundNo, err)
	}
	if len(initialDecision.ExceededDims) > 0 {
		return nil
	}

	executionPlan, err := json.Marshal(struct {
		SchemaVersion int                          `json:"schema_version"`
		BuildRunID    string                       `json:"build_run_id"`
		RoundNo       int                          `json:"round_no"`
		BriefHash     string                       `json:"brief_hash"`
		ContractHash  string                       `json:"contract_hash"`
		Brief         teambuild.BuildBrief         `json:"brief"`
		Contract      teambuild.EvaluationContract `json:"evaluation_contract"`
	}{
		SchemaVersion: 1,
		BuildRunID:    round.BuildRunID,
		RoundNo:       round.RoundNo,
		BriefHash:     round.Run.BriefHash,
		ContractHash:  round.Run.ContractHash,
		Brief:         round.Run.Brief,
		Contract:      round.Run.Contract,
	})
	if err != nil {
		return fmt.Errorf("build phase: compile authorized execution plan: %w", err)
	}

	type buildRoleStep struct {
		sourceRole  string
		messageRole string
	}
	steps := []buildRoleStep{
		{sourceRole: teambuild.SourceRoleConfigEngineer, messageRole: "config-engineer"},
		{sourceRole: teambuild.SourceRoleGraphDesigner, messageRole: "graph-designer"},
	}
	executor := p.Deps.ConstructionRoles
	if executor == nil {
		executor = buildConstructionRoleExecutor{phases: p, round: round}
	}
	previousOutput := "authorized_execution_plan=" + string(executionPlan)
	for _, step := range steps {
		if accounted[step.sourceRole] {
			// The role's run was already accounted for this round (recovered
			// from a crash). It must never be started again; only the budget
			// gate decides whether the next role may start.
			decision, err := p.Deps.Build.EvaluateBudget(
				ctx,
				round.WorkspaceID,
				round.BuildRunID,
				round.RoundNo,
				round.Run.RoundBudget,
				round.Run.TotalBudget,
			)
			if err != nil {
				return fmt.Errorf("build phase: evaluate budget after %s round %d: %w",
					step.sourceRole, round.RoundNo, err)
			}
			if len(decision.ExceededDims) > 0 {
				return nil
			}
			continue
		}

		roleContext, err := json.Marshal(struct {
			BuildRunID string `json:"build_run_id"`
			RoundNo    int    `json:"round_no"`
			Role       string `json:"role"`
		}{round.BuildRunID, round.RoundNo, step.messageRole})
		if err != nil {
			return fmt.Errorf("build phase: encode %s role context: %w", step.sourceRole, err)
		}
		message := previousOutput + "\nrole_context=" + string(roleContext)
		result, err := executor.Execute(ctx, teambuild.ConstructionRoleRequest{
			WorkspaceID: round.WorkspaceID, BuildRunID: round.BuildRunID,
			RevisionNo: round.RoundNo, Run: round.Run,
			SourceRole: step.sourceRole, Input: message,
		})
		if err != nil {
			return fmt.Errorf("build phase: %s round %d: %w", step.sourceRole, round.RoundNo, err)
		}
		previousOutput = result.Output

		// Each role's accounting must leave the next role startable; an
		// exceeded dimension stops the build with no
		// error so the controller's post-Build gate blocks the run.
		decision, err := p.Deps.Build.EvaluateBudget(
			ctx,
			round.WorkspaceID,
			round.BuildRunID,
			round.RoundNo,
			round.Run.RoundBudget,
			round.Run.TotalBudget,
		)
		if err != nil {
			return fmt.Errorf("build phase: evaluate budget after %s round %d: %w",
				step.sourceRole, round.RoundNo, err)
		}
		if len(decision.ExceededDims) > 0 {
			return nil
		}
	}
	return nil
}

type buildConstructionRoleExecutor struct {
	phases *ProductionPhases
	round  RoundContext
}

func (e buildConstructionRoleExecutor) Execute(
	ctx context.Context,
	request teambuild.ConstructionRoleRequest,
) (teambuild.ConstructionRoleResult, error) {
	p := e.phases
	if p == nil || request.WorkspaceID != e.round.WorkspaceID ||
		request.BuildRunID != e.round.BuildRunID || request.RevisionNo != e.round.RoundNo {
		return teambuild.ConstructionRoleResult{}, errors.New("construction role request identity is invalid")
	}
	rec, err := constructionRoleRecord(request.WorkspaceID, request.SourceRole)
	if err != nil {
		return teambuild.ConstructionRoleResult{}, err
	}
	result, err := p.runControlledAgent(
		ctx, e.round, rec, request.Input, request.SourceRole, "",
	)
	if err != nil {
		return teambuild.ConstructionRoleResult{}, err
	}
	if err := p.accountBuildRunUsage(ctx, e.round, request.SourceRole, result.RunID); err != nil {
		return teambuild.ConstructionRoleResult{}, fmt.Errorf("account construction role: %w", err)
	}
	usage, err := p.Deps.Build.GetRoundBudgetUsage(
		ctx, request.WorkspaceID, request.BuildRunID, request.RevisionNo,
	)
	if err != nil {
		return teambuild.ConstructionRoleResult{}, fmt.Errorf("load construction role usage ledger: %w", err)
	}
	source := teambuild.BuildUsageSource{
		WorkspaceID: request.WorkspaceID, BuildRunID: request.BuildRunID,
		RoundNo: request.RevisionNo, SourceKind: teambuild.UsageSourceKindBuildAgent,
		SourceRole: request.SourceRole, SourceRunID: result.RunID,
	}
	return teambuild.ConstructionRoleResult{
		AttemptID: fmt.Sprintf("%s/%s/%d/%s", request.WorkspaceID, request.BuildRunID, request.RevisionNo, request.SourceRole),
		RunID:     result.RunID, Output: result.Output, Usage: usage, UsageSource: source,
	}, nil
}

var _ teambuild.ConstructionRoleExecutor = buildConstructionRoleExecutor{}

// Baseline runs the optimize-mode baseline evaluation (T15C) before round 1's
// Build: it freezes the pre-task published workflow content into a candidate,
// test-runs it through the same teamrun executor, evaluates every hard gate,
// and persists an immutable baseline report (report.Baseline=true) as the
// anchor for every round's regression/improvement comparison. The baseline
// candidate run is attributed to round 1's budget ledger under the
// baseline_workflow_root role (T14B-2A seam), so the pre-Build budget gate
// sees its usage.
//
// The step is idempotent under crash recovery: a persisted baseline report
// short-circuits a re-run, an admitted-but-unsettled baseline candidate run is
// recharged and reused from its final terminal marker, and a draft created by
// a crashed baseline attempt is reused instead of minting a second one.
func (p *ProductionPhases) Baseline(ctx context.Context, round RoundContext) error {
	if round.Run.Mode != teambuild.ModeOptimize {
		return nil
	}
	if p.readBaselineReport(ctx, round.WorkspaceID, round.BuildRunID) != nil {
		return nil
	}

	team, err := p.targetTeam(ctx, round.WorkspaceID, round.Run)
	if err != nil {
		return fmt.Errorf("baseline: resolve target team: %w", err)
	}
	if team == nil {
		return errors.New("baseline: optimize target team not found")
	}
	workflowID, err := p.pinnedOptimizeWorkflowID(round.Run)
	if err != nil {
		return fmt.Errorf("baseline: pin target workflow: %w", err)
	}

	scenarios := evaluationScenarios(round.Run.Contract)
	roles := make([]string, len(scenarios))
	for index, scenario := range scenarios {
		roles[index] = scenarioSourceRole(sourceRoleBaselineWorkflowRoot, index, scenario.ID)
	}
	existingSources, err := p.reconcileScenarioUsage(ctx, round, roles)
	if err != nil {
		return fmt.Errorf("baseline: reconcile usage: %w", err)
	}
	baselineVersion, err := baselinePublishedVersion(round.Run, workflowID)
	if err != nil {
		return fmt.Errorf("baseline: resolve frozen published version: %w", err)
	}
	version, err := p.Deps.Workflows.GetVersion(
		ctx, round.WorkspaceID, workflowID, baselineVersion,
	)
	if err != nil {
		return fmt.Errorf("baseline: read frozen published version timestamp: %w", err)
	}
	candidate, err := candidateFromBaselineSnapshot(
		round.Run, workflowID, version.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("baseline: restore frozen candidate: %w", err)
	}
	runs := make([]candidateScenarioRun, 0, len(scenarios))
	for index, scenario := range scenarios {
		var runID, status string
		if existing := existingSources[roles[index]]; existing != nil {
			var reused *workflow.PublicationCandidate
			reused, runID, status, err = p.reuseCandidateRun(ctx, round, *existing)
			if err == nil && reused.ContentHash != candidate.ContentHash {
				err = errors.New("reused baseline scenario candidate hash mismatch")
			}
		} else {
			candidate, runID, status, err = p.buildAndRunCandidateFor(
				ctx, round, team, workflowID, baselineVersion,
				roles[index], candidate, scenario,
			)
		}
		if err != nil {
			return fmt.Errorf("baseline: scenario %s candidate test run: %w", scenario.ID, err)
		}
		stageOutputs := p.candidateRunStageOutputs(ctx, round.WorkspaceID, runID)
		runs = append(runs, candidateScenarioRun{
			Scenario: scenario, RunID: runID, Status: status,
			Output:       formatCandidateRunEvidence(runID, status, stageOutputs),
			StageOutputs: stageOutputs,
		})
	}
	if _, err := p.reconcileScenarioUsage(ctx, round, roles); err != nil {
		return fmt.Errorf("baseline: settle usage: %w", err)
	}
	for _, scenarioRun := range runs {
		if err := p.insertDeliverableEvidence(
			ctx, round, team, scenarioRun.RunID, scenarioRun.Status,
		); err != nil {
			return fmt.Errorf("baseline: record scenario %s deliverable evidence: %w",
				scenarioRun.Scenario.ID, err)
		}
	}
	gates, err := p.evaluateGates(ctx, round.WorkspaceID, round.BuildRunID, team.ID)
	if err != nil {
		return fmt.Errorf("baseline: evaluate gates: %w", err)
	}
	report := p.assembleReport(ctx, round, team, candidate, runs, gates)
	report.Baseline = true
	report.RoundNo = round.RoundNo
	conclusion, _ := roundConclusion(round.Run.Contract, gates, report.RubricScores, report.SevereDefects)
	report.Conclusion = conclusion
	report.ConfigChangeSummary = fmt.Sprintf(
		"baseline round %d evaluated frozen pre-task content %s for workflow %s",
		round.RoundNo, candidate.ContentHash, candidate.WorkflowID,
	)
	if err := p.persistBaselineReport(ctx, round.WorkspaceID, round.BuildRunID, report); err != nil {
		return fmt.Errorf("baseline: persist report: %w", err)
	}
	return nil
}

func baselinePublishedVersion(run teambuild.TeamBuildRun, workflowID string) (int, error) {
	if run.Baseline == nil {
		return 0, errors.New("optimize build run has no baseline snapshot")
	}
	for _, ref := range run.Baseline.Workflows {
		if ref.WorkflowID == workflowID && ref.Published != nil && ref.Published.Version > 0 {
			return ref.Published.Version, nil
		}
	}
	return 0, fmt.Errorf("workflow %q has no frozen published baseline", workflowID)
}

// candidateFromBaselineSnapshot reconstructs the immutable candidate that
// existed when the optimize run was authorized. It intentionally ignores a
// mutable draft captured beside the published artifact: that draft is the
// optimization candidate, never the pre-change baseline.
func candidateFromBaselineSnapshot(
	run teambuild.TeamBuildRun,
	workflowID string,
	expectedUpdatedAt time.Time,
) (*workflow.PublicationCandidate, error) {
	if run.Baseline == nil {
		return nil, errors.New("optimize build run has no baseline snapshot")
	}
	for _, ref := range run.Baseline.Workflows {
		if ref.WorkflowID != workflowID || ref.Published == nil {
			continue
		}
		published := ref.Published
		var payload frozen.ArtifactPayloadV1
		if err := json.Unmarshal(published.Artifact.Payload, &payload); err != nil {
			return nil, fmt.Errorf("decode frozen baseline artifact payload: %w", err)
		}
		dependencies := make([]workflow.TeamWorkflowDependency, 0, len(published.Dependencies))
		for _, dependency := range published.Dependencies {
			dependencies = append(dependencies, workflow.TeamWorkflowDependency{
				WorkspaceID: run.WorkspaceID, WorkflowID: workflowID,
				WorkflowVersion: published.Version,
				OwnerType:       dependency.OwnerType, OwnerID: dependency.OwnerID,
				OwnerAgentVersion: dependency.OwnerAgentVersion,
				DependencyType:    dependency.DependencyType,
				DependencyKey:     dependency.DependencyKey,
				DependencyVersion: dependency.DependencyVersion,
				ContentHash:       dependency.ContentHash,
			})
		}
		artifact := published.Artifact
		candidate := &workflow.PublicationCandidate{
			WorkspaceID: run.WorkspaceID, WorkflowID: workflowID,
			WorkflowVersion: published.Version, ExpectedUpdatedAt: expectedUpdatedAt,
			ArtifactSchemaVersion:     artifact.ArtifactSchemaVersion,
			CanonicalizationAlgorithm: artifact.CanonicalizationAlgorithm,
			CanonicalizationVersion:   artifact.CanonicalizationVersion,
			HashAlgorithm:             artifact.HashAlgorithm,
			Payload:                   payload, Dependencies: dependencies,
		}
		contentHash, err := frozen.ComputeArtifactContentHash(frozen.ArtifactEnvelopeHashInputV1{
			WorkspaceID: candidate.WorkspaceID, WorkflowID: candidate.WorkflowID,
			WorkflowVersion:           candidate.WorkflowVersion,
			ArtifactSchemaVersion:     candidate.ArtifactSchemaVersion,
			CanonicalizationAlgorithm: candidate.CanonicalizationAlgorithm,
			CanonicalizationVersion:   candidate.CanonicalizationVersion,
			HashAlgorithm:             candidate.HashAlgorithm,
			Payload:                   candidate.Payload,
		})
		if err != nil {
			return nil, fmt.Errorf("rehash frozen baseline artifact: %w", err)
		}
		candidate.ContentHash = contentHash
		envelopePayload, err := frozen.Canonicalize(
			candidate.Payload, frozen.PreorderArtifactPayloadV1,
		)
		if err != nil {
			return nil, fmt.Errorf("normalize frozen baseline artifact payload: %w", err)
		}
		if _, err := frozen.DecodeArtifactEnvelopeV1(frozen.ArtifactEnvelopeV1{
			WorkspaceID: candidate.WorkspaceID, WorkflowID: candidate.WorkflowID,
			WorkflowVersion:           candidate.WorkflowVersion,
			ArtifactSchemaVersion:     candidate.ArtifactSchemaVersion,
			CanonicalizationAlgorithm: candidate.CanonicalizationAlgorithm,
			CanonicalizationVersion:   candidate.CanonicalizationVersion,
			HashAlgorithm:             candidate.HashAlgorithm,
			ContentHash:               candidate.ContentHash,
			Payload:                   envelopePayload,
		}); err != nil {
			return nil, fmt.Errorf("validate frozen baseline artifact envelope: %w", err)
		}
		return candidate, nil
	}
	return nil, fmt.Errorf("workflow %q has no frozen published baseline", workflowID)
}

// reconcileBaselineUsage drains every uncharged baseline candidate source of
// the round (backfilling from the final terminal marker) and returns the
// round's baseline source when one is already associated. A source without a
// final marker returns ErrUsageSourcePending so a new baseline candidate is
// never started while the admitted run is still in flight.
func (p *ProductionPhases) reconcileBaselineUsage(
	ctx context.Context,
	round RoundContext,
) (*teambuild.BuildUsageSource, error) {
	uncharged, err := p.Deps.Build.ListUnchargedUsageSources(
		ctx, round.WorkspaceID, round.BuildRunID, round.RoundNo,
	)
	if err != nil {
		return nil, err
	}
	for _, source := range uncharged {
		if source.SourceKind != teambuild.UsageSourceKindCandidateRuntime ||
			source.SourceRole != sourceRoleBaselineWorkflowRoot {
			continue
		}
		if err := p.backfillCandidateUsageSource(ctx, round, source); err != nil {
			return nil, err
		}
	}

	sources, err := p.Deps.Build.ListUsageSources(
		ctx, round.WorkspaceID, round.BuildRunID, round.RoundNo,
	)
	if err != nil {
		return nil, err
	}
	for _, source := range sources {
		if source.SourceKind == teambuild.UsageSourceKindCandidateRuntime &&
			source.SourceRole == sourceRoleBaselineWorkflowRoot {
			copied := source
			return &copied, nil
		}
	}
	return nil, nil
}

// reconcileScenarioUsage settles and indexes the candidate-runtime sources
// for one deterministic scenario suite. The latest source for a role wins so
// the one allowed timeout retry can replace its failed first attempt without
// losing either usage record.
func (p *ProductionPhases) reconcileScenarioUsage(
	ctx context.Context,
	round RoundContext,
	roles []string,
) (map[string]*teambuild.BuildUsageSource, error) {
	wanted := make(map[string]bool, len(roles))
	for _, role := range roles {
		wanted[role] = true
	}
	uncharged, err := p.Deps.Build.ListUnchargedUsageSources(
		ctx, round.WorkspaceID, round.BuildRunID, round.RoundNo,
	)
	if err != nil {
		return nil, err
	}
	for _, source := range uncharged {
		if source.SourceKind != teambuild.UsageSourceKindCandidateRuntime ||
			!wanted[source.SourceRole] {
			continue
		}
		if err := p.backfillCandidateUsageSource(ctx, round, source); err != nil {
			return nil, err
		}
	}

	sources, err := p.Deps.Build.ListUsageSources(
		ctx, round.WorkspaceID, round.BuildRunID, round.RoundNo,
	)
	if err != nil {
		return nil, err
	}
	indexed := make(map[string]*teambuild.BuildUsageSource, len(roles))
	for _, source := range sources {
		if source.SourceKind != teambuild.UsageSourceKindCandidateRuntime ||
			!wanted[source.SourceRole] {
			continue
		}
		copied := source
		indexed[source.SourceRole] = &copied
	}
	return indexed, nil
}

// baselineDraftVersion creates the evaluation draft from the baseline
// published content (CreateDraft clones the published version) or reuses the
// highest existing draft left behind by a crashed baseline attempt.
func (p *ProductionPhases) baselineDraftVersion(
	ctx context.Context,
	workspaceID, workflowID string,
) (int, error) {
	version, err := p.Deps.Workflows.CreateDraft(ctx, workspaceID, workflowID, DefaultActor)
	if err == nil {
		return version.Version, nil
	}
	if !errors.Is(err, workflow.ErrDraftExists) {
		return 0, fmt.Errorf("create baseline draft: %w", err)
	}
	return p.latestDraftVersion(ctx, workspaceID, workflowID)
}

// persistBaselineReport stores the immutable baseline report once under the
// run's loom_store key. A concurrent or replayed write with an existing row
// is a no-op.
func (p *ProductionPhases) persistBaselineReport(
	ctx context.Context,
	workspaceID, buildRunID string,
	report teambuild.EvaluationReport,
) error {
	raw, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("encode baseline report: %w", err)
	}
	return storeext.New(p.Deps.Pool).MutateValue(
		ctx,
		baselineReportNamespacePrefix+workspaceID,
		baselineReportKey(buildRunID),
		func(current []byte, present bool) ([]byte, error) {
			if present {
				return current, nil
			}
			return raw, nil
		},
	)
}

// readBaselineReport loads the persisted baseline report, or nil when the
// run has none (create mode) or the row is unreadable.
func (p *ProductionPhases) readBaselineReport(
	ctx context.Context,
	workspaceID, buildRunID string,
) *teambuild.EvaluationReport {
	data, present, err := storeext.New(p.Deps.Pool).ReadValue(
		ctx, baselineReportNamespacePrefix+workspaceID, baselineReportKey(buildRunID),
	)
	if err != nil || !present {
		return nil
	}
	var report teambuild.EvaluationReport
	if err := json.Unmarshal(data, &report); err != nil {
		return nil
	}
	return &report
}

// reconcileBuildUsage drains every usage source of the round that has no
// budget ledger row yet. A source whose runtime run already produced a final
// terminal marker is backfilled idempotently from that marker; a source
// without a final marker returns ErrUsageSourcePending so the role is never
// started again while its run is still in flight.
func (p *ProductionPhases) reconcileBuildUsage(
	ctx context.Context,
	round RoundContext,
) error {
	sources, err := p.Deps.Build.ListUnchargedUsageSources(
		ctx,
		round.WorkspaceID,
		round.BuildRunID,
		round.RoundNo,
	)
	if err != nil {
		return err
	}
	for _, source := range sources {
		if err := p.backfillUsageSource(ctx, round, source); err != nil {
			return err
		}
	}
	return nil
}

func (p *ProductionPhases) backfillUsageSource(
	ctx context.Context,
	round RoundContext,
	source teambuild.BuildUsageSource,
) error {
	tx, err := p.Deps.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin usage reconcile: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := p.Deps.Build.LockBuildRunTx(ctx, tx, round.WorkspaceID, round.BuildRunID); err != nil {
		return fmt.Errorf("lock build run for usage reconcile: %w", err)
	}
	marker, present, err := loomruntime.NewPGTerminalStateStore().
		ReadTerminalMarkerForUpdate(ctx, tx, round.WorkspaceID, source.SourceRunID)
	if err != nil {
		return fmt.Errorf("read terminal marker for run %q: %w", source.SourceRunID, err)
	}
	if !present || marker.Phase != loomruntime.TerminalMarkerPhaseFinal {
		return fmt.Errorf(
			"%w: %s source %q",
			ErrUsageSourcePending,
			source.SourceRole,
			source.SourceRunID,
		)
	}
	if _, err := p.Deps.Build.RecordBudgetUsageTx(
		ctx,
		tx,
		round.WorkspaceID,
		round.BuildRunID,
		usageChargeFromMarker(round, source.SourceRole, source.SourceRunID, marker),
	); err != nil {
		return fmt.Errorf("backfill budget ledger for run %q: %w", source.SourceRunID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit usage reconcile: %w", err)
	}
	return nil
}

// accountedBuildRoles reports which build_agent roles already have a usage
// source for the round. Reconcile guarantees every such source is charged,
// so an accounted role is complete and must not run again.
func (p *ProductionPhases) accountedBuildRoles(
	ctx context.Context,
	round RoundContext,
) (map[string]bool, error) {
	sources, err := p.Deps.Build.ListUsageSources(
		ctx,
		round.WorkspaceID,
		round.BuildRunID,
		round.RoundNo,
	)
	if err != nil {
		return nil, err
	}
	accounted := make(map[string]bool)
	for _, source := range sources {
		if source.SourceKind == teambuild.UsageSourceKindBuildAgent {
			accounted[source.SourceRole] = true
		}
	}
	return accounted, nil
}

// accountBuildRunUsage reads the run's final terminal marker and appends the
// T14A budget ledger row for one build_agent source. Only final markers are
// authoritative; a run without one (CLI/remote meta agent) fails closed and
// no usage is fabricated. The marker read and the ledger write share one
// transaction, so a crash between the two is exactly what reconcile recovers.
func (p *ProductionPhases) accountBuildRunUsage(
	ctx context.Context,
	round RoundContext,
	role, runID string,
) error {
	if runID == "" {
		return fmt.Errorf("%w: %s run produced no runtime run id", ErrUsageMarkerMissing, role)
	}
	tx, err := p.Deps.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin usage accounting: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := p.Deps.Build.LockBuildRunTx(ctx, tx, round.WorkspaceID, round.BuildRunID); err != nil {
		return fmt.Errorf("lock build run for usage accounting: %w", err)
	}
	marker, present, err := loomruntime.NewPGTerminalStateStore().
		ReadTerminalMarkerForUpdate(ctx, tx, round.WorkspaceID, runID)
	if err != nil {
		return fmt.Errorf("read terminal marker for run %q: %w", runID, err)
	}
	if !present || marker.Phase != loomruntime.TerminalMarkerPhaseFinal {
		return fmt.Errorf("%w: run %q", ErrUsageMarkerMissing, runID)
	}
	if _, err := p.Deps.Build.RecordBudgetUsageTx(
		ctx,
		tx,
		round.WorkspaceID,
		round.BuildRunID,
		usageChargeFromMarker(round, role, runID, marker),
	); err != nil {
		return fmt.Errorf("record budget charge for run %q: %w", runID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit usage accounting: %w", err)
	}
	return nil
}

// usageChargeFromMarker maps one final terminal marker's usage into the T14A
// budget ledger identity for the round. Historical ledger rows that predate
// marker tool-call attribution remain zero; append-only usage is never rewritten.
func usageChargeFromMarker(
	round RoundContext,
	role, runID string,
	marker loomruntime.TerminalMarkerV1,
) teambuild.BudgetCharge {
	return teambuild.BudgetCharge{
		WorkspaceID:  round.WorkspaceID,
		BuildRunID:   round.BuildRunID,
		RoundNo:      round.RoundNo,
		SourceKind:   teambuild.UsageSourceKindBuildAgent,
		SourceRole:   role,
		SourceRunID:  runID,
		InputTokens:  marker.UsageInputTokens,
		OutputTokens: marker.UsageOutputTokens,
		CostUSD:      marker.UsageCostUSD,
		ToolCalls:    marker.UsageToolCalls,
	}
}

// usageChargeFromCandidateMarker maps one final candidate-runtime terminal
// marker into the T14A budget ledger identity for the round.
func usageChargeFromCandidateMarker(
	round RoundContext,
	sourceRole string,
	runID string,
	marker loomruntime.TerminalMarkerV1,
) teambuild.BudgetCharge {
	return teambuild.BudgetCharge{
		WorkspaceID:  round.WorkspaceID,
		BuildRunID:   round.BuildRunID,
		RoundNo:      round.RoundNo,
		SourceKind:   teambuild.UsageSourceKindCandidateRuntime,
		SourceRole:   sourceRole,
		SourceRunID:  runID,
		InputTokens:  marker.UsageInputTokens,
		OutputTokens: marker.UsageOutputTokens,
		CostUSD:      marker.UsageCostUSD,
		ToolCalls:    marker.UsageToolCalls,
	}
}

// reconcileCandidateUsage drains every uncharged candidate-runtime usage
// source of the round: a source whose runtime run produced a final terminal
// marker is backfilled idempotently into the budget ledger, while a source
// without a final marker returns ErrUsageSourcePending so a new candidate is
// never started while the round's run is still in flight. The returned source
// is the round's existing candidate runtime when one is already associated.
func (p *ProductionPhases) reconcileCandidateUsage(
	ctx context.Context,
	round RoundContext,
) (*teambuild.BuildUsageSource, error) {
	uncharged, err := p.Deps.Build.ListUnchargedUsageSources(
		ctx,
		round.WorkspaceID,
		round.BuildRunID,
		round.RoundNo,
	)
	if err != nil {
		return nil, err
	}
	for _, source := range uncharged {
		if source.SourceKind != teambuild.UsageSourceKindCandidateRuntime {
			continue
		}
		if err := p.backfillCandidateUsageSource(ctx, round, source); err != nil {
			return nil, err
		}
	}

	sources, err := p.Deps.Build.ListUsageSources(
		ctx,
		round.WorkspaceID,
		round.BuildRunID,
		round.RoundNo,
	)
	if err != nil {
		return nil, err
	}
	var candidate *teambuild.BuildUsageSource
	for _, source := range sources {
		// Only the round's own candidate (fixed_workflow_root) may be reused
		// as "the round's candidate". The optimize baseline run shares the
		// round number for budget attribution but is settled by Baseline and
		// must never be mistaken for the round candidate (T15C).
		if source.SourceKind == teambuild.UsageSourceKindCandidateRuntime &&
			source.SourceRole == teambuild.SourceRoleFixedWorkflowRoot {
			copied := source
			candidate = &copied
		}
	}
	return candidate, nil
}

// backfillCandidateUsageSource reads the candidate run's final terminal
// marker and appends the budget ledger row in one transaction. A run without
// a final marker fails closed (ErrUsageSourcePending): the candidate is
// either still executing or lost, and no zero usage is ever fabricated.
func (p *ProductionPhases) backfillCandidateUsageSource(
	ctx context.Context,
	round RoundContext,
	source teambuild.BuildUsageSource,
) error {
	tx, err := p.Deps.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin candidate usage reconcile: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := p.Deps.Build.LockBuildRunTx(ctx, tx, round.WorkspaceID, round.BuildRunID); err != nil {
		return fmt.Errorf("lock build run for candidate usage reconcile: %w", err)
	}
	marker, present, err := loomruntime.NewPGTerminalStateStore().
		ReadTerminalMarkerForUpdate(ctx, tx, round.WorkspaceID, source.SourceRunID)
	if err != nil {
		return fmt.Errorf("read candidate terminal marker for run %q: %w", source.SourceRunID, err)
	}
	if !present || marker.Phase != loomruntime.TerminalMarkerPhaseFinal {
		return fmt.Errorf(
			"%w: candidate runtime source %q",
			ErrUsageSourcePending,
			source.SourceRunID,
		)
	}
	if _, err := p.Deps.Build.RecordBudgetUsageTx(
		ctx,
		tx,
		round.WorkspaceID,
		round.BuildRunID,
		usageChargeFromCandidateMarker(round, source.SourceRole, source.SourceRunID, marker),
	); err != nil {
		return fmt.Errorf("backfill candidate budget ledger for run %q: %w", source.SourceRunID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit candidate usage reconcile: %w", err)
	}
	return nil
}

// reuseCandidateRun rebuilds the round's already-admitted candidate test run
// after a crash between admission and the report. The final terminal marker
// is authoritative for the run's terminal status; the snapshot and candidate
// rows were committed with admission and must still carry the round identity.
func (p *ProductionPhases) reuseCandidateRun(
	ctx context.Context,
	round RoundContext,
	source teambuild.BuildUsageSource,
) (*workflow.PublicationCandidate, string, string, error) {
	tx, err := p.Deps.Pool.Begin(ctx)
	if err != nil {
		return nil, "", "", fmt.Errorf("begin candidate reuse: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	marker, present, err := loomruntime.NewPGTerminalStateStore().
		ReadTerminalMarkerForUpdate(ctx, tx, round.WorkspaceID, source.SourceRunID)
	if err != nil {
		return nil, "", "", fmt.Errorf("read candidate terminal marker for run %q: %w", source.SourceRunID, err)
	}
	if !present || marker.Phase != loomruntime.TerminalMarkerPhaseFinal {
		return nil, "", "", fmt.Errorf(
			"%w: candidate runtime source %q",
			ErrUsageSourcePending,
			source.SourceRunID,
		)
	}
	runSnapshot, err := p.Deps.Snapshots.GetByRunID(
		ctx, round.WorkspaceID, source.SourceRunID,
	)
	if err != nil {
		return nil, "", "", fmt.Errorf("read candidate snapshot %q: %w", source.SourceRunID, err)
	}
	if runSnapshot.BuildRunID != round.BuildRunID ||
		runSnapshot.BuildRoundNo != round.RoundNo ||
		runSnapshot.CandidateContentHash == "" ||
		runSnapshot.WorkflowID == "" {
		return nil, "", "", fmt.Errorf(
			"candidate snapshot %q does not match round %d identity",
			source.SourceRunID,
			round.RoundNo,
		)
	}
	candidate, err := p.Deps.Workflows.GetCandidateTx(
		ctx,
		tx,
		round.WorkspaceID,
		runSnapshot.WorkflowID,
		runSnapshot.CandidateContentHash,
	)
	if err != nil {
		return nil, "", "", fmt.Errorf("read candidate %q for run %q: %w",
			runSnapshot.CandidateContentHash, source.SourceRunID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, "", "", fmt.Errorf("commit candidate reuse: %w", err)
	}
	status := string(teamrun.StatusSucceeded)
	if marker.Status == loomruntime.TerminalMarkerStatusFailed {
		status = string(teamrun.StatusFailed)
	}
	return candidate, source.SourceRunID, status, nil
}

// runControlledAgent executes an in-memory build resource without resolving
// it through the workspace registry. The record and execution stamp are
// supplied by build code, while AgentRunner retains the existing durable run,
// terminal, attribution, and checkpoint protocol.
func (p *ProductionPhases) runControlledAgent(
	ctx context.Context,
	round RoundContext,
	rec *registry.AgentRecord,
	message, role, evidenceHash string,
) (mcphost.AgentRunResult, error) {
	runner, err := p.agentRunner(ctx, round, rec, role, evidenceHash)
	if err != nil {
		return mcphost.AgentRunResult{}, err
	}
	stamp := execution.AgentExecutionStamp{
		AgentID: rec.ID, AgentVersion: rec.Version,
		ExecutionScope: execution.ScopeLegacyOrchestrator,
	}
	return runner.RunRecord(ctx, rec, stamp, message)
}

func (p *ProductionPhases) agentRunner(
	ctx context.Context,
	round RoundContext,
	rec *registry.AgentRecord,
	role, sourceReportHash string,
) (*mcphost.AgentRunner, error) {
	var llm contract.LLM
	var err error
	if p.Deps.Runtimes != nil && p.Deps.CLIExecutor != nil {
		assignment, selectErr := p.Deps.Runtimes.Select(
			ctx, round.WorkspaceID, engine.Codex, "", rec.RuntimeID,
		)
		if selectErr == nil {
			runtimeRecord := *rec
			runtimeRecord.Engine = engine.Codex
			runtimeRecord.Model = ""
			runtimeRecord.RuntimeID = assignment.RuntimeID
			runtimeRecord.RuntimePoolID = assignment.PoolID
			if assignment.PoolID != "" {
				runtimeRecord.RuntimePolicyMode = "engine_pool"
			} else {
				runtimeRecord.RuntimePolicyMode = "auto_single"
			}
			llm, err = runtimellm.New(
				p.Deps.CLIExecutor, round.WorkspaceID, &runtimeRecord,
				execution.AgentExecutionStamp{
					AgentID: rec.ID, AgentVersion: rec.Version,
					ExecutionScope: execution.ScopeLegacyOrchestrator,
				},
			)
			if err != nil {
				return nil, err
			}
		}
	}
	if llm == nil {
		llm, err = p.llmForWorkspace(ctx, round.WorkspaceID)
		if err != nil {
			return nil, err
		}
	}
	runner := mcphost.NewAgentRunner(
		p.Deps.Agents,
		round.WorkspaceID,
		llm,
		p.Deps.Store,
		nil,
		"", "", "", "",
		nil,
	)
	// Durable control-plane work has no per-employee wall-clock limit. The
	// controller's budgets, iterations, leases, and explicit cancellation own
	// termination instead.
	runner.RunTimeout = -1
	runner.Broker = mcphost.NewToolBroker(nil)
	runner.InnerPlatformTools = func(rec *registry.AgentRecord) []contract.ToolDispatcher {
		return p.platformToolsForAgent(ctx, round, rec)
	}
	runner.UsageAttributionHook = func(
		hookCtx context.Context,
		lease loomruntime.RunAttemptLease,
	) error {
		if role == teambuild.SourceRoleBlueprintPatchPlanner {
			_, err := p.Deps.Build.ReserveBlueprintPatchPlanningAttempt(hookCtx, teambuild.BlueprintPatchPlanningAttempt{
				WorkspaceID: round.WorkspaceID, BuildRunID: round.BuildRunID,
				RevisionNo: round.RoundNo, SourceRole: role,
				SourceReportHash: sourceReportHash, SourceRunID: lease.RunID,
			})
			return err
		}
		if role == teambuild.SourceRoleSemanticJudge {
			_, err := p.Deps.Build.ReserveSemanticEvaluationAttempt(hookCtx, teambuild.SemanticEvaluationAttempt{
				WorkspaceID: round.WorkspaceID, BuildRunID: round.BuildRunID,
				RevisionNo: round.RoundNo, SourceRole: role,
				EvidenceHash: sourceReportHash, SourceRunID: lease.RunID,
			})
			return err
		}
		_, err := p.Deps.Build.RecordUsageSource(
			hookCtx,
			round.WorkspaceID,
			round.BuildRunID,
			teambuild.BuildUsageSource{
				WorkspaceID: round.WorkspaceID,
				BuildRunID:  round.BuildRunID,
				RoundNo:     round.RoundNo,
				SourceKind:  teambuild.UsageSourceKindBuildAgent,
				SourceRole:  role,
				SourceRunID: lease.RunID,
			},
		)
		return err
	}
	if role == teambuild.SourceRoleBlueprintPatchPlanner {
		runner.TerminalResultHook = func(
			hookCtx context.Context,
			lease loomruntime.RunAttemptLease,
			result loomruntime.Result,
		) error {
			return p.Deps.Build.RecordBlueprintPatchPlanningOutput(
				hookCtx, round.WorkspaceID, round.BuildRunID, round.RoundNo,
				role, lease.RunID, result.Output,
			)
		}
	}
	if role == teambuild.SourceRoleSemanticJudge {
		runner.TerminalResultHook = func(
			hookCtx context.Context,
			lease loomruntime.RunAttemptLease,
			result loomruntime.Result,
		) error {
			// Failed graph runs also reach the terminal hook. Empty output is
			// not evaluator evidence and must not look like a recorded judgment.
			if strings.TrimSpace(result.Output) == "" {
				return nil
			}
			return p.Deps.Build.RecordSemanticEvaluationOutput(
				hookCtx, round.WorkspaceID, round.BuildRunID, round.RoundNo,
				role, lease.RunID, result.Output,
			)
		}
	}
	return runner, nil
}

// platformToolsForAgent builds the teamforge dispatchers one meta-team
// employee receives for the round's run status, mirroring
// api/teamforge_wiring.go's fail-closed wiring: a nil audit store or a failed
// receipt reissue disables the write tools instead of panicking.
func (p *ProductionPhases) platformToolsForAgent(
	ctx context.Context,
	round RoundContext,
	rec *registry.AgentRecord,
) []contract.ToolDispatcher {
	// ProductionPhases.Build is the explicitly retained legacy direct-write
	// loop. Compiler-v1 never enters Build (Controller.runCompilerV1 executes
	// its persisted ChangeSet instead), so these meta-agent runs must receive
	// the compatibility write surface rather than compiler-v2's read-only
	// conversation surface.
	decision := teamforge.DecideLegacyDirectToolSetForMode(rec.Name, round.Run.Status, round.Run.Mode)
	if decision.IsZero() {
		return nil
	}

	var receipt teambuild.BuildAuthorizationReceipt
	if decision.HasWrites() {
		reissued, err := p.Deps.Build.ReissueReceipt(ctx, round.WorkspaceID, round.BuildRunID)
		if err != nil {
			return nil
		}
		receipt = reissued
	}

	deps := p.teamForgeDeps()
	writeDeps := p.teamForgeWriteDeps()
	dispatchers := make([]contract.ToolDispatcher, 0, 5)
	if decision.Read {
		dispatchers = append(dispatchers,
			teamforge.NewReadTools(round.WorkspaceID, rec.Name, round.BuildRunID, p.Deps.Build, p.Deps.Audit, deps))
	}
	if decision.AgentWrite {
		dispatchers = append(dispatchers,
			teamforge.NewWriteTools(round.WorkspaceID, rec.Name, receipt, p.Deps.Build, p.Deps.Audit, writeDeps))
	}
	if decision.TeamWrite || decision.TeamRosterWrite {
		dispatchers = append(dispatchers,
			teamforge.NewTeamWriteToolsMode(
				round.WorkspaceID, rec.Name, receipt, p.Deps.Build, p.Deps.Audit,
				deps, writeDeps, decision.TeamWrite,
			))
	}
	if decision.GraphWrite {
		dispatchers = append(dispatchers,
			teamforge.NewGraphBuildTools(
				round.WorkspaceID, rec.Name, receipt, p.Deps.Build, p.Deps.Audit,
				writeDeps, p.Deps.Drafts.GraphDrafts(round.BuildRunID),
			))
	}
	if decision.WorkflowWrite {
		workflowTools := teamforge.NewWorkflowBuildTools(
			round.WorkspaceID, rec.Name, receipt, p.Deps.Build, p.Deps.Audit,
			deps, writeDeps, p.Deps.Drafts.WorkflowDrafts(round.BuildRunID),
		)
		if round.Run.Brief.EffectiveWorkflowBuildMode() == teambuild.WorkflowBuildModeCustom {
			workflowTools = teamforge.NewCustomWorkflowBuildTools(
				round.WorkspaceID, rec.Name, receipt, p.Deps.Build, p.Deps.Audit,
				deps, writeDeps, p.Deps.Drafts.WorkflowDrafts(round.BuildRunID),
			)
		}
		dispatchers = append(dispatchers, workflowTools)
	}
	return dispatchers
}

// teamForgeDeps assembles the narrow read-side store mapping exactly like
// api/teamforge_wiring.go's teamForgeDeps. Keep in sync with the api wiring.
func (p *ProductionPhases) teamForgeDeps() teamforge.Deps {
	return teamforge.Deps{
		Agents:        p.Deps.Agents,
		Teams:         p.Deps.Teams,
		Roster:        p.Deps.TeamWorkers,
		DispatchRules: p.Deps.Teams,
		Workflows:     p.Deps.Workflows,
		MCPs:          p.Deps.MCPs,
		Providers:     p.Deps.Providers,
		Runtimes:      p.Deps.Runtimes,
		Tasks:         p.Deps.Tasks,
		Deliverables:  p.Deps.Deliverables,
		Skills:        p.Deps.Store,
		Runs:          p.Deps.Store,
	}
}

// teamForgeWriteDeps assembles the narrow write-side store mapping exactly
// like api/teamforge_wiring.go's teamForgeWriteDeps. Keep in sync with the
// api wiring.
func (p *ProductionPhases) teamForgeWriteDeps() teamforge.WriteDeps {
	return teamforge.WriteDeps{
		Pool:          p.Deps.Pool,
		Agents:        p.Deps.Agents,
		AgentLoad:     p.Deps.Agents,
		Models:        teamforge.CredentialModelResolver{},
		Runtimes:      p.Deps.Runtimes,
		Teams:         p.Deps.Teams,
		TeamDesign:    p.Deps.Teams,
		Roster:        p.Deps.Agents,
		DispatchRules: p.Deps.Teams,
		Workflows:     p.Deps.Workflows,
	}
}

// Evaluate freezes the round's candidate, runs it through the teamrun
// executor, gathers run/task/artifact evidence, evaluates every hard gate,
// and assembles the immutable round report.
func (p *ProductionPhases) Evaluate(ctx context.Context, round RoundContext) (RoundEvaluation, error) {
	workspaceID := round.WorkspaceID
	if round.Run.EffectiveExecutionStrategy() == teambuild.ExecutionStrategyCompilerV1 {
		decision, err := p.Deps.Build.EvaluateBudget(
			ctx, workspaceID, round.BuildRunID, round.RoundNo,
			round.Run.RoundBudget, round.Run.TotalBudget,
		)
		if err != nil {
			return RoundEvaluation{}, fmt.Errorf("evaluate phase: candidate budget preflight: %w", err)
		}
		if len(decision.ExceededDims) > 0 {
			return budgetExhaustedEvaluation(teambuild.EvaluationReport{SchemaVersion: 1}, "", decision), nil
		}
	}

	team, err := p.targetTeam(ctx, workspaceID, round.Run)
	if err != nil {
		return RoundEvaluation{}, err
	}
	if team == nil {
		report := p.failedTeamReport(round, "target team was not created by the build phase")
		return RoundEvaluation{
			Conclusion:      teambuild.ConclusionRevise,
			FailureCategory: teameval.GateTeamShape,
			Diagnosis: teameval.TypedDiagnosis{
				Class:          teameval.FailureClassBlueprintValidation,
				GateCode:       teameval.GateTeamShape,
				FailureDetail:  "target team was not created by the build phase",
				RevisionAction: teameval.RevisionActionBlockPlatformDiagnosis,
			},
			Report: report,
		}, nil
	}

	scenarios := evaluationScenarios(round.Run.Contract)
	if len(scenarios) == 0 {
		return p.infraEvaluation(round, "evaluation contract has no runnable scenarios", ""), nil
	}
	roles := make([]string, len(scenarios))
	for index, scenario := range scenarios {
		roles[index] = scenarioSourceRole(teambuild.SourceRoleFixedWorkflowRoot, index, scenario.ID)
	}

	// Reconcile every scenario role before starting missing runs. Each role is
	// independently recoverable, so a crash after scenario N resumes at N+1
	// instead of repeating already charged candidate executions.
	existing, err := p.reconcileScenarioUsage(ctx, round, roles)
	if err != nil {
		return RoundEvaluation{}, fmt.Errorf(
			"evaluate phase: reconcile candidate usage round %d: %w",
			round.RoundNo,
			err,
		)
	}

	var candidate *workflow.PublicationCandidate
	var candidateRef string
	runs := make([]candidateScenarioRun, 0, len(scenarios))
	for index, scenario := range scenarios {
		source := existing[roles[index]]
		retryCandidate := false
		if source != nil && round.CandidateAttempt == 2 {
			errorCode, failureDetail, factsErr := p.candidateRunFailureFacts(
				ctx, round.WorkspaceID, source.SourceRunID,
			)
			if factsErr != nil {
				return p.infraEvaluation(round, fmt.Sprintf(
					"read scenario %s retry candidate failure: %v", scenario.ID, factsErr), ""), nil
			}
			retryCandidate = candidateRunRetryRequired(
				round.CandidateAttempt, errorCode, failureDetail,
			)
		}

		var scenarioCandidate *workflow.PublicationCandidate
		var runID, status string
		if source != nil && !retryCandidate {
			scenarioCandidate, runID, status, err = p.reuseCandidateRun(ctx, round, *source)
		} else if candidate == nil {
			workflowID, draftVersion, versionErr := p.candidateWorkflowVersion(ctx, round, team)
			if versionErr != nil {
				return p.infraEvaluation(round, fmt.Sprintf(
					"scenario %s candidate workflow version failed: %v", scenario.ID, versionErr), ""), nil
			}
			scenarioCandidate, runID, status, err = p.buildAndRunCandidateFor(
				ctx, round, team, workflowID, draftVersion, roles[index], nil, scenario,
			)
		} else {
			scenarioCandidate, runID, status, err = p.buildAndRunCandidateFor(
				ctx, round, team, candidate.WorkflowID, candidate.WorkflowVersion,
				roles[index], candidate, scenario,
			)
		}
		if err != nil {
			return p.infraEvaluation(round, fmt.Sprintf(
				"scenario %s candidate test run failed: %v", scenario.ID, err), runtimeErrorCode(err)), nil
		}
		if scenarioCandidate == nil || strings.TrimSpace(scenarioCandidate.ContentHash) == "" {
			return p.infraEvaluation(round, fmt.Sprintf(
				"scenario %s produced no frozen candidate identity", scenario.ID), ""), nil
		}
		if candidateRef == "" {
			candidateRef = scenarioCandidate.ContentHash
		} else if candidateRef != scenarioCandidate.ContentHash {
			return p.infraEvaluation(round, fmt.Sprintf(
				"scenario %s candidate hash %s differs from suite hash %s",
				scenario.ID, scenarioCandidate.ContentHash, candidateRef), ""), nil
		}
		candidate = scenarioCandidate
		stageOutputs := p.candidateRunStageOutputs(ctx, workspaceID, runID)
		runs = append(runs, candidateScenarioRun{
			Scenario: scenario, RunID: runID, Status: status,
			Output:       formatCandidateRunEvidence(runID, status, stageOutputs),
			StageOutputs: stageOutputs,
		})
		if _, err := p.reconcileScenarioUsage(ctx, round, []string{roles[index]}); err != nil {
			return RoundEvaluation{}, fmt.Errorf(
				"evaluate phase: settle scenario %s usage: %w", scenario.ID, err,
			)
		}
		decision, err := p.Deps.Build.EvaluateBudget(
			ctx, workspaceID, round.BuildRunID, round.RoundNo,
			round.Run.RoundBudget, round.Run.TotalBudget,
		)
		if err != nil {
			return RoundEvaluation{}, fmt.Errorf(
				"evaluate phase: scenario %s budget gate: %w", scenario.ID, err,
			)
		}
		if len(decision.ExceededDims) > 0 {
			report := p.assembleReport(ctx, round, team, candidate, runs, nil)
			return budgetExhaustedEvaluation(report, candidateRef, decision), nil
		}
		if status != string(teamrun.StatusSucceeded) && status != "success" {
			_, errorCode, failureDetail, factsErr := p.candidateOutcomeFacts(
				ctx, workspaceID, runID,
			)
			if factsErr != nil {
				return p.infraEvaluation(round, fmt.Sprintf(
					"read scenario %s candidate outcome facts: %v",
					scenario.ID, factsErr), ""), nil
			}
			diagnosis := teameval.DiagnoseOutcome(teameval.OutcomeInput{
				TerminalStatus: status, OriginalErrorCode: errorCode, Detail: failureDetail,
			})
			if diagnosis.Class == teameval.FailureClassRuntimeInfrastructure {
				detail := failureDetail
				if detail == "" {
					detail = fmt.Sprintf("scenario %s candidate run %s ended with status %s",
						scenario.ID, runID, status)
				}
				return p.infraEvaluation(round, detail, errorCode), nil
			}
		}
	}
	if _, err := p.reconcileScenarioUsage(ctx, round, roles); err != nil {
		return RoundEvaluation{}, fmt.Errorf(
			"evaluate phase: settle candidate usage round %d: %w",
			round.RoundNo,
			err,
		)
	}

	for _, scenarioRun := range runs {
		if evidenceErr := p.insertDeliverableEvidence(
			ctx, round, team, scenarioRun.RunID, scenarioRun.Status,
		); evidenceErr != nil {
			return p.infraEvaluation(round, fmt.Sprintf(
				"record scenario %s deliverable evidence: %v",
				scenarioRun.Scenario.ID, evidenceErr), ""), nil
		}
	}

	gates, err := p.evaluateGates(ctx, workspaceID, round.BuildRunID, team.ID)
	if err != nil {
		return p.infraEvaluation(round, fmt.Sprintf("hard-gate evaluation failed: %v", err), ""), nil
	}

	p.mu.Lock()
	p.lastCandidate = candidate
	p.mu.Unlock()

	report := p.assembleReport(ctx, round, team, candidate, runs, gates)
	if scenarioRunsSucceeded(runs) {
		decision, budgetErr := p.Deps.Build.EvaluateBudget(
			ctx, workspaceID, round.BuildRunID, round.RoundNo,
			round.Run.RoundBudget, round.Run.TotalBudget,
		)
		if budgetErr != nil {
			return RoundEvaluation{}, fmt.Errorf("evaluate phase: semantic judge budget gate: %w", budgetErr)
		}
		if len(decision.ExceededDims) > 0 {
			return budgetExhaustedEvaluation(report, candidateRef, decision), nil
		}
		semantic, evalErr := p.semanticRubricEvaluation(ctx, round, runs)
		if evalErr != nil {
			return p.infraEvaluation(round, fmt.Sprintf("semantic rubric evaluation failed: %v", evalErr), ""), nil
		}
		report.RubricScores = semantic.Scores
		report.SevereDefects = semantic.SevereDefects
		// Deterministic keyword coverage is a preflight signal, not evidence of
		// a semantic business failure. Do not persist its excerpts after the
		// independent judge has produced the authoritative rubric result.
		report.FailureSamples = nil
	}
	diagnosticIndex := 0
	for index, scenarioRun := range runs {
		if scenarioRun.Status != string(teamrun.StatusSucceeded) && scenarioRun.Status != "success" {
			diagnosticIndex = index
			break
		}
	}
	allEvidence := make([]teameval.OutcomeEvidence, len(runs))
	var errorCode, failureDetail string
	for index, scenarioRun := range runs {
		facts, runErrorCode, runFailureDetail, evidenceLoadErr := p.candidateOutcomeFacts(
			ctx, workspaceID, scenarioRun.RunID,
		)
		if evidenceLoadErr != nil {
			return p.infraEvaluation(round, fmt.Sprintf(
				"read scenario %s candidate outcome facts: %v",
				scenarioRun.Scenario.ID, evidenceLoadErr), ""), nil
		}
		allEvidence[index] = facts
		if index == diagnosticIndex {
			errorCode = runErrorCode
			failureDetail = runFailureDetail
		}
	}
	evidence := allEvidence[diagnosticIndex]
	for index, facts := range allEvidence {
		if index == diagnosticIndex {
			continue
		}
		evidence.AdditionalRefs = append(evidence.AdditionalRefs,
			facts.RunRef, facts.TaskRef, facts.DeliverableRef,
		)
	}
	failedGates := make([]teameval.GateResult, 0, len(gates))
	for _, gate := range gates {
		if gate.Status == teameval.StatusFail {
			failedGates = append(failedGates, gate)
		}
	}
	diagnosis := teameval.DiagnoseOutcome(teameval.OutcomeInput{
		TerminalStatus: runs[diagnosticIndex].Status, OriginalErrorCode: errorCode,
		Evidence: evidence, FailedGates: failedGates,
		FailedRubric: teameval.FailedRubricDimensions(
			round.Run.Contract, report.RubricScores,
		),
		SevereDefects: report.SevereDefects, Detail: failureDetail,
	})
	conclusion, category := roundConclusion(
		round.Run.Contract, gates, report.RubricScores, report.SevereDefects,
	)
	if round.Run.EffectiveExecutionStrategy() == teambuild.ExecutionStrategyCompilerV1 {
		switch diagnosis.Class {
		case teameval.FailureClassPass:
			conclusion, category = teambuild.ConclusionPass, ""
		case teameval.FailureClassBusinessQuality:
			if diagnosis.RevisionAction == teameval.RevisionActionRequestBlueprintPatch {
				conclusion = teambuild.ConclusionRevise
			} else if len(report.SevereDefects) > 0 {
				conclusion = teambuild.ConclusionBlocked
			} else {
				conclusion = teambuild.ConclusionRevise
			}
		default:
			conclusion = teambuild.ConclusionBlocked
			category = string(diagnosis.Class)
			if diagnosis.OriginalErrorCode != "" {
				category = diagnosis.OriginalErrorCode
			}
		}
		if diagnosis.Class == teameval.FailureClassRuntimeInfrastructure {
			detail := diagnosis.FailureDetail
			if diagnosis.OriginalErrorCode != "" {
				detail = "original_error_code=" + diagnosis.OriginalErrorCode
			}
			report.InfraErrors = append(report.InfraErrors, detail)
		}
	}
	return RoundEvaluation{
		CandidateRef:    candidateRef,
		Gates:           mapGateResults(gates),
		Conclusion:      conclusion,
		FailureCategory: category,
		Diagnosis:       diagnosis,
		Report:          report,
	}, nil
}

func budgetExhaustedEvaluation(
	report teambuild.EvaluationReport,
	candidateRef string,
	decision teambuild.BudgetDecision,
) RoundEvaluation {
	detail := "budget_exhausted"
	if len(decision.ExceededDims) > 0 {
		detail += ": " + strings.Join(decision.ExceededDims, ", ")
	}
	return RoundEvaluation{
		CandidateRef: candidateRef, Conclusion: teambuild.ConclusionBlocked,
		FailureCategory: string(teameval.FailureClassBudgetExhausted), Report: report,
		Diagnosis: teameval.TypedDiagnosis{
			Class:             teameval.FailureClassBudgetExhausted,
			OriginalErrorCode: string(teameval.FailureClassBudgetExhausted),
			FailureDetail:     detail,
			RevisionAction:    teameval.RevisionActionBlockPlatformDiagnosis,
		},
	}
}

// PublishStep releases the passed round's candidate through the publication
// CAS and marks the build run passed. It must be invoked after the
// controller reports publishing; the candidate retained by Evaluate is
// verified against the round ledger's candidate_ref so the published content
// is exactly the evaluated content.
func (p *ProductionPhases) PublishStep(ctx context.Context, workspaceID, buildRunID string) error {
	run, err := p.Deps.Build.GetBuildRun(ctx, workspaceID, buildRunID)
	if err != nil {
		return fmt.Errorf("publish step: load build run: %w", err)
	}
	compilerV1 := run.EffectiveExecutionStrategy() == teambuild.ExecutionStrategyCompilerV1
	rounds, err := p.Deps.Build.ListRounds(ctx, workspaceID, buildRunID)
	if err != nil {
		return fmt.Errorf("publish step: list rounds: %w", err)
	}
	if len(rounds) == 0 {
		return errors.New("publish step: build run has no recorded rounds")
	}
	lastRound := rounds[len(rounds)-1]
	if lastRound.Conclusion != teambuild.ConclusionPass {
		return fmt.Errorf("publish step: last round conclusion is %q, want pass", lastRound.Conclusion)
	}
	roundReport, err := p.Deps.Build.GetRoundReport(ctx, workspaceID, buildRunID, lastRound.RoundNo)
	if err != nil {
		return fmt.Errorf("publish step: load final round report: %w", err)
	}
	if lastRound.ReportRef == "" || roundReport.ReportHash != lastRound.ReportRef {
		return errors.New("publish step: final round report binding is invalid")
	}
	candidateRef := lastRound.CandidateRef

	p.mu.Lock()
	candidate := p.lastCandidate
	p.mu.Unlock()
	if candidate == nil {
		return errors.New("publish step: no evaluated candidate retained for the passed round")
	}
	if candidateRef != "" && candidate.ContentHash != candidateRef {
		return fmt.Errorf(
			"publish step: candidate hash %s does not match round candidate_ref %s",
			candidate.ContentHash, candidateRef,
		)
	}

	alreadyPublished := false
	if compilerV1 {
		workflowRow, getErr := p.Deps.Workflows.Get(ctx, workspaceID, candidate.WorkflowID)
		if getErr == nil && workflowRow.PublishedVersion != nil && *workflowRow.PublishedVersion == candidate.WorkflowVersion {
			artifact, artifactErr := p.Deps.Workflows.GetArtifact(ctx, workspaceID, candidate.WorkflowID, candidate.WorkflowVersion)
			alreadyPublished = artifactErr == nil && artifact.ContentHash == candidate.ContentHash
		}
	}
	publication, err := workflow.PublicationFromCandidate(candidate)
	if err != nil {
		return fmt.Errorf("publish step: convert candidate: %w", err)
	}
	tx, err := p.Deps.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("publish step: begin atomic publication: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	lockedRun, err := p.Deps.Build.LockBuildRunTx(ctx, tx, workspaceID, buildRunID)
	if err != nil {
		return fmt.Errorf("publish step: lock build run: %w", err)
	}
	if lockedRun.Status != teambuild.StatusPublishing {
		return fmt.Errorf("publish step: locked run status is %q, want publishing", lockedRun.Status)
	}
	if err := p.settlePublishUsageTx(ctx, tx, lockedRun); err != nil {
		return fmt.Errorf("publish step: settle usage: %w", err)
	}
	decision, err := p.Deps.Build.EvaluateBudgetTx(
		ctx, tx, workspaceID, buildRunID, lastRound.RoundNo,
		lockedRun.RoundBudget, lockedRun.TotalBudget,
	)
	if err != nil {
		return fmt.Errorf("publish step: G5 budget gate: %w", err)
	}
	if len(decision.ExceededDims) > 0 {
		if _, err := p.Deps.Build.BlockPublishingBudgetTx(
			ctx, tx, workspaceID, buildRunID, DefaultActor,
		); err != nil {
			return fmt.Errorf("publish step: G5 atomic block: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("publish step: commit G5 atomic block: %w", err)
		}
		return ErrCompilerPublishBudgetExhausted
	}
	if blocksIncompleteUsage(roundReport.Report, lockedRun.Brief) {
		if _, err := p.Deps.Build.BlockPublishingBudgetTx(
			ctx, tx, workspaceID, buildRunID, DefaultActor,
		); err != nil {
			return fmt.Errorf("publish step: incomplete-usage atomic block: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("publish step: commit incomplete-usage atomic block: %w", err)
		}
		return ErrCompilerPublishBudgetExhausted
	}
	verifiedBaselineHash, err := p.Deps.Build.VerifyEvaluationBaselineTx(ctx, tx, workspaceID, buildRunID)
	if err != nil {
		_ = tx.Rollback(ctx)
		return p.blockEvaluationPublicationCAS(ctx, run, err)
	}
	if !alreadyPublished {
		if err := p.Deps.Workflows.InsertPublicationTx(ctx, tx, publication); err != nil {
			return fmt.Errorf("publish step: insert publication: %w", err)
		}
	}
	if !run.EvaluationOnly {
		if err := activatePublishedTeamTx(ctx, tx, workspaceID, candidate.Payload.Team.TeamID, candidate.Payload.Team.LeadAgentID); err != nil {
			return fmt.Errorf("publish step: activate team: %w", err)
		}
	}
	if _, err := p.Deps.Build.MarkPublishedTx(ctx, tx, workspaceID, buildRunID, DefaultActor, teambuild.FinalRef{
		Ref:    candidate.ContentHash,
		TeamID: candidate.Payload.Team.TeamID,
	}, verifiedBaselineHash); err != nil {
		_ = tx.Rollback(ctx)
		return p.blockEvaluationPublicationCAS(ctx, run, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("publish step: commit atomic publication: %w", err)
	}
	return nil
}

// FinalizeTemplatePublication turns a fully materialized template build into
// its first usable immutable workflow without running candidate scenarios or
// a semantic judge. Publication, default-workflow binding, Team activation,
// project lead binding, and BuildRun completion share one transaction.
func (p *ProductionPhases) FinalizeTemplatePublication(
	ctx context.Context,
	workspaceID, buildRunID, teamID, actor string,
) (teambuild.TeamBuildRun, error) {
	if p == nil || p.Deps.Pool == nil || p.Deps.Build == nil || p.Deps.Workflows == nil || p.builder == nil {
		return teambuild.TeamBuildRun{}, errors.New("template publication dependencies are unavailable")
	}
	workflowID, workflowVersion, err := p.templatePublicationTarget(ctx, workspaceID, teamID)
	if err != nil {
		return teambuild.TeamBuildRun{}, err
	}
	tx, err := p.Deps.Pool.Begin(ctx)
	if err != nil {
		return teambuild.TeamBuildRun{}, fmt.Errorf("begin template publication: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	lockedRun, err := p.Deps.Build.LockBuildRunTx(ctx, tx, workspaceID, buildRunID)
	if err != nil {
		return teambuild.TeamBuildRun{}, fmt.Errorf("lock template build run: %w", err)
	}
	if lockedRun.Status != teambuild.StatusRoundRunning ||
		lockedRun.EffectiveExecutionStrategy() != teambuild.ExecutionStrategyTemplateInstantiate {
		return teambuild.TeamBuildRun{}, fmt.Errorf("template build run is not finalizable: status=%s strategy=%s", lockedRun.Status, lockedRun.EffectiveExecutionStrategy())
	}

	candidate, report, err := p.builder.BuildTx(ctx, tx, workflow.CandidateInput{
		WorkspaceID: workspaceID, WorkflowID: workflowID, WorkflowVersion: workflowVersion,
	})
	if err != nil {
		return teambuild.TeamBuildRun{}, fmt.Errorf("build template publication candidate: %w", err)
	}
	if report != nil && len(report.Issues) != 0 {
		return teambuild.TeamBuildRun{}, fmt.Errorf("template workflow static validation failed with %d issue(s)", len(report.Issues))
	}
	if candidate == nil || candidate.Payload.Team.TeamID != teamID {
		return teambuild.TeamBuildRun{}, errors.New("template publication candidate team identity mismatch")
	}
	publication, err := workflow.PublicationFromCandidate(candidate)
	if err != nil {
		return teambuild.TeamBuildRun{}, fmt.Errorf("convert template publication candidate: %w", err)
	}
	if err := p.Deps.Workflows.InsertPublicationTx(ctx, tx, publication); err != nil {
		return teambuild.TeamBuildRun{}, fmt.Errorf("insert template publication: %w", err)
	}
	if err := activateTemplateTeamTx(
		ctx, tx, workspaceID, teamID, workflowID, candidate.Payload.Team.LeadAgentID,
	); err != nil {
		return teambuild.TeamBuildRun{}, fmt.Errorf("activate template team: %w", err)
	}
	finalized, err := p.Deps.Build.MarkTemplatePublishedTx(
		ctx, tx, workspaceID, buildRunID, actor,
		teambuild.FinalRef{Ref: candidate.ContentHash, TeamID: teamID},
	)
	if err != nil {
		return teambuild.TeamBuildRun{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return teambuild.TeamBuildRun{}, fmt.Errorf("commit template publication: %w", err)
	}
	return finalized, nil
}

func (p *ProductionPhases) templatePublicationTarget(
	ctx context.Context,
	workspaceID, teamID string,
) (string, int, error) {
	workflows, err := p.Deps.Workflows.ListByTeam(ctx, workspaceID, teamID)
	if err != nil {
		return "", 0, fmt.Errorf("list template workflows: %w", err)
	}
	active := make([]workflow.TeamWorkflow, 0, len(workflows))
	for _, item := range workflows {
		if item.Status == workflow.WorkflowStatusActive {
			active = append(active, item)
		}
	}
	if len(active) != 1 || active[0].PublishedVersion != nil {
		return "", 0, fmt.Errorf("template team must have exactly one unpublished active workflow (found %d)", len(active))
	}
	versions, err := p.Deps.Workflows.ListVersionsByWorkflows(ctx, workspaceID, []string{active[0].ID})
	if err != nil {
		return "", 0, fmt.Errorf("list template workflow versions: %w", err)
	}
	draftVersion := 0
	for _, version := range versions {
		if version.Status != workflow.VersionStatusDraft {
			continue
		}
		if draftVersion != 0 {
			return "", 0, errors.New("template workflow has multiple drafts")
		}
		draftVersion = version.Version
	}
	if draftVersion == 0 {
		return "", 0, errors.New("template workflow has no draft to publish")
	}
	return active[0].ID, draftVersion, nil
}

func activateTemplateTeamTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, teamID, workflowID, leadAvatarID string,
) error {
	if strings.TrimSpace(teamID) == "" || strings.TrimSpace(workflowID) == "" || strings.TrimSpace(leadAvatarID) == "" {
		return errors.New("template team activation identities are required")
	}
	tag, err := tx.Exec(ctx, `
		UPDATE weave_teams
		SET status='active', default_workflow_id=$3, updated_at=now()
		WHERE workspace_id=$1 AND id=$2 AND status='building'
		  AND lead_avatar_id=$4 AND evaluation='unevaluated'
	`, workspaceID, teamID, workflowID, leadAvatarID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("team %q is not a finalizable template team", teamID)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE weave_projects
		SET avatar_id=$3
		WHERE workspace_id=$1 AND team_id=$2
		  AND archived_at IS NULL
		  AND COALESCE(system_kind,'') <> 'unclassified'
	`, workspaceID, teamID, leadAvatarID); err != nil {
		return err
	}
	return nil
}

func allowsUnmeasuredUsage(brief teambuild.BuildBrief) bool {
	waiver := brief.UnmeasuredUsageWaiver
	return waiver != nil && waiver.Accepted && strings.TrimSpace(waiver.Reason) != ""
}

func blocksIncompleteUsage(report teambuild.EvaluationReport, brief teambuild.BuildBrief) bool {
	return report.UsageComplete != nil && !*report.UsageComplete && !allowsUnmeasuredUsage(brief)
}

func (p *ProductionPhases) settlePublishUsageTx(
	ctx context.Context,
	tx pgx.Tx,
	run teambuild.TeamBuildRun,
) error {
	sources, err := p.Deps.Build.ListAllUnchargedUsageSourcesTx(
		ctx, tx, run.WorkspaceID, run.BuildRunID,
	)
	if err != nil {
		return err
	}
	for _, source := range sources {
		marker, present, err := loomruntime.NewPGTerminalStateStore().
			ReadTerminalMarkerForUpdate(ctx, tx, run.WorkspaceID, source.SourceRunID)
		if err != nil {
			return err
		}
		if !present || marker.Phase != loomruntime.TerminalMarkerPhaseFinal {
			return fmt.Errorf("%w: source %s/%s", ErrUsageSourcePending, source.SourceKind, source.SourceRunID)
		}
		round := RoundContext{
			WorkspaceID: run.WorkspaceID, BuildRunID: run.BuildRunID,
			RoundNo: source.RoundNo, Run: run,
		}
		var charge teambuild.BudgetCharge
		switch source.SourceKind {
		case teambuild.UsageSourceKindBuildAgent:
			charge = usageChargeFromMarker(round, source.SourceRole, source.SourceRunID, marker)
		case teambuild.UsageSourceKindCandidateRuntime:
			charge = usageChargeFromCandidateMarker(round, source.SourceRole, source.SourceRunID, marker)
		default:
			return fmt.Errorf("unsupported usage source kind %q", source.SourceKind)
		}
		if _, err := p.Deps.Build.RecordBudgetUsageTx(
			ctx, tx, run.WorkspaceID, run.BuildRunID, charge,
		); err != nil {
			return err
		}
	}
	return nil
}

func (p *ProductionPhases) blockEvaluationPublicationCAS(
	ctx context.Context,
	run teambuild.TeamBuildRun,
	cause error,
) error {
	if !run.EvaluationOnly ||
		(!errors.Is(cause, teambuild.ErrEvaluationBaselineChanged) &&
			!errors.Is(cause, teambuild.ErrEvaluationPublishCAS)) {
		return fmt.Errorf("publish step: atomic publication: %w", cause)
	}
	if _, err := p.Deps.Build.TransitionStatus(ctx, run.WorkspaceID, run.BuildRunID,
		teambuild.StatusPublishing, teambuild.StatusBlocked, DefaultActor,
		"evaluation_baseline_cas_failed"); err != nil {
		return fmt.Errorf("publish step: block evaluation CAS failure: %v (original: %w)", err, cause)
	}
	return fmt.Errorf("publish step: evaluation blocked by baseline CAS: %w", cause)
}

func activatePublishedTeamTx(ctx context.Context, tx pgx.Tx, workspaceID, teamID, leadAvatarID string) error {
	if strings.TrimSpace(teamID) == "" || strings.TrimSpace(leadAvatarID) == "" {
		return nil
	}
	tag, err := tx.Exec(ctx, `
		UPDATE weave_teams
		SET status='active', lead_avatar_id=$3, evaluation='evaluated', updated_at=now()
		WHERE workspace_id=$1 AND id=$2 AND status IN ('building','active')
	`, workspaceID, teamID, leadAvatarID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("team %q is not publishable", teamID)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE weave_projects
		SET avatar_id=$3
		WHERE workspace_id=$1 AND team_id=$2
		  AND archived_at IS NULL
		  AND COALESCE(system_kind,'') <> 'unclassified'
	`, workspaceID, teamID, leadAvatarID); err != nil {
		return err
	}
	return nil
}

// targetTeam resolves the build brief's target team. Create mode names the
// team; optimize mode targets an existing team id.
func (p *ProductionPhases) targetTeam(
	ctx context.Context,
	workspaceID string,
	run teambuild.TeamBuildRun,
) (*org.Team, error) {
	if run.Mode == teambuild.ModeOptimize {
		if strings.TrimSpace(run.Brief.TeamID) == "" {
			return nil, errors.New("optimize build run has no target team id")
		}
		teams, err := p.Deps.Teams.ListTeams(ctx, workspaceID)
		if err != nil {
			return nil, fmt.Errorf("list teams: %w", err)
		}
		for i := range teams {
			if teams[i].ID == run.Brief.TeamID {
				return &teams[i], nil
			}
		}
		return nil, nil
	}
	name := strings.TrimSpace(run.Brief.NewTeamName)
	if name == "" {
		return nil, errors.New("create build run has no new_team_name")
	}
	teams, err := p.Deps.Teams.ListTeams(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list teams: %w", err)
	}
	for i := range teams {
		if teams[i].Name == name {
			return &teams[i], nil
		}
	}
	return nil, nil
}

// buildAndRunCandidate resolves the round's target workflow (create mode:
// the single active workflow; optimize mode: the baseline/asset-scope pinned
// workflow) and its latest draft, then builds and runs the candidate.
func (p *ProductionPhases) buildAndRunCandidate(
	ctx context.Context,
	round RoundContext,
	team *org.Team,
) (*workflow.PublicationCandidate, string, string, error) {
	workflowID, draftVersion, err := p.candidateWorkflowVersion(ctx, round, team)
	if err != nil {
		return nil, "", "", err
	}
	scenarios := evaluationScenarios(round.Run.Contract)
	if len(scenarios) == 0 {
		return nil, "", "", errors.New("evaluation contract has no runnable scenarios")
	}
	return p.buildAndRunCandidateFor(
		ctx, round, team, workflowID, draftVersion,
		teambuild.SourceRoleFixedWorkflowRoot, nil, scenarios[0],
	)
}

func (p *ProductionPhases) candidateWorkflowVersion(
	ctx context.Context,
	round RoundContext,
	team *org.Team,
) (string, int, error) {
	workspaceID := round.WorkspaceID
	workflowID := strings.TrimSpace(round.FrozenWorkflowID)
	draftVersion := round.FrozenWorkflowVersion
	if workflowID != "" || draftVersion != 0 {
		if workflowID == "" || draftVersion < 1 {
			return "", 0, errors.New("candidate workflow version binding does not match target workflow")
		}
		workflowRow, loadErr := p.Deps.Workflows.Get(ctx, workspaceID, workflowID)
		if loadErr != nil {
			return "", 0, fmt.Errorf("read compiler-bound workflow: %w", loadErr)
		}
		if team == nil || workflowRow.TeamID != team.ID || workflowRow.Status == workflow.WorkflowStatusArchived {
			return "", 0, errors.New("compiler-bound workflow does not belong to the target team")
		}
		version, loadErr := p.Deps.Workflows.GetVersion(ctx, workspaceID, workflowID, draftVersion)
		if loadErr != nil {
			return "", 0, fmt.Errorf("read compiler-bound workflow draft: %w", loadErr)
		}
		if version.Status != workflow.VersionStatusDraft {
			return "", 0, errors.New("compiler-bound workflow version is not a draft")
		}
		return workflowID, draftVersion, nil
	}

	workflowID, err := p.targetWorkflowID(ctx, workspaceID, round.Run, team)
	if err != nil {
		return "", 0, err
	}
	draftVersion, err = p.latestDraftVersion(ctx, workspaceID, workflowID)
	if err != nil {
		return "", 0, err
	}
	return workflowID, draftVersion, nil
}

// targetWorkflowID pins the workflow this round evaluates. Create mode keeps
// the "first non-archived workflow of the freshly created team" rule.
// Optimize mode uses the baseline snapshot + asset scope to pin the exact
// workflow the brief froze — never "the first non-archived" (T15C).
func (p *ProductionPhases) targetWorkflowID(
	ctx context.Context,
	workspaceID string,
	run teambuild.TeamBuildRun,
	team *org.Team,
) (string, error) {
	if run.Mode == teambuild.ModeOptimize {
		return p.pinnedOptimizeWorkflowID(run)
	}
	workflows, err := p.Deps.Workflows.ListByTeam(ctx, workspaceID, team.ID)
	if err != nil {
		return "", fmt.Errorf("list team workflows: %w", err)
	}
	var workflowID string
	for _, wf := range workflows {
		if wf.Status != workflow.WorkflowStatusArchived {
			workflowID = wf.ID
			break
		}
	}
	if workflowID == "" {
		return "", errors.New("target team has no active workflow")
	}
	return workflowID, nil
}

// pinnedOptimizeWorkflowID returns the single workflow the optimize brief
// pinned. Existing workflow optimizations must pin a workflow frozen in the
// baseline snapshot; the one allowed creation case is an optimize run whose
// baseline had no workflows and whose scope pins the deterministic first
// workflow ID for the target team.
func (p *ProductionPhases) pinnedOptimizeWorkflowID(run teambuild.TeamBuildRun) (string, error) {
	if run.Baseline == nil {
		return "", errors.New("optimize build run has no baseline snapshot")
	}
	baselineWorkflows := make(map[string]struct{}, len(run.Baseline.Workflows))
	for _, ref := range run.Baseline.Workflows {
		baselineWorkflows[ref.WorkflowID] = struct{}{}
	}
	var scoped []string
	for _, ref := range run.AssetScope.Refs {
		if ref.Kind != "workflow" || ref.ID == "" {
			continue
		}
		if _, frozen := baselineWorkflows[ref.ID]; !frozen {
			if len(run.Baseline.Workflows) == 0 &&
				ref.ID == teambuild.FirstOptimizeWorkflowID(run.Baseline.Team.TeamID) {
				scoped = append(scoped, ref.ID)
				continue
			}
			return "", fmt.Errorf(
				"optimize scope pins workflow %q that is not frozen in the baseline snapshot",
				ref.ID,
			)
		}
		scoped = append(scoped, ref.ID)
	}
	if len(scoped) == 0 {
		return "", errors.New("optimize asset scope pins no workflow")
	}
	if len(scoped) > 1 {
		return "", fmt.Errorf(
			"optimize asset scope pins %d workflows; exactly one is required for this ticket",
			len(scoped),
		)
	}
	return scoped[0], nil
}

// latestDraftVersion returns the highest draft version of one workflow, or an
// error when the workflow has no draft to evaluate.
func (p *ProductionPhases) latestDraftVersion(
	ctx context.Context,
	workspaceID, workflowID string,
) (int, error) {
	versions, err := p.Deps.Workflows.ListVersionsByWorkflows(ctx, workspaceID, []string{workflowID})
	if err != nil {
		return 0, fmt.Errorf("list workflow versions: %w", err)
	}
	draftVersion := 0
	for _, version := range versions {
		if version.Status == workflow.VersionStatusDraft && version.Version > draftVersion {
			draftVersion = version.Version
		}
	}
	if draftVersion == 0 {
		return 0, errors.New("target workflow has no draft to evaluate")
	}
	return draftVersion, nil
}

// buildAndRunCandidateFor builds and persists the round candidate, admits and
// runs the candidate test run, and returns the candidate plus the test run's
// snapshot id and terminal status. sourceRole distinguishes the round's own
// candidate (fixed_workflow_root) from the optimize baseline candidate
// (baseline_workflow_root) in the T14B usage-source ledger.
func (p *ProductionPhases) buildAndRunCandidateFor(
	ctx context.Context,
	round RoundContext,
	team *org.Team,
	workflowID string,
	draftVersion int,
	sourceRole string,
	frozenCandidate *workflow.PublicationCandidate,
	scenario evaluationScenario,
) (*workflow.PublicationCandidate, string, string, error) {
	workspaceID := round.WorkspaceID
	tx, err := p.Deps.Pool.Begin(ctx)
	if err != nil {
		return nil, "", "", fmt.Errorf("begin candidate build: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	candidate := frozenCandidate
	if candidate == nil {
		var buildReport *machine.Report
		candidate, buildReport, err = p.builder.BuildTx(ctx, tx, workflow.CandidateInput{
			WorkspaceID:     workspaceID,
			WorkflowID:      workflowID,
			WorkflowVersion: draftVersion,
		})
		if err != nil {
			return nil, "", "", fmt.Errorf("build publication candidate: %w", err)
		}
		if buildReport != nil && len(buildReport.Issues) != 0 {
			return nil, "", "", fmt.Errorf("candidate validation: %s", buildReport.Issues[0].Code)
		}
	}
	// Candidates are content-addressed and immutable: re-freezing identical
	// content (the optimize baseline of an unchanged published workflow, or a
	// round that re-committed the same draft) reuses the existing row instead
	// of colliding on the content-hash primary key.
	if _, err := p.Deps.Workflows.GetCandidateTx(
		ctx, tx, workspaceID, workflowID, candidate.ContentHash,
	); err != nil {
		if !errors.Is(err, workflow.ErrCandidateNotFound) {
			return nil, "", "", fmt.Errorf("check existing candidate: %w", err)
		}
		if err := p.Deps.Workflows.InsertCandidateTx(ctx, tx, candidate, DefaultActor); err != nil {
			return nil, "", "", fmt.Errorf("insert candidate: %w", err)
		}
	}
	admitted, err := p.Deps.Workflows.AdmitWorkflowCandidateRunTx(
		ctx, tx, workflow.WorkflowCandidateRunAdmissionRequest{
			WorkspaceID: workspaceID,
			WorkflowID:  workflowID,
			BuildRunID:  round.BuildRunID,
			RoundNo:     round.RoundNo,
			ContentHash: candidate.ContentHash,
			SourceRef:   round.BuildRunID,
			TriggerType: "api",
		},
	)
	if err != nil {
		return nil, "", "", fmt.Errorf("admit candidate run: %w", err)
	}
	created, err := p.Deps.Snapshots.CreateTx(ctx, tx, admitted)
	if err != nil {
		return nil, "", "", fmt.Errorf("create candidate snapshot: %w", err)
	}
	// T14B-2A: associate the candidate runtime with the round's usage source
	// table in the same transaction as admission, before any graph work, so
	// a crash before the terminal marker/ledger can be reconciled instead of
	// starting a duplicate candidate.
	if _, err := p.Deps.Build.RecordUsageSourceTx(
		ctx,
		tx,
		workspaceID,
		round.BuildRunID,
		teambuild.BuildUsageSource{
			WorkspaceID: workspaceID,
			BuildRunID:  round.BuildRunID,
			RoundNo:     round.RoundNo,
			SourceKind:  teambuild.UsageSourceKindCandidateRuntime,
			SourceRole:  sourceRole,
			SourceRunID: created.RunID,
		},
	); err != nil {
		return nil, "", "", fmt.Errorf("record candidate usage source: %w", err)
	}
	rootTask := &taskqueue.Task{
		// The task identity must be unique per admitted run: the optimize
		// baseline and a round can freeze identical content, so a content-hash
		// key alone would collide on the task queue primary key.
		ID:                    "task-eval-" + shortID(workspaceID, round.BuildRunID, created.RunID),
		WorkspaceID:           workspaceID,
		IdentityKind:          taskqueue.IdentityTeamWorkflow,
		IdentitySchemaVersion: 2,
		WorkflowID:            workflowID,
		WorkflowVersion:       draftVersion,
		RunSnapshotID:         created.RunID,
		Source:                "api",
		Kind:                  "team_workflow",
		Payload:               candidateRunPayload(scenario),
	}
	if err := p.Deps.Tasks.EnqueueTx(ctx, tx, rootTask); err != nil {
		return nil, "", "", fmt.Errorf("enqueue candidate run task: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, "", "", fmt.Errorf("commit candidate build: %w", err)
	}

	status, err := p.driveCandidateRun(ctx, workspaceID, created.RunID)
	if err != nil {
		return candidate, created.RunID, "", err
	}
	return candidate, created.RunID, status, nil
}

// driveCandidateRun executes the candidate test run to a terminal state with
// the same executor + reconciler assembly teamrun's workers use, replacing
// the production host factory through HostFactoryForSnapshot when a seam is
// injected.
func (p *ProductionPhases) driveCandidateRun(
	ctx context.Context,
	workspaceID, runID string,
) (string, error) {
	runStore := teamrun.NewPGStore()
	checkpointStore := teamrun.NewPGCheckpointStore()
	consumer := &teamrun.Consumer{
		Transactions: p.Deps.Pool,
		Snapshots:    p.Deps.Snapshots,
		Runs:         runStore,
		Tasks:        p.Deps.Tasks,
	}
	runtime := &teamrun.WorkflowSerialRuntime{
		Artifacts:              p.Deps.Workflows,
		Loader:                 p.candidateRuntimeLoader(),
		HostFactory:            workflow.NewRuntimeHostFactory(),
		HostFactoryForSnapshot: p.Deps.HostFactoryForSnapshot,
		CredentialResolvers: func(workspaceID string) (workflow.RuntimeCredentialResolver, error) {
			return credentials.NewPoolCredentialResolver(
				p.Deps.Pool,
				workspaceID,
				credentials.TxCredentialSources{
					ProviderGate:    p.Deps.Providers.ValidateReferenceTx,
					ProviderResolve: p.Deps.Providers.ResolveProviderAPIKeyTx,
					MCPGate:         p.Deps.MCPs.ValidateReferenceTx,
					MCPResolve:      p.Deps.MCPs.ResolveMCPAccessTx,
					RuntimeGate:     p.Deps.Runtimes.ValidateReferenceTx,
					RuntimeResolve:  p.Deps.Runtimes.ResolveRuntimeAccessTx,
					DeliveryGate:    p.Deps.Delivery.ValidateReferenceTx,
					DeliveryResolve: p.Deps.Delivery.ResolveDeliveryAccessTx,
				},
			)
		},
		Transactions: p.Deps.Pool,
		Runs:         runStore,
		Checkpoints:  checkpointStore,
		Tasks:        p.Deps.Tasks,
		Snapshots:    p.Deps.Snapshots,
	}
	checkpointReader := &teamrun.FanoutCheckpointReader{
		Transactions: p.Deps.Pool, Runs: runStore, Checkpoints: checkpointStore,
	}
	coordinator := fanout.NewWorkflowCoordinator(p.Deps.Pool, p.Deps.Fanout, checkpointReader)
	records := storeext.New(p.Deps.Pool)
	coordinator.CreatorLeases = &teamrun.FanoutCreatorLeaseReader{
		Transactions: p.Deps.Pool, Records: records,
	}
	coordinator.Resumer = &teamrun.FanoutParentRunResumer{
		Transactions: p.Deps.Pool, Fanout: p.Deps.Fanout, Runs: runStore,
		Checkpoints: checkpointStore, Tasks: p.Deps.Tasks, Records: records,
	}
	coordinator.Tasks = p.Deps.Tasks
	executor := &teamrun.Executor{
		MemberBudgets:      loomruntime.MemberBudgetCoordinator{},
		Tasks:              p.Deps.Tasks,
		Consumer:           consumer,
		Transactions:       p.Deps.Pool,
		Runs:               runStore,
		Checkpoints:        checkpointStore,
		Runtime:            runtime,
		Fanout:             teamrun.FanoutCoordinatorAdapter{Coordinator: coordinator},
		HeartbeatInterval:  time.Second,
		ClaimWorkspaceID:   workspaceID,
		ClaimRunSnapshotID: runID,
	}
	reconciler := &fanout.WorkflowReconcilerWorker{
		Transactions: p.Deps.Pool,
		Store:        p.Deps.Fanout,
		Coordinator:  coordinator,
		BatchSize:    16,
	}

	status, err := driveCandidateRunLoop(
		ctx,
		runID,
		candidateDriveLoopConfig{
			MaxDuration:      30 * time.Minute,
			IdlePollInterval: 100 * time.Millisecond,
		},
		func(loopCtx context.Context) (bool, error) {
			return executor.ProcessNext(loopCtx, DefaultActor)
		},
		func(loopCtx context.Context) (int, error) {
			return reconciler.Sweep(loopCtx)
		},
		func(loopCtx context.Context) (teamrun.Status, error) {
			return p.readTeamRunStatus(loopCtx, runStore, workspaceID, runID)
		},
	)
	if err == nil {
		return status, nil
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cleanupCancel()
	if cleanupErr := p.cancelCandidateRun(cleanupCtx, runStore, workspaceID, runID); cleanupErr != nil {
		return "", errors.Join(err, fmt.Errorf("cancel candidate run: %w", cleanupErr))
	}
	return "", err
}

func (p *ProductionPhases) cancelCandidateRun(
	ctx context.Context,
	runStore *teamrun.PGStore,
	workspaceID, runID string,
) error {
	if _, err := p.Deps.Tasks.CancelRunTasks(ctx, workspaceID, runID); err != nil {
		return err
	}
	tx, err := p.Deps.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin candidate cancellation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	run, err := runStore.GetForUpdateTx(ctx, tx, workspaceID, runID)
	if err != nil {
		return err
	}
	if run.Status.Terminal() {
		return tx.Commit(ctx)
	}
	now := time.Now().UTC()
	switch run.Status {
	case teamrun.StatusQueued:
		_, err = runStore.CancelQueuedTx(ctx, tx, teamrun.CancelQueuedRequest{
			WorkspaceID: workspaceID, RunID: runID, ExpectedStatus: run.Status,
			ExpectedTeamRunGeneration: run.Generation, ExpectedExecutionLeaseEpoch: run.ExecutionLeaseEpoch,
			ExpectedResumeGeneration: run.ResumeGeneration,
			IdempotencyKey:           "candidate-driver-cancel-queued:" + runID,
			Actor:                    DefaultActor, Source: "candidate_driver", OccurredAt: now,
		})
	case teamrun.StatusRunning, teamrun.StatusParked:
		run, err = runStore.RequestCancelTx(ctx, tx, teamrun.RequestCancelRequest{
			WorkspaceID: workspaceID, RunID: runID, ExpectedStatus: run.Status,
			ExpectedTeamRunGeneration: run.Generation, ExpectedExecutionLeaseEpoch: run.ExecutionLeaseEpoch,
			ExpectedResumeGeneration: run.ResumeGeneration,
			CancelActor:              DefaultActor, CancelReason: "candidate driver ended before TeamRun terminal",
			GraceDeadlineAt: now, IdempotencyKey: "candidate-driver-request-cancel:" + runID,
			Actor: DefaultActor, Source: "candidate_driver", OccurredAt: now,
		})
		if err == nil {
			_, err = runStore.ConfirmCancelTx(ctx, tx, teamrun.ConfirmCancelRequest{
				WorkspaceID: workspaceID, RunID: runID, ExpectedStatus: run.Status,
				ExpectedTeamRunGeneration: run.Generation, ExpectedExecutionLeaseEpoch: run.ExecutionLeaseEpoch,
				ExpectedResumeGeneration: run.ResumeGeneration,
				IdempotencyKey:           "candidate-driver-confirm-cancel:" + runID,
				Actor:                    DefaultActor, Source: "candidate_driver", OccurredAt: now,
			})
		}
	case teamrun.StatusCancelRequested:
		_, err = runStore.ConfirmCancelTx(ctx, tx, teamrun.ConfirmCancelRequest{
			WorkspaceID: workspaceID, RunID: runID, ExpectedStatus: run.Status,
			ExpectedTeamRunGeneration: run.Generation, ExpectedExecutionLeaseEpoch: run.ExecutionLeaseEpoch,
			ExpectedResumeGeneration: run.ResumeGeneration,
			IdempotencyKey:           "candidate-driver-confirm-cancel:" + runID,
			Actor:                    DefaultActor, Source: "candidate_driver", OccurredAt: now,
		})
	default:
		err = fmt.Errorf("candidate run %q has unsupported cancellation status %q", runID, run.Status)
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type candidateDriveLoopConfig struct {
	MaxDuration      time.Duration
	IdlePollInterval time.Duration
}

// driveCandidateRunLoop is time-bounded instead of iteration-bounded because
// one logical workflow may park and resume several times while remote runtime
// workers finish fanout legs. A fixed iteration budget can expire even while
// those workers are making healthy progress, and an empty local claim does not
// prove a stall when another executor currently owns the task.
func driveCandidateRunLoop(
	ctx context.Context,
	runID string,
	config candidateDriveLoopConfig,
	processNext func(context.Context) (bool, error),
	sweep func(context.Context) (int, error),
	readStatus func(context.Context) (teamrun.Status, error),
) (string, error) {
	if config.MaxDuration <= 0 {
		return "", errors.New("candidate drive max duration must be positive")
	}
	if config.IdlePollInterval <= 0 {
		return "", errors.New("candidate drive idle poll interval must be positive")
	}

	loopCtx, cancel := context.WithTimeout(ctx, config.MaxDuration)
	defer cancel()

	var lastStatus teamrun.Status
	for {
		if err := candidateDriveContextError(ctx, loopCtx, runID, config.MaxDuration, lastStatus); err != nil {
			return "", err
		}

		processed, err := processNext(loopCtx)
		if err != nil {
			if contextErr := candidateDriveContextError(ctx, loopCtx, runID, config.MaxDuration, lastStatus); contextErr != nil {
				return "", contextErr
			}
			// ProcessNext may have claimed and advanced the durable TeamRun before
			// a retryable execution failure occurs. The task queue keeps that work
			// eligible for retry, so failing the enclosing BuildRun here creates a
			// split brain: candidate_run=failed while the same TeamRun later
			// succeeds. The TeamRun terminal state is the authority; keep driving
			// while it remains non-terminal and let the bounded loop own timeout.
			status, statusErr := readStatus(loopCtx)
			if statusErr != nil {
				return "", fmt.Errorf("execute candidate run: %w (read status: %v)", err, statusErr)
			}
			lastStatus = status
			if status.Terminal() {
				return string(status), nil
			}
			timer := time.NewTimer(config.IdlePollInterval)
			select {
			case <-loopCtx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return "", candidateDriveContextError(ctx, loopCtx, runID, config.MaxDuration, lastStatus)
			case <-timer.C:
			}
			continue
		}
		swept, err := sweep(loopCtx)
		if err != nil {
			if contextErr := candidateDriveContextError(ctx, loopCtx, runID, config.MaxDuration, lastStatus); contextErr != nil {
				return "", contextErr
			}
			return "", fmt.Errorf("reconcile candidate run: %w", err)
		}
		status, err := readStatus(loopCtx)
		if err != nil {
			if contextErr := candidateDriveContextError(ctx, loopCtx, runID, config.MaxDuration, lastStatus); contextErr != nil {
				return "", contextErr
			}
			return "", err
		}
		lastStatus = status
		if status.Terminal() {
			return string(status), nil
		}
		if processed || swept > 0 {
			continue
		}

		timer := time.NewTimer(config.IdlePollInterval)
		select {
		case <-loopCtx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return "", candidateDriveContextError(ctx, loopCtx, runID, config.MaxDuration, lastStatus)
		case <-timer.C:
		}
	}
}

func candidateDriveContextError(
	parentCtx, loopCtx context.Context,
	runID string,
	maxDuration time.Duration,
	lastStatus teamrun.Status,
) error {
	if err := parentCtx.Err(); err != nil {
		return err
	}
	if !errors.Is(loopCtx.Err(), context.DeadlineExceeded) {
		return nil
	}
	return fmt.Errorf(
		"candidate run %q did not reach terminal within %s (last status %q)",
		runID,
		maxDuration,
		lastStatus,
	)
}

func (p *ProductionPhases) candidateRuntimeLoader() *workflow.RuntimeLoader {
	return &workflow.RuntimeLoader{
		Registry:    p.Deps.Descriptors,
		CLIExecutor: p.Deps.CLIExecutor,
	}
}

func (p *ProductionPhases) readTeamRunStatus(
	ctx context.Context,
	runStore *teamrun.PGStore,
	workspaceID, runID string,
) (teamrun.Status, error) {
	tx, err := p.Deps.Pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin run status read: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	run, err := runStore.GetTx(ctx, tx, workspaceID, runID)
	if err != nil {
		if errors.Is(err, teamrun.ErrTeamRunIdentityMismatch) {
			return "", nil
		}
		return "", fmt.Errorf("read candidate run status: %w", err)
	}
	return run.Status, nil
}

// insertDeliverableEvidence persists the candidate test run's artifact
// evidence row. The deliverable store only exposes the message-promotion
// write path, which requires session-outbox rows that candidate test runs do
// not create, so the phase records the immutable artifact row directly; the
// table's immutability trigger protects it afterwards.
func (p *ProductionPhases) insertDeliverableEvidence(
	ctx context.Context,
	round RoundContext,
	team *org.Team,
	runID, status string,
) error {
	workspaceID := round.WorkspaceID
	content := fmt.Sprintf("team forge candidate test run %s reached terminal status %s", runID, status)
	if output := p.candidateRunOutput(ctx, workspaceID, runID); output != "" {
		content = output
	}
	projectID, conversationID, userID, err := p.resolveBuildRunConversation(ctx, workspaceID, round.Run.ConversationID)
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(workspaceID + "\x1f" + "team-forge-eval" + "\x1f" + runID))
	_, err = p.Deps.Pool.Exec(ctx, `
		INSERT INTO weave_final_deliverables (
			id, workspace_id, project_id, conversation_id, user_id, lead_avatar_id,
			session_id, event_id, run_id, run_snapshot_id, title, content,
			content_type, metadata, created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$8,$8,$9,$10,'text/markdown','{}'::jsonb,$11)
		ON CONFLICT (workspace_id, user_id, lead_avatar_id, session_id, event_id) DO NOTHING
	`,
		"deliverable_"+hex.EncodeToString(digest[:16]),
		workspaceID,
		nullableTextValue(projectID),
		nullableTextValue(conversationID),
		userID,
		team.LeadAvatarID,
		"team-forge-eval",
		runID,
		"Team forge candidate test run output",
		content,
		time.Now().UTC(),
	)
	if err != nil {
		return fmt.Errorf("insert candidate deliverable evidence: %w", err)
	}
	return nil
}

func nullableTextValue(value string) any {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return value
}

func (p *ProductionPhases) resolveBuildRunConversation(
	ctx context.Context,
	workspaceID, conversationID string,
) (projectID, resolvedConversationID, userID string, err error) {
	userID = "system"
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		return "", "", userID, nil
	}
	err = p.Deps.Pool.QueryRow(ctx, `
		SELECT project_id, id, user_id
		FROM weave_conversations
		WHERE workspace_id=$1 AND id=$2
	`, workspaceID, conversationID).Scan(&projectID, &resolvedConversationID, &userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", "system", nil
	}
	if err != nil {
		return "", "", "", fmt.Errorf("resolve build run deliverable conversation: %w", err)
	}
	return projectID, resolvedConversationID, userID, nil
}

// candidateRunOutput returns a readable evidence bundle for one candidate run,
// including all terminal task results attributable to the same run snapshot.
// Remote CLI/Codex worker tasks are agent tasks: they do not own TeamRun.run_id
// but do carry run_snapshot_id, so both identities must be accepted here.
func (p *ProductionPhases) candidateRunOutput(ctx context.Context, workspaceID, runID string) string {
	return formatCandidateRunEvidence(
		runID,
		"",
		p.candidateRunStageOutputs(ctx, workspaceID, runID),
	)
}

func (p *ProductionPhases) candidateRunStageOutputs(
	ctx context.Context,
	workspaceID, runID string,
) []candidateStageOutput {
	const batch = 500
	stages := make([]candidateStageOutput, 0)
	seen := make(map[string]bool)
	for offset := 0; ; offset += batch {
		tasks, _, err := p.Deps.Tasks.List(ctx, workspaceID, batch, offset)
		if err != nil {
			return stages
		}
		for _, task := range tasks {
			if seen[task.ID] || task.Status != taskqueue.StatusCompleted || len(task.Result) == 0 {
				continue
			}
			if task.RunID != runID && task.RunSnapshotID != runID {
				continue
			}
			output := strings.TrimSpace(decodeTaskResult(task.Result))
			if output == "" {
				continue
			}
			seen[task.ID] = true
			stages = append(stages, candidateStageOutput{
				TaskID: task.ID, Source: task.Source, Kind: task.Kind, Agent: task.Agent,
				TerminalStatus: task.Status, Output: truncateCandidateEvidence(output),
			})
		}
		if len(tasks) < batch {
			for left, right := 0, len(stages)-1; left < right; left, right = left+1, right-1 {
				stages[left], stages[right] = stages[right], stages[left]
			}
			return stages
		}
	}
}

func decodeTaskResult(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var engineResult struct {
		Output string `json:"output"`
	}
	if json.Unmarshal(raw, &engineResult) == nil && strings.TrimSpace(engineResult.Output) != "" {
		return engineResult.Output
	}
	return string(raw)
}

func formatCandidateRunEvidence(
	runID, status string,
	stages []candidateStageOutput,
) string {
	var builder strings.Builder
	builder.WriteString("# Candidate run evidence\n\n")
	builder.WriteString("run_id: ")
	builder.WriteString(runID)
	builder.WriteString("\n")
	if status != "" {
		builder.WriteString("terminal_status: ")
		builder.WriteString(status)
		builder.WriteString("\n")
	}
	if len(stages) == 0 {
		builder.WriteString("\nNo completed task output was attributable to this candidate run.\n")
		return builder.String()
	}
	for index, stage := range stages {
		builder.WriteString("\n## Stage ")
		builder.WriteString(fmt.Sprint(index + 1))
		builder.WriteString(" · ")
		builder.WriteString(stage.Source)
		builder.WriteString("/")
		builder.WriteString(stage.Kind)
		if stage.Agent != "" {
			builder.WriteString(" · ")
			builder.WriteString(stage.Agent)
		}
		builder.WriteString("\n\n")
		builder.WriteString("task_id: ")
		builder.WriteString(stage.TaskID)
		builder.WriteString("\nstatus: ")
		builder.WriteString(stage.TerminalStatus)
		builder.WriteString("\n\n```text\n")
		builder.WriteString(stage.Output)
		builder.WriteString("\n```\n")
	}
	return builder.String()
}

func truncateCandidateEvidence(output string) string {
	const limit = 12000
	output = strings.TrimSpace(output)
	if len(output) <= limit {
		return output
	}
	return output[:limit] + "\n\n[truncated candidate stage evidence]"
}

// candidateOutcomeFacts reads the persisted run, task, and deliverable
// identities used by the typed router. Missing layers remain empty: the
// router then fails closed as infrastructure instead of manufacturing a
// business-quality signal.
func (p *ProductionPhases) candidateOutcomeFacts(
	ctx context.Context,
	workspaceID, runID string,
) (teameval.OutcomeEvidence, string, string, error) {
	evidence := teameval.OutcomeEvidence{}
	tx, err := p.Deps.Pool.Begin(ctx)
	if err != nil {
		return evidence, "", "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	run, err := teamrun.NewPGStore().GetTx(ctx, tx, workspaceID, runID)
	if err != nil {
		return evidence, "", "", err
	}
	evidence.RunID = run.RunID
	evidence.RunSnapshotID = run.RunSnapshotID
	evidence.RunRef = fmt.Sprintf("run_id=%s run_snapshot_id=%s", run.RunID, run.RunSnapshotID)
	errorCode := ""
	if run.ErrorCode != nil {
		errorCode = string(*run.ErrorCode)
	}
	failureDetail := ""
	if run.CauseSummary != nil {
		failureDetail = *run.CauseSummary
	}

	const batch = 500
	for offset := 0; ; offset += batch {
		tasks, _, listErr := p.Deps.Tasks.List(ctx, workspaceID, batch, offset)
		if listErr != nil {
			return evidence, errorCode, failureDetail, listErr
		}
		for _, task := range tasks {
			if task.RunSnapshotID == run.RunSnapshotID &&
				(task.RunID == "" || task.RunID == run.RunID) {
				evidence.TaskRef = "task=" + task.ID
				break
			}
		}
		if evidence.TaskRef != "" || len(tasks) < batch {
			break
		}
	}
	var deliverableID string
	err = tx.QueryRow(ctx, `
		SELECT id
		FROM weave_final_deliverables
		WHERE workspace_id=$1
		  AND run_id=$2
		  AND run_snapshot_id=$3
		  AND btrim(content) <> ''
		ORDER BY created_at DESC, id DESC
		LIMIT 1
	`, workspaceID, run.RunID, run.RunSnapshotID).Scan(&deliverableID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return evidence, errorCode, failureDetail, err
	}
	if strings.TrimSpace(deliverableID) != "" {
		evidence.DeliverableRef = "deliverable=" + deliverableID
	}
	return evidence, errorCode, failureDetail, nil
}

func (p *ProductionPhases) candidateRunFailureFacts(
	ctx context.Context,
	workspaceID, runID string,
) (string, string, error) {
	tx, err := p.Deps.Pool.Begin(ctx)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	run, err := teamrun.NewPGStore().GetTx(ctx, tx, workspaceID, runID)
	if err != nil {
		return "", "", err
	}
	errorCode := ""
	if run.ErrorCode != nil {
		errorCode = string(*run.ErrorCode)
	}
	failureDetail := ""
	if run.CauseSummary != nil {
		failureDetail = *run.CauseSummary
	}
	return errorCode, failureDetail, nil
}

func candidateRunRetryRequired(attempt int, errorCode, failureDetail string) bool {
	if attempt < 2 || attempt > 3 || errorCode != string(teamrun.ErrorCodeExecutionUnrecoverable) {
		return false
	}
	diagnosis := teameval.DiagnoseOutcome(teameval.OutcomeInput{
		TerminalStatus: "failed", OriginalErrorCode: errorCode, Detail: failureDetail,
	})
	return diagnosis.RevisionAction == teameval.RevisionActionResumeSameRevision
}

func runtimeErrorCode(err error) string {
	var classified *teamrun.ExecutionError
	if errors.As(err, &classified) && classified != nil && teamrun.ValidateErrorCode(classified.Code) {
		return string(classified.Code)
	}
	return ""
}

// evaluateGates runs the full hard-gate evaluator over the round's frozen
// configuration.
func (p *ProductionPhases) evaluateGates(
	ctx context.Context,
	workspaceID, buildRunID, teamID string,
) ([]teameval.GateResult, error) {
	evaluator := teameval.NewGateEvaluator(teameval.Deps{
		Pool:            p.Deps.Pool,
		BuildRuns:       p.Deps.Build,
		Models:          teameval.ModelResolverFunc(credentials.ResolveModelRevisionTx),
		Teams:           p.Deps.Teams,
		Roster:          p.Deps.TeamWorkers,
		Agents:          p.Deps.Agents,
		Workflows:       p.Deps.Workflows,
		MCPs:            p.Deps.MCPs,
		Runtimes:        p.Deps.Runtimes,
		SkillsNamespace: p.Deps.Store,
		Snapshots:       p.Deps.Snapshots,
		Tasks:           p.Deps.Tasks,
		Deliverables:    p.Deps.Deliverables,
		Audit:           p.Deps.Audit,
		Runs:            p.Deps.Store,
	})
	return evaluator.Evaluate(ctx, workspaceID, buildRunID, teamID)
}

// assembleReport builds the immutable round report from the evaluated
// configuration, the candidate test run, and the gate results.
func (p *ProductionPhases) assembleReport(
	ctx context.Context,
	round RoundContext,
	team *org.Team,
	candidate *workflow.PublicationCandidate,
	runs []candidateScenarioRun,
	gates []teameval.GateResult,
) teambuild.EvaluationReport {
	scenarioResults := make([]teambuild.ScenarioResult, 0, len(runs))
	artifacts := make([]teameval.ScenarioArtifact, 0, len(runs))
	for _, scenarioRun := range runs {
		scenarioResults = append(scenarioResults, teambuild.ScenarioResult{
			ScenarioID: scenarioRun.Scenario.ID, InputVersion: scenarioRun.Scenario.InputVersion,
			RunID: scenarioRun.RunID, TerminalStatus: scenarioRun.Status,
			ArtifactRef: candidate.ContentHash,
		})
		artifacts = append(artifacts, teameval.ScenarioArtifact{
			ScenarioID: scenarioRun.Scenario.ID, TerminalStatus: scenarioRun.Status,
			Expected: scenarioRun.Scenario.Expected, Output: scenarioRun.Output,
		})
	}
	rubric := teameval.EvaluateRubric(round.Run.Contract, artifacts)
	report := teambuild.EvaluationReport{
		SchemaVersion: 1,
		TestedTeams: []teambuild.VersionedRef{
			{Kind: "team", Name: team.Name, Version: 1},
		},
		TestedWorkflows: []teambuild.VersionedRef{
			{Kind: "workflow", Name: candidate.WorkflowID, Version: candidate.WorkflowVersion},
		},
		ConfigChangeSummary: fmt.Sprintf(
			"round %d produced candidate %s for workflow %s",
			round.RoundNo, candidate.ContentHash, candidate.WorkflowID,
		),
		HardGateResults: mapGateResults(gates),
		ScenarioResults: scenarioResults,
		RubricScores:    rubric.Scores,
		SevereDefects:   rubric.SevereDefects,
		FailureSamples:  rubric.FailureSamples,
	}
	loaded, err := p.loadEvaluatedAssets(ctx, round, team)
	if err == nil {
		report.TestedAgents = loaded
	}
	// The report carries the candidate run's measured usage exactly as the
	// durable terminal record reports it, plus the usage-completeness
	// annotation: fanout legs and CLI nodes with missing receipts/dimensions
	// are unmeasured, so usage_complete=false names the unmeasured part instead
	// of fabricating zeros. The budget gate enforces this measured usage as
	// a lower bound. Measured tool calls follow the same terminal receipt.
	incompleteReasons := make([]string, 0)
	seenIncompleteReason := make(map[string]bool)
	usageDimensionsObserved := false
	usageHasTokens, usageHasCost := true, true
	seenUsageSource := make(map[string]bool)
	for _, scenarioRun := range runs {
		entry, ok := p.candidateRunUsage(ctx, round.WorkspaceID, scenarioRun.RunID)
		if !ok {
			continue
		}
		measured := entry.SelfExclusive
		report.InputTokens += int64(measured.InputTokens)
		report.OutputTokens += int64(measured.OutputTokens)
		report.CostUSD += measured.CostUSD
		report.ToolCalls += int64(measured.ToolCalls)
		if entry.UsageHasTokens != nil && entry.UsageHasCost != nil {
			usageDimensionsObserved = true
			usageHasTokens = usageHasTokens && *entry.UsageHasTokens
			usageHasCost = usageHasCost && *entry.UsageHasCost
		}
		for _, source := range entry.UsageSources {
			if !seenUsageSource[source] {
				seenUsageSource[source] = true
				report.UsageSources = append(report.UsageSources, source)
			}
		}
		if entry.UsageComplete != nil && !*entry.UsageComplete {
			reason := strings.TrimSpace(entry.UsageIncompleteReason)
			if reason != "" && !seenIncompleteReason[reason] {
				seenIncompleteReason[reason] = true
				incompleteReasons = append(incompleteReasons, reason)
			}
		}
	}
	report.Tokens = report.InputTokens + report.OutputTokens
	if usageDimensionsObserved {
		hasTokens, hasCost := usageHasTokens, usageHasCost
		report.UsageHasTokens = &hasTokens
		report.UsageHasCost = &hasCost
	}
	if len(incompleteReasons) > 0 {
		incomplete := false
		report.UsageComplete = &incomplete
		report.UsageIncompleteReason = strings.Join(incompleteReasons, ",")
	}
	// T15C gate-level comparison: every round report carries the
	// regressions/improvements versus the frozen baseline report (pass→fail =
	// regression, fail→pass = improvement). Create-mode runs have no baseline
	// and keep both lists empty.
	if baseline := p.readBaselineReport(ctx, round.WorkspaceID, round.BuildRunID); baseline != nil {
		report.Regressions, report.Improvements = compareGateResults(
			baseline.HardGateResults,
			report.HardGateResults,
		)
	}
	return report
}

func semanticEvaluationPackage(
	round RoundContext,
	runs []candidateScenarioRun,
) ([]byte, string, error) {
	scenarios := make([]semanticEvaluationScenarioV1, 0, len(runs))
	for _, run := range runs {
		scenarios = append(scenarios, semanticEvaluationScenarioV1{
			ScenarioID: run.Scenario.ID, InputVersion: run.Scenario.InputVersion,
			TerminalStatus: run.Status, Input: run.Scenario.Input,
			Expected: run.Scenario.Expected, Output: run.Output,
			StageOutputs: run.StageOutputs,
		})
	}
	payload, err := json.Marshal(semanticEvaluationPackageV1{
		SchemaVersion: 1, ContractHash: round.Run.ContractHash,
		Contract: round.Run.Contract, Scenarios: scenarios,
	})
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(payload)
	return payload, hex.EncodeToString(sum[:]), nil
}

type semanticRubricEvaluation struct {
	Scores        []teambuild.RubricScore
	SevereDefects []string
}

func decodeSemanticEvaluationOutput(
	output string,
	contract teambuild.EvaluationContract,
	runs []candidateScenarioRun,
) (semanticRubricEvaluation, error) {
	if err := rejectDuplicateJSONKeys(strings.NewReader(output)); err != nil {
		return semanticRubricEvaluation{}, fmt.Errorf("decode semantic evaluation output: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(output))
	decoder.DisallowUnknownFields()
	var judged teambuild.SemanticJudgeOutputV1
	if err := decoder.Decode(&judged); err != nil {
		return semanticRubricEvaluation{}, fmt.Errorf("decode semantic evaluation output: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return semanticRubricEvaluation{}, errors.New("decode semantic evaluation output: trailing content is forbidden")
	}
	if judged.SchemaVersion != 1 || len(judged.RubricScores) != len(contract.Rubric) {
		return semanticRubricEvaluation{}, errors.New("semantic evaluation output does not cover the frozen rubric")
	}
	validScenarioIDs := make(map[string]bool, len(runs))
	for _, run := range runs {
		validScenarioIDs[run.Scenario.ID] = true
	}
	scores := make([]teambuild.RubricScore, len(contract.Rubric))
	for index, dimension := range contract.Rubric {
		judgment := judged.RubricScores[index]
		if judgment.DimensionID != dimension.ID || judgment.Score < 0 ||
			judgment.Score > dimension.MaxScore ||
			!reasonCitesScenario(judgment.Reason, validScenarioIDs) {
			return semanticRubricEvaluation{}, fmt.Errorf("semantic evaluation score %d violates frozen dimension %q", index, dimension.ID)
		}
		scores[index] = teambuild.RubricScore{
			DimensionID: judgment.DimensionID, Score: judgment.Score,
			Reason: "semantic evaluator; " + strings.TrimSpace(judgment.Reason),
		}
	}
	if judged.SevereDefects == nil {
		return semanticRubricEvaluation{}, errors.New("semantic evaluation output must include severe_defects")
	}
	severe := make([]string, 0, len(*judged.SevereDefects))
	seenSevere := make(map[string]bool, len(*judged.SevereDefects))
	for _, defect := range *judged.SevereDefects {
		scenarioID := strings.TrimSpace(defect.ScenarioID)
		reason := strings.TrimSpace(defect.Reason)
		if !validScenarioIDs[scenarioID] || reason == "" || seenSevere[scenarioID] {
			return semanticRubricEvaluation{}, errors.New("semantic evaluation severe defect is not bound to one supplied scenario")
		}
		seenSevere[scenarioID] = true
		severe = append(severe, fmt.Sprintf("scenario %s: %s", scenarioID, reason))
	}
	return semanticRubricEvaluation{Scores: scores, SevereDefects: severe}, nil
}

func reasonCitesScenario(reason string, validScenarioIDs map[string]bool) bool {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return false
	}
	for scenarioID := range validScenarioIDs {
		if strings.Contains(reason, scenarioID) {
			return true
		}
	}
	return false
}

func (p *ProductionPhases) semanticRubricEvaluation(
	ctx context.Context,
	round RoundContext,
	runs []candidateScenarioRun,
) (semanticRubricEvaluation, error) {
	payload, evidenceHash, err := semanticEvaluationPackage(round, runs)
	if err != nil {
		return semanticRubricEvaluation{}, fmt.Errorf("build semantic evaluation package: %w", err)
	}
	executor := p.Deps.SemanticJudge
	if executor == nil {
		executor = buildSemanticJudgeExecutor{phases: p, round: round}
	}
	judged, err := executor.Execute(ctx, teambuild.SemanticJudgeRequest{
		WorkspaceID: round.WorkspaceID, BuildRunID: round.BuildRunID,
		RevisionNo: round.RoundNo, Run: round.Run,
		Payload: payload, EvidenceHash: evidenceHash,
	})
	if err != nil {
		return semanticRubricEvaluation{}, err
	}
	return decodeSemanticEvaluationOutput(judged.Output, round.Run.Contract, runs)
}

type buildSemanticJudgeExecutor struct {
	phases *ProductionPhases
	round  RoundContext
}

func (e buildSemanticJudgeExecutor) Execute(
	ctx context.Context,
	request teambuild.SemanticJudgeRequest,
) (teambuild.SemanticJudgeResult, error) {
	p := e.phases
	if p == nil || request.WorkspaceID != e.round.WorkspaceID || request.BuildRunID != e.round.BuildRunID ||
		request.RevisionNo != e.round.RoundNo || request.EvidenceHash == "" {
		return teambuild.SemanticJudgeResult{}, errors.New("semantic judge request identity is invalid")
	}
	attempt, err := p.Deps.Build.GetSemanticEvaluationAttempt(
		ctx, request.WorkspaceID, request.BuildRunID, request.RevisionNo,
		teambuild.SourceRoleSemanticJudge,
	)
	var semanticRunErr error
	if errors.Is(err, teambuild.ErrSemanticEvaluationAttemptNotFound) {
		result, runErr := p.runControlledAgent(
			ctx, e.round, semanticJudgeRecord(request.WorkspaceID), string(request.Payload),
			teambuild.SourceRoleSemanticJudge, request.EvidenceHash,
		)
		if runErr != nil {
			semanticRunErr = runErr
		} else if err := p.accountBuildRunUsage(
			ctx, e.round, teambuild.SourceRoleSemanticJudge, result.RunID,
		); err != nil {
			return teambuild.SemanticJudgeResult{}, fmt.Errorf("account semantic evaluator: %w", err)
		}
		attempt, err = p.Deps.Build.GetSemanticEvaluationAttempt(
			ctx, request.WorkspaceID, request.BuildRunID, request.RevisionNo,
			teambuild.SourceRoleSemanticJudge,
		)
		if err != nil {
			if semanticRunErr != nil {
				return teambuild.SemanticJudgeResult{}, fmt.Errorf("run semantic evaluator: %w", semanticRunErr)
			}
			return teambuild.SemanticJudgeResult{}, fmt.Errorf("load semantic evaluator attempt: %w", err)
		}
	} else if err != nil {
		return teambuild.SemanticJudgeResult{}, err
	}
	if attempt.EvidenceHash != request.EvidenceHash || attempt.SourceRole != teambuild.SourceRoleSemanticJudge {
		return teambuild.SemanticJudgeResult{}, errors.New("semantic evaluation attempt is not bound to current evidence")
	}
	if err := p.backfillUsageSource(ctx, e.round, teambuild.BuildUsageSource{
		WorkspaceID: attempt.WorkspaceID, BuildRunID: attempt.BuildRunID,
		RoundNo: attempt.RevisionNo, SourceKind: teambuild.UsageSourceKindBuildAgent,
		SourceRole: attempt.SourceRole, SourceRunID: attempt.SourceRunID,
	}); err != nil {
		return teambuild.SemanticJudgeResult{}, fmt.Errorf("reconcile semantic evaluator usage: %w", err)
	}
	output, err := semanticEvaluationAttemptOutput(attempt, semanticRunErr)
	if err != nil {
		return teambuild.SemanticJudgeResult{}, err
	}
	usage, err := p.Deps.Build.GetRoundBudgetUsage(ctx, request.WorkspaceID, request.BuildRunID, request.RevisionNo)
	if err != nil {
		return teambuild.SemanticJudgeResult{}, fmt.Errorf("load semantic evaluator usage ledger: %w", err)
	}
	return teambuild.SemanticJudgeResult{
		AttemptID: fmt.Sprintf("%s/%s/%d/%s", request.WorkspaceID, request.BuildRunID, request.RevisionNo, teambuild.SourceRoleSemanticJudge),
		RunID:     attempt.SourceRunID, Output: output, Usage: usage,
		UsageSource: teambuild.BuildUsageSource{
			WorkspaceID: attempt.WorkspaceID, BuildRunID: attempt.BuildRunID,
			RoundNo: attempt.RevisionNo, SourceKind: teambuild.UsageSourceKindBuildAgent,
			SourceRole: attempt.SourceRole, SourceRunID: attempt.SourceRunID,
		},
	}, nil
}

var _ teambuild.SemanticJudgeExecutor = buildSemanticJudgeExecutor{}

func blueprintPatchPlannerRecord(workspaceID string) *registry.AgentRecord {
	return &registry.AgentRecord{
		Name: teambuild.BlueprintPatchPlannerResourceName,
		ID: uuid.NewSHA1(uuid.NameSpaceURL, []byte(
			"weave/build/blueprint-patch-planner/v1\x00"+workspaceID,
		)).String(),
		WorkspaceID: workspaceID, Version: 1,
		DisplayName: "蓝图补丁规划器", Role: "worker",
		Visibility: registry.VisibilityPlatform,
		Spec: stdlib.AgentSpec{Identity: stdlib.IdentitySpec{
			Core: teambuild.BlueprintPatchPlannerPrompt,
		}},
		MemoryConfig:    &registry.MemoryConfig{Enabled: false},
		Compaction:      &registry.CompactionConfig{Enabled: false},
		GraphType:       "declarative",
		GraphDefinition: teambuild.BlueprintPatchPlannerDefinition(),
		Tags:            []string{"system", "build-controlled"},
	}
}

func semanticJudgeRecord(workspaceID string) *registry.AgentRecord {
	return &registry.AgentRecord{
		Name: teambuild.SemanticJudgeResourceName,
		ID: uuid.NewSHA1(uuid.NameSpaceURL, []byte(
			"weave/build/semantic-judge/v1\x00"+workspaceID,
		)).String(),
		WorkspaceID: workspaceID, Version: 1,
		DisplayName: "语义质量裁判", Role: "worker",
		Visibility: registry.VisibilityPlatform,
		Spec: stdlib.AgentSpec{Identity: stdlib.IdentitySpec{
			Core: teambuild.SemanticJudgePrompt,
		}},
		Compaction:      &registry.CompactionConfig{Enabled: false},
		GraphType:       "declarative",
		GraphDefinition: teambuild.SemanticJudgeDefinition(),
		Tags:            []string{"system", "build-controlled"},
	}
}

func constructionRoleRecord(workspaceID, sourceRole string) (*registry.AgentRecord, error) {
	var name, displayName, prompt string
	var definition *registry.GraphDefinition
	var outputSchema *json.RawMessage
	switch sourceRole {
	case teambuild.SourceRoleConfigEngineer:
		name, displayName, prompt = teambuild.ConfigEngineerResourceName, "配置工程师", teambuild.ConfigEngineerPrompt
		definition = teambuild.ConfigEngineerDefinition()
		schema := teambuild.ConfigEngineerOutputSchema()
		outputSchema = &schema
	case teambuild.SourceRoleGraphDesigner:
		name, displayName, prompt = teambuild.GraphDesignerResourceName, "图设计师", teambuild.GraphDesignerPrompt
		definition = teambuild.GraphDesignerDefinition()
	default:
		return nil, fmt.Errorf("unsupported construction source role %q", sourceRole)
	}
	return &registry.AgentRecord{
		Name: name,
		ID: uuid.NewSHA1(uuid.NameSpaceURL, []byte(
			"weave/build/construction-role/v1\x00"+workspaceID+"\x00"+sourceRole,
		)).String(),
		WorkspaceID: workspaceID, Version: 1,
		DisplayName: displayName, Role: "worker",
		Visibility:      registry.VisibilityPlatform,
		Spec:            stdlib.AgentSpec{Identity: stdlib.IdentitySpec{Core: prompt}},
		MemoryConfig:    &registry.MemoryConfig{Enabled: false},
		Compaction:      &registry.CompactionConfig{Enabled: false},
		OutputSchema:    outputSchema,
		GraphType:       "declarative",
		GraphDefinition: definition,
		Tags:            []string{"system", "build-controlled"},
	}, nil
}

func semanticEvaluationAttemptOutput(
	attempt teambuild.SemanticEvaluationAttempt,
	runErr error,
) (string, error) {
	if attempt.OutputText != nil && attempt.OutputHash != nil {
		return *attempt.OutputText, nil
	}
	if runErr != nil {
		return "", fmt.Errorf("semantic evaluator run failed before recording output: %w", runErr)
	}
	return "", errors.New("semantic evaluator reached terminal state without a recorded output")
}

func scenarioRunsSucceeded(runs []candidateScenarioRun) bool {
	if len(runs) == 0 {
		return false
	}
	for _, run := range runs {
		if run.Status != string(teamrun.StatusSucceeded) && run.Status != "success" {
			return false
		}
	}
	return true
}

// compareGateResults derives the gate-level regressions and improvements of
// one round against the baseline: a gate that passed in the baseline but
// fails now is a regression; a gate that failed in the baseline but passes
// now is an improvement. Order follows the current results slice (the stable
// AllGateCodes order from the evaluator).
func compareGateResults(
	baseline, current []teambuild.HardGateResult,
) (regressions, improvements []string) {
	baselineByID := make(map[string]bool, len(baseline))
	for _, result := range baseline {
		baselineByID[result.GateID] = result.Passed
	}
	for _, result := range current {
		baselinePassed, seen := baselineByID[result.GateID]
		if !seen {
			continue
		}
		if baselinePassed && !result.Passed {
			regressions = append(regressions, result.GateID)
		}
		if !baselinePassed && result.Passed {
			improvements = append(improvements, result.GateID)
		}
	}
	return regressions, improvements
}

// candidateRunUsage reads the candidate run's durable terminal record and
// returns the measured usage plus its completeness annotation. It reports
// absent when no valid terminal record is readable; the report then leaves
// the usage counters empty rather than fabricating any usage.
func (p *ProductionPhases) candidateRunUsage(
	ctx context.Context,
	workspaceID, runID string,
) (loomruntime.TerminalEntryV3, bool) {
	data, present, err := storeext.New(p.Deps.Pool).ReadValue(
		ctx, "audit:"+workspaceID, runID,
	)
	if err != nil || !present {
		return loomruntime.TerminalEntryV3{}, false
	}
	inspection := loomruntime.InspectTerminalRecord(true, data)
	if inspection.Classification != loomruntime.TerminalRecordValid ||
		inspection.Entry == nil || inspection.Err != nil {
		return loomruntime.TerminalEntryV3{}, false
	}
	return *inspection.Entry, true
}

// loadEvaluatedAssets reads the exact evaluated agent versions (lead +
// roster workers) for the report's asset-version traceability.
func (p *ProductionPhases) loadEvaluatedAssets(
	ctx context.Context,
	round RoundContext,
	team *org.Team,
) ([]teambuild.VersionedRef, error) {
	agents, err := p.Deps.Agents.List(ctx, round.WorkspaceID)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]registry.AgentRecord, len(agents))
	for _, record := range agents {
		byID[record.ID] = record
	}
	refs := make([]teambuild.VersionedRef, 0, 1+2)
	if lead, ok := byID[team.LeadAvatarID]; ok {
		refs = append(refs, teambuild.VersionedRef{
			Kind: "agent", Name: lead.Name, Version: lead.Version,
		})
	}
	roster, err := p.Deps.TeamWorkers.ListByTeam(ctx, round.WorkspaceID, team.ID)
	if err != nil {
		return nil, err
	}
	for _, worker := range roster {
		if record, ok := byID[worker.WorkerAgentID]; ok {
			refs = append(refs, teambuild.VersionedRef{
				Kind: "agent", Name: record.Name, Version: record.Version,
			})
		}
	}
	return refs, nil
}

// roundConclusion enforces the frozen evaluation contract: severe defects
// block, otherwise every hard gate and every rubric threshold must pass.
func roundConclusion(
	contract teambuild.EvaluationContract,
	gates []teameval.GateResult,
	scores []teambuild.RubricScore,
	severeDefects []string,
) (string, string) {
	if len(severeDefects) > 0 {
		return teambuild.ConclusionBlocked, "severe_defect"
	}
	for _, gate := range gates {
		if gate.Status == teameval.StatusFail {
			return teambuild.ConclusionRevise, gate.Code
		}
	}
	if failed := teameval.FailedRubricDimensions(contract, scores); len(failed) > 0 {
		return teambuild.ConclusionRevise, "rubric:" + failed[0]
	}
	return teambuild.ConclusionPass, ""
}

func mapGateResults(gates []teameval.GateResult) []teambuild.HardGateResult {
	results := make([]teambuild.HardGateResult, 0, len(gates))
	for _, gate := range gates {
		results = append(results, gate.ToHardGateResult())
	}
	return results
}

// infraEvaluation reports an evaluation-infrastructure failure: the report is
// persisted as evidence, the controller stops the run as blocked without
// consuming the improvement counter.
func (p *ProductionPhases) infraEvaluation(round RoundContext, detail, errorCode string) RoundEvaluation {
	report := teambuild.EvaluationReport{
		SchemaVersion:   1,
		Conclusion:      teambuild.ConclusionBlocked,
		InfraErrors:     []string{detail},
		HardGateResults: []teambuild.HardGateResult{},
		ScenarioResults: []teambuild.ScenarioResult{},
	}
	return RoundEvaluation{
		Conclusion:      teambuild.ConclusionBlocked,
		InfraError:      true,
		FailureCategory: "evaluation_infrastructure_error",
		Diagnosis: teameval.DiagnoseOutcome(teameval.OutcomeInput{
			TerminalStatus: "failed", OriginalErrorCode: errorCode, Detail: detail,
		}),
		Report: report,
	}
}

// failedTeamReport is the business-side report when the build phase did not
// create the target team; the team-shape gate fails with the evidence text.
func (p *ProductionPhases) failedTeamReport(round RoundContext, detail string) teambuild.EvaluationReport {
	return teambuild.EvaluationReport{
		SchemaVersion:       1,
		Conclusion:          teambuild.ConclusionRevise,
		ConfigChangeSummary: detail,
		HardGateResults: []teambuild.HardGateResult{
			{GateID: teameval.GateTeamShape, Passed: false, EvidenceRef: detail},
		},
		ScenarioResults: []teambuild.ScenarioResult{},
	}
}

func candidateRunPayload(scenario evaluationScenario) json.RawMessage {
	type payload struct {
		Brief            string `json:"brief"`
		Mode             string `json:"mode"`
		ScenarioID       string `json:"scenario_id,omitempty"`
		InputVersion     string `json:"input_version,omitempty"`
		Input            string `json:"input,omitempty"`
		Expected         string `json:"expected,omitempty"`
		PerturbationRule string `json:"perturbation_rule,omitempty"`
		Guidance         string `json:"guidance"`
	}
	out := payload{
		Brief: "team forge candidate scenario test", Mode: "candidate_scenario_test",
		ScenarioID: scenario.ID, InputVersion: scenario.InputVersion,
		Input: scenario.Input, Expected: scenario.Expected,
		PerturbationRule: scenario.PerturbationRule,
		Guidance:         "Run this as a short platform validation. Do not ask follow-up questions. Use the provided scenario input, honor the perturbation when present, make reasonable assumptions for missing details, and return a concise final deliverable plus PASS/REVISE evidence.",
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return json.RawMessage(`{"brief":"team forge candidate scenario test","mode":"candidate_scenario_test","guidance":"Run this as a short platform validation. Do not ask follow-up questions."}`)
	}
	return encoded
}

func shortID(parts ...string) string {
	digest := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return hex.EncodeToString(digest[:6])
}
