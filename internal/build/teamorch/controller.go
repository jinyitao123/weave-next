// Package teamorch implements the in-process round controller that drives a
// TeamBuildRun through the three-round build/evaluate loop (§9 of the team
// forge plan).
//
// Design decision (reviewer ruling, recorded in the T10 ticket): the first
// version drives rounds from a Go controller instead of the meta team's own
// TeamWorkflow. A TeamWorkflow path would freeze artifacts and its serial
// machine does not support handoff; the three-round loop is inherently
// stateful and would require a new graph node type, which is over-engineering
// for the skeleton. A future ticket may rework the loop as a TeamWorkflow.
package teamorch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/build/teameval"
	"github.com/jinyitao123/weave/internal/build/teamforge"
)

// DefaultActor is the principal recorded in the build run transition ledger
// for transitions driven by the round controller.
const DefaultActor = "round-controller"

// Phases is the per-round work injected into the controller. The production
// wiring (architect agent run, GateEvaluator, candidate test runs, publish)
// lands in T11; this ticket only provides the skeleton and persistence.
type Phases interface {
	// Build runs the build phase for one round. Errors abort Run and leave
	// the run in round_running so a later invocation resumes the round from
	// the same round number.
	Build(ctx context.Context, round RoundContext) error
	// Baseline runs the optimize-mode baseline evaluation before round 1's
	// Build: it freezes the pre-task published content into a candidate,
	// test-runs it, evaluates every hard gate, and persists the baseline
	// report. The controller invokes it exactly once per optimize run inside
	// round 1 (round_running); the implementation must be idempotent under
	// crash recovery (a persisted report short-circuits a re-run). Errors
	// abort Run the same way Build errors do.
	Baseline(ctx context.Context, round RoundContext) error
	// Evaluate runs the evaluation phase for one round and returns its
	// outcome. Errors abort Run the same way Build errors do; an evaluation
	// infrastructure failure is reported through RoundEvaluation.InfraError
	// instead of an error so it is persisted and stopped distinctly from a
	// business failure.
	Evaluate(ctx context.Context, round RoundContext) (RoundEvaluation, error)
}

type templatePublicationFinalizer interface {
	FinalizeTemplatePublication(
		ctx context.Context,
		workspaceID, buildRunID, teamID, actor string,
	) (teambuild.TeamBuildRun, error)
}

// RoundContext carries the frozen control record facts one round can use.
type RoundContext struct {
	WorkspaceID      string
	BuildRunID       string
	RoundNo          int
	CandidateAttempt int
	// FrozenWorkflowID/Version are set by compiler_v1 after workflow_compile
	// materializes an exact draft. Legacy rounds leave them empty and resolve
	// the latest draft through the existing path.
	FrozenWorkflowID      string
	FrozenWorkflowVersion int
	Run                   teambuild.TeamBuildRun
}

// RoundEvaluation is the outcome of one round's evaluation.
type RoundEvaluation struct {
	// CandidateRef names the frozen candidate the build phase produced. The
	// controller persists it together with the round result; T11 wires it to
	// the architect run's frozen candidate.
	CandidateRef string
	Gates        []teambuild.HardGateResult
	// Conclusion is the round conclusion: pass, revise, or blocked.
	Conclusion string
	// FailureCategory is the primary failure class (F1–F14 or a custom
	// label). Two consecutive non-pass rounds with the same category trigger
	// the early stop; the controller persists it inside the report JSON.
	FailureCategory string
	// InfraError marks an evaluation-infrastructure failure. The controller
	// persists the report but records no round and stops the run as blocked
	// with reason evaluation_infrastructure_error, without consuming the
	// "no improvement" counter.
	InfraError bool
	// Diagnosis is the platform-owned typed failure route. Legacy executions
	// may leave it empty and continue to use InfraError/Conclusion.
	Diagnosis teameval.TypedDiagnosis
	// ReportRef is the expected content hash of Report. When non-empty it
	// must equal Report.Hash(); the controller always persists the recomputed
	// hash.
	ReportRef string
	// Report is the immutable EvaluationReport persisted for the round. The
	// controller owns RoundNo, Conclusion, and FailureCategory inside the
	// report and overrides them from the RoundEvaluation fields.
	Report teambuild.EvaluationReport
}

// BlueprintRevisionPlanner is the Task 7 production seam. It may plan only
// a report-bound BlueprintPatch; it does not grant storage or graph writes.
type BlueprintRevisionPlanner interface {
	PlanBlueprintPatch(
		ctx context.Context,
		round RoundContext,
		report teambuild.EvaluationReport,
		diagnosis teameval.TypedDiagnosis,
	) (teambuild.BlueprintPatchV1, error)
}

// Result describes how Run finished.
type Result struct {
	WorkspaceID string `json:"workspace_id"`
	BuildRunID  string `json:"build_run_id"`
	// Status is the run status when Run returned: publishing (round passed)
	// or blocked. It may also be a terminal status already reached before
	// Run was invoked.
	Status string `json:"status"`
	// StopReason is the termination reason recorded in the transition ledger
	// when Status == blocked.
	StopReason string `json:"stop_reason,omitempty"`
	// Rounds is the number of rounds recorded in the round ledger.
	Rounds int `json:"rounds"`
}

// Controller drives one TeamBuildRun through its rounds (§9). Every state
// change goes through teambuild.Store's CAS state machine; a concurrent
// writer that moves the run out from under the controller surfaces as an
// error from Run.
//
// Run is synchronous and idempotent under crash recovery: starting with the
// run in round_running resumes the interrupted round (or finishes the stop
// that was already recorded) based on the round ledger and report rows. The
// controller deliberately does not spawn background goroutines; callers may
// host it in their own worker loop following the controllable Start/Stop
// shape of internal/teamrun/workers.go.
type Controller struct {
	Store  *teambuild.Store
	Phases Phases
	// Compiler executes persisted compiler-v1 operation DAGs. It is selected
	// only by TeamBuildRun.ExecutionStrategy; legacy runs never call it.
	Compiler compilerDriver
	// Actor is the principal recorded in the transition ledger; defaults to
	// DefaultActor when empty.
	Actor string
	// Clock supplies "now" for expiry checks; nil uses the real clock.
	Clock teambuild.Clock
	// RevisionPlanner is required only when compiler_v1 reaches a genuine,
	// evidence-complete business-quality failure. Nil fails closed.
	RevisionPlanner BlueprintRevisionPlanner
}

// NewController creates a round controller. clock may be nil (real clock).
func NewController(store *teambuild.Store, phases Phases, clock teambuild.Clock) *Controller {
	controller := &Controller{Store: store, Phases: phases, Actor: DefaultActor, Clock: clock}
	if handler, ok := phases.(OperationHandler); ok && store != nil {
		controller.Compiler = NewCompilerExecutor(store, handler)
	}
	return controller
}

// Run drives workspaceID/buildRunID through the round loop until the run
// reaches publishing (pass), blocked, or an error. See the package comment
// for the crash recovery contract.
func (c *Controller) Run(ctx context.Context, workspaceID, buildRunID string) (Result, error) {
	if c == nil || c.Store == nil || c.Phases == nil {
		return Result{}, errors.New("round controller: store and phases are required")
	}
	actor := c.Actor
	if actor == "" {
		actor = DefaultActor
	}

	run, err := c.Store.GetBuildRun(ctx, workspaceID, buildRunID)
	if err != nil {
		return Result{}, fmt.Errorf("round controller: load build run: %w", err)
	}
	switch run.EffectiveExecutionStrategy() {
	case teambuild.ExecutionStrategyCompilerV1:
		return c.runCompilerV1(ctx, run, actor)
	case teambuild.ExecutionStrategyTemplateInstantiate:
		return c.runTemplateInstantiate(ctx, run, actor)
	}
	switch run.Status {
	case teambuild.StatusPassed, teambuild.StatusBlocked, teambuild.StatusCancelled, teambuild.StatusPublishing:
		return Result{WorkspaceID: workspaceID, BuildRunID: buildRunID, Status: run.Status}, nil
	case teambuild.StatusPlanning:
		return Result{}, errors.New("round controller: build run is not authorized")
	}

	rounds, err := c.Store.ListRounds(ctx, workspaceID, buildRunID)
	if err != nil {
		return Result{}, fmt.Errorf("round controller: list rounds: %w", err)
	}

	// Crash recovery. A round_running run can be one of three interrupted
	// states, resolved from the round ledger and report rows:
	//  1. a round was started but never produced a report — resume that round;
	//  2. a report row exists for the next round without a round ledger row —
	//     the process died after persisting an infra-error report before
	//     blocking the run, so finish the stop;
	//  3. the last recorded round's post-round transition was not applied —
	//     the process died between RecordRoundResult and TransitionStatus, so
	//     replay the transition and continue.
	if run.Status == teambuild.StatusRoundRunning {
		if pendingReport, err := c.Store.GetRoundReport(ctx, workspaceID, buildRunID, len(rounds)+1); err == nil {
			reason := "evaluation_infrastructure_error"
			if run.EffectiveExecutionStrategy() == teambuild.ExecutionStrategyCompilerV1 &&
				strings.TrimSpace(pendingReport.Report.FailureCategory) != "" {
				reason = pendingReport.Report.FailureCategory
			}
			if _, err := c.Store.TransitionStatus(ctx, workspaceID, buildRunID,
				teambuild.StatusRoundRunning, teambuild.StatusBlocked, actor,
				reason); err != nil {
				return Result{}, fmt.Errorf("round controller: finalize infra-error stop: %w", err)
			}
			return Result{WorkspaceID: workspaceID, BuildRunID: buildRunID,
				Status: teambuild.StatusBlocked, StopReason: reason,
				Rounds: len(rounds)}, nil
		} else if !errors.Is(err, teambuild.ErrRoundReportNotFound) {
			return Result{}, fmt.Errorf("round controller: check interrupted round report: %w", err)
		}
		if len(rounds) > 0 {
			last := rounds[len(rounds)-1]
			switch last.Conclusion {
			case teambuild.ConclusionPass:
				if _, err := c.Store.TransitionStatus(ctx, workspaceID, buildRunID,
					teambuild.StatusRoundRunning, teambuild.StatusPublishing, actor,
					"crash recovery: finalizing recorded pass"); err != nil {
					return Result{}, fmt.Errorf("round controller: finalize recorded pass: %w", err)
				}
				return Result{WorkspaceID: workspaceID, BuildRunID: buildRunID,
					Status: teambuild.StatusPublishing, Rounds: len(rounds)}, nil
			case teambuild.ConclusionBlocked:
				if _, err := c.Store.TransitionStatus(ctx, workspaceID, buildRunID,
					teambuild.StatusRoundRunning, teambuild.StatusBlocked, actor,
					"crash recovery: finalizing recorded block"); err != nil {
					return Result{}, fmt.Errorf("round controller: finalize recorded block: %w", err)
				}
				return Result{WorkspaceID: workspaceID, BuildRunID: buildRunID,
					Status:     teambuild.StatusBlocked,
					StopReason: "crash recovery: finalizing recorded block",
					Rounds:     len(rounds)}, nil
			case teambuild.ConclusionRevise:
				if last.RoundNo >= run.Contract.MaxIterations {
					if _, err := c.Store.TransitionStatus(ctx, workspaceID, buildRunID,
						teambuild.StatusRoundRunning, teambuild.StatusBlocked, actor,
						"max iterations reached without pass"); err != nil {
						return Result{}, fmt.Errorf("round controller: terminate after recorded final revise: %w", err)
					}
					return Result{WorkspaceID: workspaceID, BuildRunID: buildRunID,
						Status:     teambuild.StatusBlocked,
						StopReason: "max iterations reached without pass",
						Rounds:     len(rounds)}, nil
				}
				// Replay the missing revise transition so the next round
				// starts from a clean authorized state. If the transition had
				// already been applied before a later crash, this replay is
				// still safe: the next round restarts from scratch.
				if _, err := c.Store.TransitionStatus(ctx, workspaceID, buildRunID,
					teambuild.StatusRoundRunning, teambuild.StatusAuthorized, actor,
					"crash recovery: resuming after recorded revise"); err != nil {
					return Result{}, fmt.Errorf("round controller: resume after recorded revise: %w", err)
				}
			}
		}
		run, err = c.Store.GetBuildRun(ctx, workspaceID, buildRunID)
		if err != nil {
			return Result{}, fmt.Errorf("round controller: reload build run: %w", err)
		}
	}

	// The loop below starts from authorized. If the contract's iteration
	// budget is already consumed (all rounds recorded), terminate.
	if run.Status == teambuild.StatusAuthorized && len(rounds) >= run.Contract.MaxIterations {
		reason := "max iterations reached without pass"
		if _, err := c.Store.TransitionStatus(ctx, workspaceID, buildRunID,
			teambuild.StatusAuthorized, teambuild.StatusBlocked, actor, reason); err != nil {
			return Result{}, fmt.Errorf("round controller: terminate after max iterations: %w", err)
		}
		return Result{WorkspaceID: workspaceID, BuildRunID: buildRunID,
			Status: teambuild.StatusBlocked, StopReason: reason, Rounds: len(rounds)}, nil
	}

	for roundNo := len(rounds) + 1; roundNo <= run.Contract.MaxIterations; roundNo++ {
		if run.Status == teambuild.StatusAuthorized {
			// expires_at bounds admission of new work. Once a round is admitted,
			// long-running execution remains valid and resumable.
			if !c.now().Before(run.ExpiresAt) {
				reason := fmt.Sprintf("build run authorization expired at %s", run.ExpiresAt.UTC().Format(time.RFC3339))
				if _, err := c.Store.TransitionStatus(ctx, workspaceID, buildRunID,
					teambuild.StatusAuthorized, teambuild.StatusBlocked, actor, reason); err != nil {
					return Result{}, fmt.Errorf("round controller: stop expired authorization: %w", err)
				}
				return Result{WorkspaceID: workspaceID, BuildRunID: buildRunID,
					Status: teambuild.StatusBlocked, StopReason: reason, Rounds: len(rounds)}, nil
			}
			run, err = c.Store.TransitionStatus(ctx, workspaceID, buildRunID,
				teambuild.StatusAuthorized, teambuild.StatusRoundRunning, actor,
				fmt.Sprintf("round %d started", roundNo))
			if err != nil {
				return Result{}, fmt.Errorf("round controller: start round %d: %w", roundNo, err)
			}
		}
		if ctx.Err() != nil {
			return Result{}, fmt.Errorf("round controller: execution interrupted: %w", ctx.Err())
		}

		roundCtx := RoundContext{
			WorkspaceID: workspaceID,
			BuildRunID:  buildRunID,
			RoundNo:     roundNo,
			Run:         run,
		}
		// Budget gate, before Build: a strictly exceeded dimension blocks
		// without calling Build. Exact equality is not an over-budget verdict.
		//
		// T15C baseline evaluation: for an optimize run, round 1 first
		// evaluates the frozen baseline (pre-task published content) and
		// persists the baseline report. It runs inside round_running R1 as an
		// independent step before the pre-Build budget gate so its usage is
		// already in round 1's ledger when Build's gate is evaluated. The
		// state machine gains no new status: the run stays round_running
		// across the baseline step exactly like a crashed Build resume.
		if roundNo == 1 && run.Mode == teambuild.ModeOptimize {
			if err := c.Phases.Baseline(ctx, roundCtx); err != nil {
				if ctx.Err() != nil {
					return Result{}, fmt.Errorf("round controller: baseline interrupted: %w", ctx.Err())
				}
				return Result{}, fmt.Errorf("round controller: baseline phase round %d: %w", roundNo, err)
			}
		}
		if res, blocked, err := c.budgetGate(ctx, workspaceID, buildRunID, actor,
			roundNo, len(rounds), run); err != nil {
			return Result{}, err
		} else if blocked {
			return res, nil
		}
		if err := c.Phases.Build(ctx, roundCtx); err != nil {
			if ctx.Err() != nil {
				return Result{}, fmt.Errorf("round controller: build interrupted: %w", ctx.Err())
			}
			return Result{}, fmt.Errorf("round controller: build phase round %d: %w", roundNo, err)
		}
		// Budget gate, after Build: the build phase's charges are in the
		// ledger. A strictly exceeded limit stops the run.
		if res, blocked, err := c.budgetGate(ctx, workspaceID, buildRunID, actor,
			roundNo, len(rounds), run); err != nil {
			return Result{}, err
		} else if blocked {
			return res, nil
		}
		eval, err := c.Phases.Evaluate(ctx, roundCtx)
		if err != nil {
			if ctx.Err() != nil {
				return Result{}, fmt.Errorf("round controller: evaluation interrupted: %w", ctx.Err())
			}
			return Result{}, fmt.Errorf("round controller: evaluate phase round %d: %w", roundNo, err)
		}
		compilerV1 := run.EffectiveExecutionStrategy() == teambuild.ExecutionStrategyCompilerV1
		if compilerV1 {
			if eval.Diagnosis.Class == "" {
				if eval.Conclusion == teambuild.ConclusionPass {
					eval.Diagnosis = teameval.TypedDiagnosis{
						Class: teameval.FailureClassPass, RevisionAction: teameval.RevisionActionNone,
					}
				} else {
					eval.Diagnosis = teameval.TypedDiagnosis{
						Class:          teameval.FailureClassRuntimeInfrastructure,
						RevisionAction: teameval.RevisionActionBlockPlatformDiagnosis,
						FailureDetail:  "compiler_v1 evaluation omitted typed diagnosis",
					}
				}
			}
			if eval.Diagnosis.Class != teameval.FailureClassPass &&
				eval.Diagnosis.Class != teameval.FailureClassBusinessQuality {
				return c.stopCompilerDiagnosis(ctx, workspaceID, buildRunID, actor,
					roundNo, len(rounds), eval)
			}
		}

		if eval.InfraError {
			report, err := c.infraReport(eval, roundNo)
			if err != nil {
				return Result{}, err
			}
			if _, err := c.Store.SaveRoundReport(ctx, workspaceID, buildRunID, report); err != nil {
				return Result{}, fmt.Errorf("round controller: persist infra-error report: %w", err)
			}
			if _, err := c.Store.TransitionStatus(ctx, workspaceID, buildRunID,
				teambuild.StatusRoundRunning, teambuild.StatusBlocked, actor,
				"evaluation_infrastructure_error"); err != nil {
				return Result{}, fmt.Errorf("round controller: stop after infra error: %w", err)
			}
			return Result{WorkspaceID: workspaceID, BuildRunID: buildRunID,
				Status: teambuild.StatusBlocked, StopReason: "evaluation_infrastructure_error",
				Rounds: len(rounds)}, nil
		}

		// Budget gate, after Evaluate: read the round + total ledger before
		// the report/round rows and the pass/revise transition. Exceeded
		// dimensions block even a passing round; exact equality does not.
		if res, blocked, err := c.budgetGate(ctx, workspaceID, buildRunID, actor,
			roundNo, len(rounds), run); err != nil {
			return Result{}, err
		} else if blocked {
			return res, nil
		}

		report, err := c.roundReport(eval, roundNo)
		if err != nil {
			return Result{}, err
		}
		if compilerV1 && eval.Diagnosis.Class == teameval.FailureClassBusinessQuality {
			if eval.Diagnosis.RevisionAction != teameval.RevisionActionRequestBlueprintPatch {
				eval.Diagnosis.FailureDetail = "business-quality diagnosis did not request BlueprintPatch"
				return c.stopCompilerDiagnosis(ctx, workspaceID, buildRunID, actor,
					roundNo, len(rounds), eval)
			}
			if c.RevisionPlanner == nil {
				eval.Diagnosis.FailureDetail = "blueprint_patch_planner_unavailable"
				return c.stopCompilerDiagnosis(ctx, workspaceID, buildRunID, actor,
					roundNo, len(rounds), eval)
			}
			patch, planErr := c.RevisionPlanner.PlanBlueprintPatch(ctx, roundCtx, report, eval.Diagnosis)
			if planErr != nil {
				if errors.Is(planErr, ErrBlueprintPatchPlannerBudgetExhausted) {
					return c.stopCompilerDiagnosisWithReason(ctx, workspaceID, buildRunID, actor,
						roundNo, len(rounds), eval, "budget_exhausted")
				}
				eval.Diagnosis.FailureDetail = "blueprint_patch_planner_failed: " + planErr.Error()
				return c.stopCompilerDiagnosis(ctx, workspaceID, buildRunID, actor,
					roundNo, len(rounds), eval)
			}
			reportHash, hashErr := report.Hash()
			if hashErr != nil {
				return Result{}, fmt.Errorf("round controller: hash compiler_v1 report: %w", hashErr)
			}
			if patch.SourceReportHash != reportHash || patch.FailureClass != teambuild.BlueprintPatchFailureBusinessQuality {
				eval.Diagnosis.FailureDetail = "blueprint_patch_report_or_failure_class_mismatch"
				return c.stopCompilerDiagnosis(ctx, workspaceID, buildRunID, actor,
					roundNo, len(rounds), eval)
			}
			if _, hashErr := patch.BlueprintPatchHash(); hashErr != nil {
				eval.Diagnosis.FailureDetail = "blueprint_patch_invalid: " + hashErr.Error()
				return c.stopCompilerDiagnosis(ctx, workspaceID, buildRunID, actor,
					roundNo, len(rounds), eval)
			}
			// Task 6 proves and invokes the typed planning seam only. Task 7
			// supplies durable patch persistence/application; until then the
			// controller must not record a revise round for a discarded patch.
			eval.Diagnosis.FailureDetail = "blueprint_patch_execution_unavailable"
			return c.stopCompilerDiagnosis(ctx, workspaceID, buildRunID, actor,
				roundNo, len(rounds), eval)
		}

		// Early stop: the same failure category for two consecutive rounds
		// without a pass means no improvement.
		stopReason := ""
		if eval.Conclusion == teambuild.ConclusionRevise &&
			eval.FailureCategory != "" && len(rounds) > 0 {
			prev, err := c.Store.GetRoundReport(ctx, workspaceID, buildRunID, len(rounds))
			if err != nil {
				return Result{}, fmt.Errorf("round controller: load previous report: %w", err)
			}
			if prev.Report.FailureCategory == eval.FailureCategory {
				stopReason = fmt.Sprintf(
					"no improvement for two consecutive rounds: failure category %s",
					eval.FailureCategory,
				)
			}
		}

		round, err := c.recordRound(ctx, workspaceID, buildRunID, eval, report)
		if err != nil {
			return Result{}, fmt.Errorf("round controller: record round %d: %w", roundNo, err)
		}
		rounds = append(rounds, round)

		switch {
		case stopReason != "":
			if _, err := c.Store.TransitionStatus(ctx, workspaceID, buildRunID,
				teambuild.StatusRoundRunning, teambuild.StatusBlocked, actor, stopReason); err != nil {
				return Result{}, fmt.Errorf("round controller: early stop: %w", err)
			}
			return Result{WorkspaceID: workspaceID, BuildRunID: buildRunID,
				Status: teambuild.StatusBlocked, StopReason: stopReason, Rounds: roundNo}, nil
		case eval.Conclusion == teambuild.ConclusionPass:
			if _, err := c.Store.TransitionStatus(ctx, workspaceID, buildRunID,
				teambuild.StatusRoundRunning, teambuild.StatusPublishing, actor,
				fmt.Sprintf("round %d passed", roundNo)); err != nil {
				return Result{}, fmt.Errorf("round controller: enter publishing: %w", err)
			}
			return Result{WorkspaceID: workspaceID, BuildRunID: buildRunID,
				Status: teambuild.StatusPublishing, Rounds: roundNo}, nil
		case eval.Conclusion == teambuild.ConclusionBlocked:
			reason := fmt.Sprintf("round %d conclusion blocked", roundNo)
			if eval.FailureCategory != "" {
				reason += ": " + eval.FailureCategory
			}
			if _, err := c.Store.TransitionStatus(ctx, workspaceID, buildRunID,
				teambuild.StatusRoundRunning, teambuild.StatusBlocked, actor, reason); err != nil {
				return Result{}, fmt.Errorf("round controller: stop after blocked conclusion: %w", err)
			}
			return Result{WorkspaceID: workspaceID, BuildRunID: buildRunID,
				Status: teambuild.StatusBlocked, StopReason: reason, Rounds: roundNo}, nil
		default: // revise
			if roundNo < run.Contract.MaxIterations {
				if _, err := c.Store.TransitionStatus(ctx, workspaceID, buildRunID,
					teambuild.StatusRoundRunning, teambuild.StatusAuthorized, actor,
					fmt.Sprintf("round %d revise, preparing round %d", roundNo, roundNo+1)); err != nil {
					return Result{}, fmt.Errorf("round controller: prepare next round: %w", err)
				}
				run, err = c.Store.GetBuildRun(ctx, workspaceID, buildRunID)
				if err != nil {
					return Result{}, fmt.Errorf("round controller: reload build run: %w", err)
				}
			} else {
				reason := "max iterations reached without pass"
				if _, err := c.Store.TransitionStatus(ctx, workspaceID, buildRunID,
					teambuild.StatusRoundRunning, teambuild.StatusBlocked, actor, reason); err != nil {
					return Result{}, fmt.Errorf("round controller: terminate after max iterations: %w", err)
				}
				return Result{WorkspaceID: workspaceID, BuildRunID: buildRunID,
					Status: teambuild.StatusBlocked, StopReason: reason, Rounds: roundNo}, nil
			}
		}
	}
	return Result{WorkspaceID: workspaceID, BuildRunID: buildRunID,
		Status: run.Status, Rounds: len(rounds)}, nil
}

// runTemplateInstantiate executes the receipt-gated materialization DAG and
// then delegates one atomic publication/activation/finalization transaction
// to the production phases. It never runs candidate scenarios or a judge.
func (c *Controller) runTemplateInstantiate(
	ctx context.Context,
	run teambuild.TeamBuildRun,
	actor string,
) (Result, error) {
	workspaceID, buildRunID := run.WorkspaceID, run.BuildRunID
	switch run.Status {
	case teambuild.StatusPassed, teambuild.StatusBlocked, teambuild.StatusCancelled:
		return Result{WorkspaceID: workspaceID, BuildRunID: buildRunID, Status: run.Status}, nil
	case teambuild.StatusPlanning:
		return Result{}, errors.New("round controller: build run is not authorized")
	case teambuild.StatusPublishing:
		return Result{}, errors.New("round controller: template_instantiate must never enter publishing")
	}
	if c.Compiler == nil {
		return Result{}, errors.New("round controller: template_instantiate executor unavailable")
	}
	if run.Status == teambuild.StatusAuthorized {
		if !c.now().Before(run.ExpiresAt) {
			reason := fmt.Sprintf("build run authorization expired at %s", run.ExpiresAt.UTC().Format(time.RFC3339))
			if _, err := c.Store.TransitionStatus(ctx, workspaceID, buildRunID,
				teambuild.StatusAuthorized, teambuild.StatusBlocked, actor, reason); err != nil {
				return Result{}, fmt.Errorf("round controller: stop expired template authorization: %w", err)
			}
			return Result{WorkspaceID: workspaceID, BuildRunID: buildRunID, Status: teambuild.StatusBlocked, StopReason: reason}, nil
		}
		var err error
		run, err = c.Store.TransitionStatus(ctx, workspaceID, buildRunID,
			teambuild.StatusAuthorized, teambuild.StatusRoundRunning, actor,
			"template_instantiate asset materialization started")
		if err != nil {
			return Result{}, fmt.Errorf("round controller: start template_instantiate execution: %w", err)
		}
	}
	execution, err := c.Compiler.Execute(ctx, workspaceID, buildRunID)
	if err != nil {
		return Result{}, fmt.Errorf("round controller: execute template_instantiate revision: %w", err)
	}
	if execution.Cancelled {
		current, loadErr := c.Store.GetBuildRun(context.WithoutCancel(ctx), workspaceID, buildRunID)
		if loadErr != nil {
			return Result{}, fmt.Errorf("round controller: reload cancelled template run: %w", loadErr)
		}
		if current.Status == teambuild.StatusRoundRunning {
			current, loadErr = c.Store.TransitionStatus(context.WithoutCancel(ctx), workspaceID, buildRunID,
				teambuild.StatusRoundRunning, teambuild.StatusCancelled, actor, "template_operation_cancelled")
			if loadErr != nil {
				return Result{}, fmt.Errorf("round controller: cancel template run: %w", loadErr)
			}
		}
		return Result{WorkspaceID: workspaceID, BuildRunID: buildRunID, Status: current.Status, StopReason: "template_operation_cancelled"}, nil
	}
	if execution.Failure != nil {
		failure := execution.Failure
		if failure.Class == teameval.FailureClassRuntimeInfrastructure && failure.Retryable {
			current, loadErr := c.Store.GetBuildRun(ctx, workspaceID, buildRunID)
			if loadErr != nil {
				return Result{}, loadErr
			}
			return Result{WorkspaceID: workspaceID, BuildRunID: buildRunID, Status: current.Status, StopReason: failure.Code}, nil
		}
		current, transitionErr := c.Store.TransitionStatus(ctx, workspaceID, buildRunID,
			teambuild.StatusRoundRunning, teambuild.StatusBlocked, actor, failure.Code)
		if transitionErr != nil {
			return Result{}, fmt.Errorf("round controller: block template_instantiate failure: %w", transitionErr)
		}
		return Result{WorkspaceID: workspaceID, BuildRunID: buildRunID, Status: current.Status, StopReason: failure.Code}, nil
	}
	if !execution.Complete {
		current, loadErr := c.Store.GetBuildRun(ctx, workspaceID, buildRunID)
		if loadErr != nil {
			return Result{}, loadErr
		}
		return Result{WorkspaceID: workspaceID, BuildRunID: buildRunID, Status: current.Status}, nil
	}
	steps, err := c.Store.ListOperationSteps(ctx, workspaceID, buildRunID, execution.RevisionNo)
	if err != nil {
		return Result{}, fmt.Errorf("round controller: list completed template operations: %w", err)
	}
	teamID, err := templateInstantiatedTeamID(steps)
	if err != nil {
		return Result{}, fmt.Errorf("round controller: resolve instantiated team: %w", err)
	}
	finalizer, ok := c.Phases.(templatePublicationFinalizer)
	if !ok {
		return Result{}, errors.New("round controller: template publication finalizer unavailable")
	}
	finalized, err := finalizer.FinalizeTemplatePublication(ctx, workspaceID, buildRunID, teamID, actor)
	if err != nil {
		return Result{}, fmt.Errorf("round controller: finalize template_instantiate: %w", err)
	}
	return Result{WorkspaceID: workspaceID, BuildRunID: buildRunID, Status: finalized.Status}, nil
}

func templateInstantiatedTeamID(steps []teambuild.OperationStep) (string, error) {
	for _, step := range steps {
		if step.OperationType != string(teamforge.OperationTeamCreate) ||
			step.Status != teambuild.OperationStatusSucceeded {
			continue
		}
		var evidence struct {
			ToolResult struct {
				Team struct {
					ID string `json:"id"`
				} `json:"team"`
			} `json:"tool_result"`
		}
		if err := json.Unmarshal(step.EvidenceJSON, &evidence); err != nil {
			return "", err
		}
		if teamID := strings.TrimSpace(evidence.ToolResult.Team.ID); teamID != "" {
			return teamID, nil
		}
		return "", errors.New("team_create evidence has no team id")
	}
	return "", errors.New("completed template revision has no successful team_create operation")
}

// runCompilerV1 is intentionally separate from the legacy round loop. Its
// RoundNo is the immutable Blueprint revision number, and it never invokes
// Baseline, Build, or the fixed config/graph meta-agent sequence.
func (c *Controller) runCompilerV1(ctx context.Context, run teambuild.TeamBuildRun, actor string) (Result, error) {
	workspaceID, buildRunID := run.WorkspaceID, run.BuildRunID
	switch run.Status {
	case teambuild.StatusBlocked, teambuild.StatusCancelled:
		return Result{WorkspaceID: workspaceID, BuildRunID: buildRunID, Status: run.Status}, nil
	case teambuild.StatusPlanning:
		return Result{}, errors.New("round controller: build run is not authorized")
	}
	if c.Compiler == nil {
		return Result{}, errors.New("round controller: compiler_v1 executor unavailable")
	}
	rounds, err := c.Store.ListRounds(ctx, workspaceID, buildRunID)
	if err != nil {
		return Result{}, fmt.Errorf("round controller: list compiler_v1 rounds: %w", err)
	}
	if run.Status == teambuild.StatusAuthorized {
		// Expiry is an admission boundary only. Once admitted, operation leases
		// renew for liveness without imposing a business-duration deadline.
		if !c.now().Before(run.ExpiresAt) {
			reason := fmt.Sprintf("build run authorization expired at %s", run.ExpiresAt.UTC().Format(time.RFC3339))
			if _, err := c.Store.TransitionStatus(ctx, workspaceID, buildRunID,
				teambuild.StatusAuthorized, teambuild.StatusBlocked, actor, reason); err != nil {
				return Result{}, fmt.Errorf("round controller: stop expired compiler authorization: %w", err)
			}
			return Result{WorkspaceID: workspaceID, BuildRunID: buildRunID,
				Status: teambuild.StatusBlocked, StopReason: reason}, nil
		}
		var err error
		run, err = c.Store.TransitionStatus(ctx, workspaceID, buildRunID,
			teambuild.StatusAuthorized, teambuild.StatusRoundRunning, actor,
			"compiler_v1 operation DAG started")
		if err != nil {
			return Result{}, fmt.Errorf("round controller: start compiler_v1 execution: %w", err)
		}
	}
	for {
		// Candidate success is persisted on the operation step before Execute
		// yields. Reconcile it before claiming more work so a crash in that small
		// window cannot let publish bypass the round/report ledger.
		if run.Status == teambuild.StatusRoundRunning {
			passing, recoverErr := c.persistedCompilerPassingEvaluation(ctx, workspaceID, buildRunID, rounds)
			if recoverErr != nil {
				return Result{}, recoverErr
			}
			if passing != nil {
				rounds, recoverErr = c.recordCompilerPassingRound(ctx, workspaceID, buildRunID, *passing, rounds)
				if recoverErr != nil {
					return Result{}, recoverErr
				}
			}
		}
		execution, err := c.Compiler.Execute(ctx, workspaceID, buildRunID)
		if err != nil {
			return Result{}, fmt.Errorf("round controller: execute compiler_v1 revision: %w", err)
		}
		if execution.Cancelled {
			current, loadErr := c.Store.GetBuildRun(context.WithoutCancel(ctx), workspaceID, buildRunID)
			if loadErr != nil {
				return Result{}, fmt.Errorf("round controller: reload cancelled compiler_v1 run: %w", loadErr)
			}
			if current.Status == teambuild.StatusRoundRunning {
				current, loadErr = c.Store.TransitionStatus(context.WithoutCancel(ctx), workspaceID, buildRunID,
					teambuild.StatusRoundRunning, teambuild.StatusCancelled, actor, "compiler_operation_cancelled")
				if loadErr != nil {
					return Result{}, fmt.Errorf("round controller: cancel compiler_v1 run: %w", loadErr)
				}
			}
			return Result{WorkspaceID: workspaceID, BuildRunID: buildRunID,
				Status: current.Status, StopReason: "compiler_operation_cancelled", Rounds: execution.RevisionNo}, nil
		}
		if execution.Failure != nil {
			failure := execution.Failure
			if failure.Class == teameval.FailureClassBudgetExhausted {
				current, loadErr := c.Store.GetBuildRun(ctx, workspaceID, buildRunID)
				if loadErr != nil {
					return Result{}, loadErr
				}
				if current.Status == teambuild.StatusBlocked {
					return Result{WorkspaceID: workspaceID, BuildRunID: buildRunID,
						Status: current.Status, StopReason: failure.Code,
						Rounds: execution.RevisionNo}, nil
				}
			}
			if failure.Class == teameval.FailureClassRuntimeInfrastructure && failure.Retryable {
				current, loadErr := c.Store.GetBuildRun(ctx, workspaceID, buildRunID)
				if loadErr != nil {
					return Result{}, loadErr
				}
				return Result{WorkspaceID: workspaceID, BuildRunID: buildRunID,
					Status: current.Status, StopReason: failure.Code, Rounds: execution.RevisionNo}, nil
			}
			evaluation := RoundEvaluation{
				Conclusion:      teambuild.ConclusionBlocked,
				FailureCategory: failure.Code,
				Report:          teambuild.EvaluationReport{SchemaVersion: 1},
				Diagnosis: teameval.TypedDiagnosis{
					Class: failure.Class, OriginalErrorCode: failure.Code,
					RevisionAction: teameval.RevisionActionBlockPlatformDiagnosis,
				},
			}
			if execution.Evaluation != nil {
				evaluation = *execution.Evaluation
			}
			// A retryable candidate infrastructure diagnosis deliberately asks the
			// controller to keep the same revision active. Once the operation step
			// has exhausted that retry and is terminal, carrying the original
			// resume action forward would requeue the execution job forever while
			// the BuildRun remains round_running. The operation result is the
			// authority for whether another same-revision attempt still exists.
			if failure.Class == teameval.FailureClassRuntimeInfrastructure &&
				!failure.Retryable &&
				evaluation.Diagnosis.RevisionAction == teameval.RevisionActionResumeSameRevision {
				evaluation.Diagnosis.RevisionAction = teameval.RevisionActionBlockPlatformDiagnosis
			}
			if failure.Class == teameval.FailureClassBusinessQuality {
				result, appended, handleErr := c.handleCompilerBusinessQuality(ctx, run, actor, execution.RevisionNo, evaluation)
				if handleErr != nil || !appended {
					return result, handleErr
				}
				// The async execution job owns this controller invocation. Drive
				// the newly appended latest revision now so no successful job can
				// strand a round_running run with pending work.
				continue
			}
			return c.stopCompilerDiagnosis(ctx, workspaceID, buildRunID, actor,
				execution.RevisionNo, execution.RevisionNo-1, evaluation)
		}
		if execution.Evaluation != nil {
			rounds, err = c.recordCompilerPassingRound(
				ctx, workspaceID, buildRunID, *execution.Evaluation, rounds,
			)
			if err != nil {
				return Result{}, err
			}
			// candidate_run has completed, and publish remains pending. Keep the
			// same controller invocation alive to finish the operation DAG.
			continue
		}
		current, err := c.Store.GetBuildRun(ctx, workspaceID, buildRunID)
		if err != nil {
			return Result{}, fmt.Errorf("round controller: reload compiler_v1 result: %w", err)
		}
		roundCount := execution.RevisionNo
		if len(rounds) > 0 {
			roundCount = len(rounds)
		}
		return Result{WorkspaceID: workspaceID, BuildRunID: buildRunID,
			Status: current.Status, Rounds: roundCount}, nil
	}
}

// persistedCompilerPassingEvaluation recovers the immutable evaluation from a
// succeeded candidate_run step. The last recorded pass is accepted only when
// it names the same candidate; otherwise the caller must append the missing
// round before compiler_v1 can claim publish.
func (c *Controller) persistedCompilerPassingEvaluation(
	ctx context.Context,
	workspaceID, buildRunID string,
	rounds []teambuild.Round,
) (*RoundEvaluation, error) {
	revision, err := c.Store.GetLatestBlueprintRevision(ctx, workspaceID, buildRunID)
	if err != nil {
		return nil, fmt.Errorf("round controller: load compiler_v1 revision for round recovery: %w", err)
	}
	steps, err := c.Store.ListOperationSteps(ctx, workspaceID, buildRunID, revision.RevisionNo)
	if err != nil {
		return nil, fmt.Errorf("round controller: list compiler_v1 steps for round recovery: %w", err)
	}
	for _, step := range steps {
		if step.OperationType != string(teamforge.OperationCandidateRun) ||
			step.Status != teambuild.OperationStatusSucceeded {
			continue
		}
		var eval RoundEvaluation
		if err := json.Unmarshal(step.EvidenceJSON, &eval); err != nil {
			return nil, fmt.Errorf("round controller: decode compiler_v1 candidate evaluation: %w", err)
		}
		if len(rounds) > 0 {
			last := rounds[len(rounds)-1]
			if last.Conclusion == teambuild.ConclusionPass && last.CandidateRef == eval.CandidateRef {
				report, reportErr := c.roundReport(eval, last.RoundNo)
				if reportErr != nil {
					return nil, reportErr
				}
				reportHash, hashErr := report.Hash()
				if hashErr != nil {
					return nil, fmt.Errorf("round controller: hash recovered compiler_v1 report: %w", hashErr)
				}
				if reportHash != last.ReportRef {
					return nil, errors.New("round controller: recovered compiler_v1 pass conflicts with recorded round")
				}
				return nil, nil
			}
		}
		return &eval, nil
	}
	return nil, nil
}

func (c *Controller) recordCompilerPassingRound(
	ctx context.Context,
	workspaceID, buildRunID string,
	eval RoundEvaluation,
	rounds []teambuild.Round,
) ([]teambuild.Round, error) {
	if eval.Conclusion != teambuild.ConclusionPass || eval.Diagnosis.Class != teameval.FailureClassPass {
		return nil, fmt.Errorf(
			"round controller: compiler_v1 successful candidate has non-pass evaluation %q/%q",
			eval.Conclusion, eval.Diagnosis.Class,
		)
	}
	if len(rounds) > 0 {
		last := rounds[len(rounds)-1]
		if last.Conclusion == teambuild.ConclusionPass && last.CandidateRef == eval.CandidateRef {
			return rounds, nil
		}
	}
	report, err := c.roundReport(eval, len(rounds)+1)
	if err != nil {
		return nil, err
	}
	round, err := c.recordRound(ctx, workspaceID, buildRunID, eval, report)
	if err != nil {
		return nil, fmt.Errorf("round controller: record compiler_v1 passing round: %w", err)
	}
	return append(rounds, round), nil
}

func (c *Controller) handleCompilerBusinessQuality(
	ctx context.Context, run teambuild.TeamBuildRun, actor string, revisionNo int, eval RoundEvaluation,
) (Result, bool, error) {
	workspaceID, buildRunID := run.WorkspaceID, run.BuildRunID
	if run.EvaluationOnly {
		report, err := c.roundReport(eval, revisionNo)
		if err != nil {
			return Result{}, false, err
		}
		if _, err := c.Store.SaveRoundReport(ctx, workspaceID, buildRunID, report); err != nil &&
			!errors.Is(err, teambuild.ErrRoundReportExists) {
			return Result{}, false, fmt.Errorf("round controller: persist evaluation-only business report: %w", err)
		}
		result, err := c.blockCompilerWithPersistedReport(ctx, workspaceID, buildRunID, actor,
			"evaluation_business_quality_failed", revisionNo)
		return result, false, err
	}
	if eval.Diagnosis.RevisionAction != teameval.RevisionActionRequestBlueprintPatch {
		eval.Diagnosis.FailureDetail = "business-quality diagnosis did not request BlueprintPatch"
		result, err := c.stopCompilerDiagnosis(ctx, workspaceID, buildRunID, actor, revisionNo, revisionNo-1, eval)
		return result, false, err
	}
	if c.RevisionPlanner == nil {
		eval.Diagnosis.FailureDetail = "blueprint_patch_planner_unavailable"
		result, err := c.stopCompilerDiagnosis(ctx, workspaceID, buildRunID, actor, revisionNo, revisionNo-1, eval)
		return result, false, err
	}
	report, err := c.roundReport(eval, revisionNo)
	if err != nil {
		return Result{}, false, err
	}
	if _, err := c.Store.SaveRoundReport(ctx, workspaceID, buildRunID, report); err != nil {
		if !errors.Is(err, teambuild.ErrRoundReportExists) {
			return Result{}, false, fmt.Errorf("round controller: persist compiler_v1 business report: %w", err)
		}
		existing, loadErr := c.Store.GetRoundReport(ctx, workspaceID, buildRunID, revisionNo)
		reportHash, hashErr := report.Hash()
		if loadErr != nil || hashErr != nil || existing.ReportHash != reportHash {
			return Result{}, false, fmt.Errorf("round controller: existing compiler_v1 business report conflicts")
		}
	}
	latest, err := c.Store.GetLatestBlueprintRevision(ctx, workspaceID, buildRunID)
	if err != nil {
		return Result{}, false, fmt.Errorf("round controller: load compiler_v1 blueprint for revision: %w", err)
	}
	var currentBlueprint teambuild.TeamBlueprintV1
	if err := json.Unmarshal(latest.BlueprintJSON, &currentBlueprint); err != nil {
		return Result{}, false, fmt.Errorf("round controller: decode compiler_v1 blueprint: %w", err)
	}
	if latest.RevisionNo != revisionNo || revisionNo >= currentBlueprint.RevisionPolicy.MaxRevisions {
		result, stopErr := c.blockCompilerWithPersistedReport(ctx, workspaceID, buildRunID, actor,
			"blueprint_revision_limit_reached", revisionNo)
		return result, false, stopErr
	}
	patch, err := c.RevisionPlanner.PlanBlueprintPatch(ctx, RoundContext{
		WorkspaceID: workspaceID, BuildRunID: buildRunID, RoundNo: revisionNo, Run: run,
	}, report, eval.Diagnosis)
	if err != nil {
		if errors.Is(err, ErrBlueprintPatchPlannerBudgetExhausted) {
			result, stopErr := c.blockCompilerWithPersistedReport(ctx, workspaceID, buildRunID, actor,
				"budget_exhausted", revisionNo)
			return result, false, stopErr
		}
		result, stopErr := c.blockCompilerWithPersistedReport(ctx, workspaceID, buildRunID, actor,
			"blueprint_patch_planner_failed", revisionNo)
		return result, false, stopErr
	}
	reportHash, err := report.Hash()
	if err != nil {
		return Result{}, false, fmt.Errorf("round controller: hash compiler_v1 report: %w", err)
	}
	if patch.SourceReportHash != reportHash || patch.FailureClass != teambuild.BlueprintPatchFailureBusinessQuality {
		result, stopErr := c.blockCompilerWithPersistedReport(ctx, workspaceID, buildRunID, actor,
			"blueprint_patch_report_or_failure_class_mismatch", revisionNo)
		return result, false, stopErr
	}
	if _, err := patch.BlueprintPatchHash(); err != nil {
		result, stopErr := c.blockCompilerWithPersistedReport(ctx, workspaceID, buildRunID, actor,
			"blueprint_patch_invalid", revisionNo)
		return result, false, stopErr
	}
	if err := validateCompilerPatchExecutable(currentBlueprint, patch); err != nil {
		result, stopErr := c.blockCompilerWithPersistedReport(ctx, workspaceID, buildRunID, actor,
			"blueprint_patch_path_not_executable", revisionNo)
		return result, false, stopErr
	}
	revisedBlueprint, err := teambuild.ApplyBlueprintPatchV1(currentBlueprint, patch)
	if err != nil {
		result, stopErr := c.blockCompilerWithPersistedReport(ctx, workspaceID, buildRunID, actor,
			"blueprint_patch_invalid", revisionNo)
		return result, false, stopErr
	}
	baseline, err := teamforge.AuthorizedBaselineV1(run, revisedBlueprint)
	if err != nil {
		return Result{}, false, fmt.Errorf("round controller: project authorization-bound compiler baseline: %w", err)
	}
	changeSet, err := teamforge.CompileChangeSetV1(baseline, revisedBlueprint)
	if err != nil {
		return Result{}, false, fmt.Errorf("round controller: compile revised ChangeSet: %w", err)
	}
	blueprintJSON, err := json.Marshal(revisedBlueprint)
	if err != nil {
		return Result{}, false, err
	}
	blueprintHash, err := revisedBlueprint.BlueprintHash()
	if err != nil {
		return Result{}, false, err
	}
	changeSetJSON, err := changeSet.CanonicalBytes()
	if err != nil {
		return Result{}, false, err
	}
	changeSetHash, err := changeSet.CanonicalHash()
	if err != nil {
		return Result{}, false, err
	}
	patchJSON, err := json.Marshal(patch)
	if err != nil {
		return Result{}, false, err
	}
	patchHash, err := patch.BlueprintPatchHash()
	if err != nil {
		return Result{}, false, err
	}
	appended, err := c.Store.AppendCompilerRevisionFromPatch(ctx, workspaceID, buildRunID, teambuild.CompilerRevisionAppend{
		Bundle: teambuild.CompilerAuthorizationBundle{
			RevisionNo: latest.RevisionNo + 1, BlueprintJSON: blueprintJSON, BlueprintHash: blueprintHash,
			ChangeSetJSON: changeSetJSON, ChangeSetHash: changeSetHash,
			BaselineHash: latest.BaselineHash, BaselineCapturedAt: latest.BaselineCapturedAt,
			TemplateGapAuthorizationJSON: latest.TemplateGapAuthorizationJSON,
			TemplateGapAuthorizationHash: latest.TemplateGapAuthorizationHash,
			EvaluationContractHash:       latest.EvaluationContractHash,
		},
		SourceReportHash: reportHash, BlueprintPatchJSON: patchJSON, BlueprintPatchHash: patchHash,
	})
	if err != nil {
		return Result{}, false, fmt.Errorf("round controller: append compiler_v1 blueprint revision: %w", err)
	}
	return Result{WorkspaceID: workspaceID, BuildRunID: buildRunID,
		Status: teambuild.StatusRoundRunning, Rounds: appended.RevisionNo}, true, nil
}

func (c *Controller) stopCompilerDiagnosisWithReason(
	ctx context.Context,
	workspaceID, buildRunID, actor string,
	roundNo, roundsLen int,
	eval RoundEvaluation,
	reason string,
) (Result, error) {
	eval.Diagnosis.OriginalErrorCode = reason
	eval.Diagnosis.Class = teameval.FailureClassBudgetExhausted
	eval.Diagnosis.RevisionAction = teameval.RevisionActionBlockPlatformDiagnosis
	eval.Diagnosis.FailureDetail = reason
	eval.FailureCategory = reason
	return c.stopCompilerDiagnosis(ctx, workspaceID, buildRunID, actor, roundNo, roundsLen, eval)
}

func validateCompilerPatchExecutable(blueprint teambuild.TeamBlueprintV1, patch teambuild.BlueprintPatchV1) error {
	for _, change := range patch.Changes {
		parts := strings.Split(change.Path, "/")
		if change.Path == "/purpose" || change.Path == "/lead_ref" {
			continue
		}
		if len(parts) == 4 && parts[1] == "members" {
			switch parts[3] {
			case "responsibilities", "capabilities", "model_ref", "execution_policy":
				continue
			}
		}
		if len(parts) >= 3 && parts[1] == "workflow" && parts[2] == "template_parameters" {
			if blueprint.Workflow.Mode != teambuild.BlueprintWorkflowTemplate {
				return fmt.Errorf("patch path %s has no deterministic revision executor for %s workflow", change.Path, blueprint.Workflow.Mode)
			}
			if len(parts) == 5 && parts[3] == "result_requirements" {
				continue
			}
			if len(parts) == 4 {
				switch parts[3] {
				case "lead_instruction", "primary_ref", "reviewer_ref", "parallel_worker_refs", "finalizer_ref", "max_iterations":
					continue
				}
			}
			return fmt.Errorf("patch path %s has no deterministic revision executor", change.Path)
		}
		return fmt.Errorf("patch path %s has no deterministic revision executor", change.Path)
	}
	return nil
}

func (c *Controller) blockCompilerWithPersistedReport(
	ctx context.Context, workspaceID, buildRunID, actor, reason string, revisionNo int,
) (Result, error) {
	if _, err := c.Store.TransitionStatus(ctx, workspaceID, buildRunID,
		teambuild.StatusRoundRunning, teambuild.StatusBlocked, actor, reason); err != nil {
		return Result{}, fmt.Errorf("round controller: block compiler_v1 revision: %w", err)
	}
	return Result{WorkspaceID: workspaceID, BuildRunID: buildRunID,
		Status: teambuild.StatusBlocked, StopReason: reason, Rounds: revisionNo}, nil
}

// stopCompilerDiagnosis handles every compiler-v1 non-business outcome
// without recording a revise round. Retryable infrastructure keeps the same
// round/revision resumable; all deterministic platform/governance failures
// persist a blocked diagnostic report with the original code.
func (c *Controller) stopCompilerDiagnosis(
	ctx context.Context,
	workspaceID, buildRunID, actor string,
	roundNo, roundsLen int,
	eval RoundEvaluation,
) (Result, error) {
	diagnosis := eval.Diagnosis
	reason := strings.TrimSpace(diagnosis.OriginalErrorCode)
	if reason == "" {
		reason = string(diagnosis.Class)
	}
	if diagnosis.Class == teameval.FailureClassCancelled {
		if _, err := c.Store.TransitionStatus(ctx, workspaceID, buildRunID,
			teambuild.StatusRoundRunning, teambuild.StatusCancelled, actor, reason); err != nil {
			return Result{}, fmt.Errorf("round controller: cancel compiler_v1 run: %w", err)
		}
		return Result{WorkspaceID: workspaceID, BuildRunID: buildRunID,
			Status: teambuild.StatusCancelled, StopReason: reason, Rounds: roundsLen}, nil
	}
	if diagnosis.Class == teameval.FailureClassRuntimeInfrastructure &&
		diagnosis.RevisionAction == teameval.RevisionActionResumeSameRevision {
		return Result{WorkspaceID: workspaceID, BuildRunID: buildRunID,
			Status: teambuild.StatusRoundRunning, StopReason: reason, Rounds: roundsLen}, nil
	}
	eval.FailureCategory = reason
	if diagnosis.FailureDetail != "" {
		eval.Report.InfraErrors = append(eval.Report.InfraErrors, diagnosis.FailureDetail)
	}
	report, err := c.infraReport(eval, roundNo)
	if err != nil {
		return Result{}, err
	}
	if _, err := c.Store.SaveRoundReport(ctx, workspaceID, buildRunID, report); err != nil {
		return Result{}, fmt.Errorf("round controller: persist compiler_v1 diagnostic report: %w", err)
	}
	if _, err := c.Store.TransitionStatus(ctx, workspaceID, buildRunID,
		teambuild.StatusRoundRunning, teambuild.StatusBlocked, actor, reason); err != nil {
		return Result{}, fmt.Errorf("round controller: block compiler_v1 diagnosis: %w", err)
	}
	return Result{WorkspaceID: workspaceID, BuildRunID: buildRunID,
		Status: teambuild.StatusBlocked, StopReason: reason, Rounds: roundsLen}, nil
}

// budgetGate is one round-controller budget checkpoint. It reads the round
// and total ledger (the controller never writes it; usage sources do) and
// stops the run only when ExceededDims is non-empty.
func (c *Controller) budgetGate(
	ctx context.Context,
	workspaceID, buildRunID, actor string,
	roundNo, roundsLen int,
	run teambuild.TeamBuildRun,
) (Result, bool, error) {
	decision, err := c.Store.EvaluateBudget(
		ctx, workspaceID, buildRunID, roundNo, run.RoundBudget, run.TotalBudget,
	)
	if err != nil {
		return Result{}, false, fmt.Errorf("round controller: evaluate budget: %w", err)
	}
	block := func(reason string) (Result, bool, error) {
		if _, err := c.Store.TransitionStatus(ctx, workspaceID, buildRunID,
			teambuild.StatusRoundRunning, teambuild.StatusBlocked, actor, reason); err != nil {
			return Result{}, false, fmt.Errorf("round controller: stop after budget gate: %w", err)
		}
		return Result{
			WorkspaceID: workspaceID,
			BuildRunID:  buildRunID,
			Status:      teambuild.StatusBlocked,
			StopReason:  reason,
			Rounds:      roundsLen,
		}, true, nil
	}
	if len(decision.ExceededDims) > 0 {
		return block(string(teameval.FailureClassBudgetExhausted))
	}
	return Result{}, false, nil
}

// roundReport finalizes the report owned by the controller: the round number,
// conclusion, and failure category come from RoundEvaluation, and the
// content hash is verified against the phase-provided ReportRef when set.
func (c *Controller) roundReport(eval RoundEvaluation, roundNo int) (teambuild.EvaluationReport, error) {
	report := eval.Report
	report.RoundNo = roundNo
	report.FailureCategory = eval.FailureCategory
	report.Conclusion = eval.Conclusion
	switch eval.Conclusion {
	case teambuild.ConclusionPass, teambuild.ConclusionRevise, teambuild.ConclusionBlocked:
	default:
		return teambuild.EvaluationReport{}, fmt.Errorf(
			"round controller: invalid round conclusion %q", eval.Conclusion,
		)
	}
	reportHash, err := report.Hash()
	if err != nil {
		return teambuild.EvaluationReport{}, fmt.Errorf("round controller: hash report: %w", err)
	}
	if eval.ReportRef != "" && eval.ReportRef != reportHash {
		return teambuild.EvaluationReport{}, fmt.Errorf(
			"round controller: report ref %s does not match report hash %s",
			eval.ReportRef, reportHash,
		)
	}
	return report, nil
}

// infraReport finalizes the report for an infrastructure-error stop. The
// report is persisted as evidence, but no round ledger row is appended, so
// the stop is never counted as a business failure and never consumes the
// "no improvement" counter.
func (c *Controller) infraReport(eval RoundEvaluation, roundNo int) (teambuild.EvaluationReport, error) {
	report := eval.Report
	report.RoundNo = roundNo
	report.FailureCategory = eval.FailureCategory
	// The evaluation could not complete; the report conclusion is blocked
	// only because the immutable conclusion domain has no infra value. The
	// run-level reason evaluation_infrastructure_error distinguishes it from
	// a business failure.
	report.Conclusion = teambuild.ConclusionBlocked
	reportHash, err := report.Hash()
	if err != nil {
		return teambuild.EvaluationReport{}, fmt.Errorf("round controller: hash infra report: %w", err)
	}
	if eval.ReportRef != "" && eval.ReportRef != reportHash {
		return teambuild.EvaluationReport{}, fmt.Errorf(
			"round controller: infra report ref %s does not match report hash %s",
			eval.ReportRef, reportHash,
		)
	}
	return report, nil
}

// recordRound persists the evaluation report and the round ledger row in one
// transaction, so the ledger's report_ref always resolves to a report row.
func (c *Controller) recordRound(
	ctx context.Context,
	workspaceID, buildRunID string,
	eval RoundEvaluation,
	report teambuild.EvaluationReport,
) (teambuild.Round, error) {
	if strings.TrimSpace(eval.CandidateRef) == "" {
		return teambuild.Round{}, errors.New("round controller: candidate_ref is required")
	}
	reportHash, err := report.Hash()
	if err != nil {
		return teambuild.Round{}, fmt.Errorf("round controller: hash report: %w", err)
	}
	result := teambuild.RoundResult{
		CandidateRef: eval.CandidateRef,
		ReportRef:    reportHash,
		Conclusion:   eval.Conclusion,
	}
	tx, err := c.Store.BeginTx(ctx)
	if err != nil {
		return teambuild.Round{}, fmt.Errorf("begin round persistence: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := c.Store.SaveRoundReportTx(ctx, tx, workspaceID, buildRunID, report); err != nil {
		return teambuild.Round{}, fmt.Errorf("save round report: %w", err)
	}
	round, err := c.Store.RecordRoundResultTx(ctx, tx, workspaceID, buildRunID, result)
	if err != nil {
		return teambuild.Round{}, fmt.Errorf("record round result: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return teambuild.Round{}, fmt.Errorf("commit round persistence: %w", err)
	}
	return round, nil
}

func (c *Controller) now() time.Time {
	if c.Clock != nil {
		return c.Clock.Now()
	}
	return time.Now()
}
