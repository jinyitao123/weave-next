package teamorch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/teamrun"
	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/build/teameval"
	"github.com/jinyitao123/weave/internal/build/teamforge"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

var ErrCompilerExecutionPending = errors.New("compiler execution has no locally claimable ready operation")

// CompilerOperationStore is the durable operation-DAG surface used by the
// compiler executor. *teambuild.Store is the production implementation.
type CompilerOperationStore interface {
	GetBuildRun(context.Context, string, string) (teambuild.TeamBuildRun, error)
	GetLatestBlueprintRevision(context.Context, string, string) (teambuild.BlueprintRevision, error)
	ListOperationSteps(context.Context, string, string, int) ([]teambuild.OperationStep, error)
	ClaimReadyOperationStep(context.Context, string, string, int, string, time.Duration) (teambuild.OperationStep, error)
	RenewOperationStepLease(context.Context, string, string, int, string, string, int64, time.Duration) (teambuild.OperationStep, error)
	SucceedOperationStep(context.Context, string, string, int, string, string, int64, string, json.RawMessage) (teambuild.OperationStep, error)
	SkipOperationStep(context.Context, string, string, int, string, string, int64, string, json.RawMessage) (teambuild.OperationStep, error)
	FailOperationStep(context.Context, string, string, int, string, string, int64, string, string, json.RawMessage) (teambuild.OperationStep, error)
	RetryOperationStep(context.Context, string, string, int, string, string, int64, string, string, json.RawMessage) (teambuild.OperationStep, error)
}

// OperationContext contains only persisted, hash-bound compiler inputs. A
// handler cannot ask a meta-agent to rediscover the operation.
type OperationContext struct {
	WorkspaceID string
	BuildRunID  string
	Run         teambuild.TeamBuildRun
	Revision    teambuild.BlueprintRevision
	Blueprint   teambuild.TeamBlueprintV1
	ChangeSet   teamforge.ChangeSetV1
	Operation   teamforge.ChangeOperationV1
	Step        teambuild.OperationStep
	Steps       []teambuild.OperationStep
}

// OperationHandler performs one closed ChangeSet operation. ProductionPhases
// implements this interface; tests use a deterministic fake.
type OperationHandler interface {
	HandleOperation(context.Context, OperationContext) (OperationResult, error)
}

type OperationResult struct {
	Skip       bool
	OutputHash string
	Evidence   json.RawMessage
	Evaluation *RoundEvaluation
}

// OperationError is the only handler failure accepted by the executor.
// Retryable failures preserve an immutable attempt and return the step to
// pending on the same Blueprint revision.
type OperationError struct {
	Class      teameval.FailureClass
	Code       string
	Retryable  bool
	Evidence   json.RawMessage
	Evaluation *RoundEvaluation
	Cause      error
}

func (e *OperationError) Error() string {
	if e == nil {
		return ""
	}
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", e.Code, e.Cause)
	}
	return e.Code
}

func (e *OperationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

type CompilerExecutionResult struct {
	RevisionNo int
	Complete   bool
	Pending    bool
	Cancelled  bool
	Failure    *OperationError
	Evaluation *RoundEvaluation
}

type compilerDriver interface {
	Execute(context.Context, string, string) (CompilerExecutionResult, error)
}

// CompilerExecutor executes one persisted revision. Lease duration is only a
// liveness fence: it is renewed while the handler runs and never limits total
// business execution time.
type CompilerExecutor struct {
	Store         CompilerOperationStore
	Handler       OperationHandler
	WorkerID      string
	LeaseDuration time.Duration
	PublishStep   func(context.Context, teambuild.OperationStep, string)
}

func NewCompilerExecutor(store CompilerOperationStore, handler OperationHandler) *CompilerExecutor {
	return &CompilerExecutor{
		Store: store, Handler: handler,
		WorkerID: "compiler-executor", LeaseDuration: 30 * time.Second,
	}
}

func (e *CompilerExecutor) SetOperationStepPublisher(publisher func(context.Context, teambuild.OperationStep, string)) {
	if e == nil {
		return
	}
	e.PublishStep = publisher
}

func (e *CompilerExecutor) Execute(ctx context.Context, workspaceID, buildRunID string) (CompilerExecutionResult, error) {
	if e == nil || e.Store == nil || e.Handler == nil {
		return CompilerExecutionResult{}, errors.New("compiler executor: store and operation handler are required")
	}
	workerID := strings.TrimSpace(e.WorkerID)
	if workerID == "" {
		workerID = "compiler-executor"
	}
	lease := e.LeaseDuration
	if lease <= 0 {
		lease = 30 * time.Second
	}
	revision, err := e.Store.GetLatestBlueprintRevision(ctx, workspaceID, buildRunID)
	if err != nil {
		return CompilerExecutionResult{}, fmt.Errorf("compiler executor: load latest revision: %w", err)
	}
	var blueprint teambuild.TeamBlueprintV1
	if err := json.Unmarshal(revision.BlueprintJSON, &blueprint); err != nil {
		return CompilerExecutionResult{}, fmt.Errorf("compiler executor: decode persisted blueprint: %w", err)
	}
	var changeSet teamforge.ChangeSetV1
	if err := json.Unmarshal(revision.ChangeSetJSON, &changeSet); err != nil {
		return CompilerExecutionResult{}, fmt.Errorf("compiler executor: decode persisted change set: %w", err)
	}
	operations := make(map[string]teamforge.ChangeOperationV1, len(changeSet.Operations))
	for _, operation := range changeSet.Operations {
		operations[operation.OperationID] = operation
	}

	for {
		if err := ctx.Err(); err != nil {
			return CompilerExecutionResult{RevisionNo: revision.RevisionNo, Cancelled: true}, nil
		}
		run, err := e.Store.GetBuildRun(ctx, workspaceID, buildRunID)
		if err != nil {
			return CompilerExecutionResult{}, fmt.Errorf("compiler executor: load build run: %w", err)
		}
		if run.Status == teambuild.StatusCancelled {
			return CompilerExecutionResult{RevisionNo: revision.RevisionNo, Cancelled: true}, nil
		}
		steps, err := e.Store.ListOperationSteps(ctx, workspaceID, buildRunID, revision.RevisionNo)
		if err != nil {
			return CompilerExecutionResult{}, fmt.Errorf("compiler executor: list operation steps: %w", err)
		}
		step, err := e.Store.ClaimReadyOperationStep(ctx, workspaceID, buildRunID, revision.RevisionNo, workerID, lease)
		if errors.Is(err, teambuild.ErrNoReadyOperationStep) {
			return summarizeCompilerSteps(revision.RevisionNo, steps), nil
		}
		if err != nil {
			return CompilerExecutionResult{}, fmt.Errorf("compiler executor: claim ready operation: %w", err)
		}
		operation, ok := operations[step.OperationID]
		if !ok || string(operation.Type) != step.OperationType || !knownCompilerOperation(operation.Type) {
			failure := &OperationError{Class: teameval.FailureClassCompile, Code: "compiler_operation_identity_mismatch", Evidence: json.RawMessage(`{"source":"persisted_change_set"}`)}
			if finishErr := e.finishFailure(context.WithoutCancel(ctx), step, workerID, failure); finishErr != nil {
				return CompilerExecutionResult{}, finishErr
			}
			return CompilerExecutionResult{RevisionNo: revision.RevisionNo, Failure: failure}, nil
		}
		result, opErr := e.handleWithLease(ctx, lease, workerID, OperationContext{
			WorkspaceID: workspaceID, BuildRunID: buildRunID, Run: run,
			Revision: revision, Blueprint: blueprint, ChangeSet: changeSet,
			Operation: operation, Step: step, Steps: steps,
		})
		if opErr != nil {
			failure := normalizeOperationError(opErr, result.Evaluation)
			if finishErr := e.finishFailure(context.WithoutCancel(ctx), step, workerID, failure); finishErr != nil {
				return CompilerExecutionResult{}, finishErr
			}
			return CompilerExecutionResult{
				RevisionNo: revision.RevisionNo,
				Cancelled:  failure.Class == teameval.FailureClassCancelled,
				Failure:    failure, Evaluation: failure.Evaluation,
			}, nil
		}
		evidence, err := normalizeOperationEvidence(result.Evidence, operation)
		if err != nil {
			failure := &OperationError{Class: teameval.FailureClassCompile, Code: "compiler_operation_evidence_invalid", Cause: err}
			if finishErr := e.finishFailure(context.WithoutCancel(ctx), step, workerID, failure); finishErr != nil {
				return CompilerExecutionResult{}, finishErr
			}
			return CompilerExecutionResult{RevisionNo: revision.RevisionNo, Failure: failure}, nil
		}
		outputHash := result.OutputHash
		if outputHash == "" {
			outputHash = sha256Hex(evidence)
		}
		finishCtx := context.WithoutCancel(ctx)
		var finished teambuild.OperationStep
		if result.Skip {
			finished, err = e.Store.SkipOperationStep(finishCtx, workspaceID, buildRunID, revision.RevisionNo, step.OperationID, workerID, step.LeaseEpoch, outputHash, evidence)
		} else {
			finished, err = e.Store.SucceedOperationStep(finishCtx, workspaceID, buildRunID, revision.RevisionNo, step.OperationID, workerID, step.LeaseEpoch, outputHash, evidence)
		}
		if err != nil {
			return CompilerExecutionResult{}, fmt.Errorf("compiler executor: persist operation completion: %w", err)
		}
		e.publishStep(finishCtx, finished)
		// A passing candidate must enter the ordinary round/report ledger while
		// the build run is still round_running. Yield after durably completing
		// candidate_run so the controller can record that evidence before the
		// dependent publish operation transitions the run through publishing to
		// passed. A restart can recover the same evaluation from EvidenceJSON.
		if operation.Type == teamforge.OperationCandidateRun && result.Evaluation != nil {
			return CompilerExecutionResult{
				RevisionNo: revision.RevisionNo,
				Pending:    true,
				Evaluation: result.Evaluation,
			}, nil
		}
	}
}

func knownCompilerOperation(operationType teamforge.ChangeOperationTypeV1) bool {
	switch operationType {
	case teamforge.OperationAgentCreate, teamforge.OperationAgentUpdate,
		teamforge.OperationTeamCreate, teamforge.OperationTeamUpdate,
		teamforge.OperationRosterSet, teamforge.OperationAgentGraphCompile,
		teamforge.OperationWorkflowCompile, teamforge.OperationCandidateRun,
		teamforge.OperationPublish:
		return true
	default:
		return false
	}
}

func (e *CompilerExecutor) handleWithLease(ctx context.Context, lease time.Duration, workerID string, operation OperationContext) (OperationResult, error) {
	handlerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := make(chan struct{})
	var renewalErr error
	var renewalMu sync.Mutex
	done := make(chan struct{})
	go func() {
		defer close(done)
		interval := lease / 3
		if interval <= 0 {
			interval = time.Millisecond
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-handlerCtx.Done():
				return
			case <-ticker.C:
				_, err := e.Store.RenewOperationStepLease(handlerCtx, operation.WorkspaceID, operation.BuildRunID,
					operation.Revision.RevisionNo, operation.Step.OperationID, workerID, operation.Step.LeaseEpoch, lease)
				if err != nil {
					renewalMu.Lock()
					renewalErr = err
					renewalMu.Unlock()
					cancel()
					return
				}
			}
		}
	}()
	result, err := e.Handler.HandleOperation(handlerCtx, operation)
	close(stop)
	<-done
	renewalMu.Lock()
	leaseErr := renewalErr
	renewalMu.Unlock()
	if leaseErr != nil {
		return result, &OperationError{Class: teameval.FailureClassRuntimeInfrastructure,
			Code: "operation_lease_renewal_failed", Retryable: true, Cause: leaseErr}
	}
	if ctx.Err() != nil {
		return result, &OperationError{Class: teameval.FailureClassCancelled,
			Code: "compiler_operation_cancelled", Evidence: json.RawMessage(`{"cancelled":true}`), Cause: ctx.Err()}
	}
	return result, err
}

func (e *CompilerExecutor) finishFailure(ctx context.Context, step teambuild.OperationStep, workerID string, failure *OperationError) error {
	evidence, err := normalizeFailureEvidence(failure)
	if err != nil {
		return fmt.Errorf("compiler executor: encode typed failure evidence: %w", err)
	}
	var finished teambuild.OperationStep
	if failure.Retryable {
		finished, err = e.Store.RetryOperationStep(ctx, step.WorkspaceID, step.BuildRunID, step.RevisionNo,
			step.OperationID, workerID, step.LeaseEpoch, string(failure.Class), failure.Code, evidence)
	} else {
		finished, err = e.Store.FailOperationStep(ctx, step.WorkspaceID, step.BuildRunID, step.RevisionNo,
			step.OperationID, workerID, step.LeaseEpoch, string(failure.Class), failure.Code, evidence)
	}
	if err != nil {
		return fmt.Errorf("compiler executor: persist typed operation failure: %w", err)
	}
	e.publishStep(ctx, finished)
	return nil
}

func (e *CompilerExecutor) publishStep(ctx context.Context, step teambuild.OperationStep) {
	if e == nil || e.PublishStep == nil {
		return
	}
	e.PublishStep(ctx, step, "team_build_operation_updated")
}

func summarizeCompilerSteps(revisionNo int, steps []teambuild.OperationStep) CompilerExecutionResult {
	if len(steps) == 0 {
		return CompilerExecutionResult{RevisionNo: revisionNo, Failure: &OperationError{
			Class: teameval.FailureClassCompile, Code: "compiler_revision_has_no_operations",
		}}
	}
	complete := true
	for _, step := range steps {
		switch step.Status {
		case teambuild.OperationStatusSucceeded, teambuild.OperationStatusSkipped:
		case teambuild.OperationStatusFailed:
			failure := &OperationError{
				Class: teameval.FailureClass(step.ErrorClass), Code: step.ErrorCode, Evidence: step.EvidenceJSON,
			}
			result := CompilerExecutionResult{RevisionNo: revisionNo, Failure: failure}
			if step.OperationType == string(teamforge.OperationCandidateRun) {
				var evaluation RoundEvaluation
				if err := json.Unmarshal(step.EvidenceJSON, &evaluation); err == nil && evaluation.Diagnosis.Class != "" {
					failure.Evaluation = &evaluation
					result.Evaluation = &evaluation
				}
			}
			return result
		default:
			complete = false
		}
	}
	return CompilerExecutionResult{RevisionNo: revisionNo, Complete: complete, Pending: !complete}
}

func normalizeOperationError(err error, evaluation *RoundEvaluation) *OperationError {
	var typed *OperationError
	if errors.As(err, &typed) && typed != nil {
		copy := *typed
		if copy.Class == "" {
			copy.Class = teameval.FailureClassRuntimeInfrastructure
		}
		if strings.TrimSpace(copy.Code) == "" {
			copy.Code = "compiler_operation_failed"
		}
		if copy.Evaluation == nil {
			copy.Evaluation = evaluation
		}
		return &copy
	}
	return &OperationError{Class: teameval.FailureClassRuntimeInfrastructure,
		Code: "compiler_operation_failed", Retryable: true, Cause: err, Evaluation: evaluation}
}

func normalizeOperationEvidence(raw json.RawMessage, operation teamforge.ChangeOperationV1) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.Marshal(map[string]any{"operation_id": operation.OperationID, "operation_type": operation.Type})
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, errors.New("operation evidence must be a JSON object")
	}
	return json.Marshal(object)
}

func normalizeFailureEvidence(failure *OperationError) (json.RawMessage, error) {
	if len(failure.Evidence) != 0 {
		var object map[string]any
		if err := json.Unmarshal(failure.Evidence, &object); err == nil && object != nil {
			return json.Marshal(object)
		}
	}
	value := map[string]any{"class": failure.Class, "code": failure.Code, "retryable": failure.Retryable}
	if failure.Cause != nil {
		value["detail"] = failure.Cause.Error()
	}
	return json.Marshal(value)
}

func sha256Hex(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

// HandleOperation is the production deterministic operation adapter. The
// first compiler-v1 closure deliberately uses only existing platform CAS
// surfaces; unsupported operation contracts fail closed instead of invoking
// a meta-agent or pretending the write succeeded.
func (p *ProductionPhases) HandleOperation(ctx context.Context, operation OperationContext) (OperationResult, error) {
	if p == nil || p.Deps.Build == nil {
		return OperationResult{}, &OperationError{Class: teameval.FailureClassRuntimeInfrastructure,
			Code: "compiler_phase_dependencies_unavailable", Retryable: true}
	}
	if operation.Run.EvaluationOnly {
		switch operation.Operation.Type {
		case teamforge.OperationWorkflowCompile, teamforge.OperationCandidateRun, teamforge.OperationPublish:
		default:
			return OperationResult{}, &OperationError{Class: teameval.FailureClassGovernance,
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
		return OperationResult{}, &OperationError{Class: teameval.FailureClassCompile,
			Code:     "compiler_operation_handler_unavailable_" + string(operation.Operation.Type),
			Evidence: json.RawMessage(`{"fail_closed":true}`)}
	}
}

func (p *ProductionPhases) handleCompiledAsset(ctx context.Context, operation OperationContext) (OperationResult, error) {
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
			return OperationResult{}, &OperationError{
				Class: teameval.FailureClass(assetErr.Class), Code: assetErr.Code,
				Retryable: assetErr.Retryable, Evidence: assetErr.Evidence, Cause: assetErr.Cause,
			}
		}
		return OperationResult{}, err
	}
	return OperationResult{Skip: result.Skip, OutputHash: result.OutputHash, Evidence: result.Evidence}, nil
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

func (p *ProductionPhases) handleCompilerWorkflow(ctx context.Context, operation OperationContext) (OperationResult, error) {
	if operation.Operation.Compiler == teamforge.CompilerDeclarativeV1 {
		return p.handleCompilerDeclarativeWorkflow(ctx, operation)
	}
	var input compilerWorkflowTemplateInput
	if err := json.Unmarshal(operation.Operation.Input, &input); err != nil {
		return OperationResult{}, &OperationError{Class: teameval.FailureClassCompile,
			Code: "workflow_compile_input_invalid", Cause: err}
	}
	if input.Mode != teambuild.BlueprintWorkflowTemplate || operation.Operation.Compiler != teamforge.CompilerWorkflowV1 {
		return OperationResult{}, &OperationError{Class: teameval.FailureClassCompile,
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
		return OperationResult{}, &OperationError{Class: teameval.FailureClassCompile,
			Code: "workflow_compile_member_binding_failed", Cause: err}
	}
	if operation.Run.EvaluationOnly {
		return p.verifyEvaluationWorkflowDraft(ctx, operation, boundHash, "")
	}
	receipt, err := p.Deps.Build.ReissueReceipt(ctx, operation.WorkspaceID, operation.BuildRunID)
	if err != nil {
		return OperationResult{}, &OperationError{Class: teameval.FailureClassGovernance,
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
			return OperationResult{}, &OperationError{Class: teameval.FailureClassCompile,
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
		return OperationResult{}, &OperationError{Class: teameval.FailureClassCompile,
			Code: "workflow_compile_envelope_invalid", Cause: err}
	}
	dispatcher := teamforge.NewWorkflowBuildTools(
		operation.WorkspaceID, "platform-compiler", receipt, p.Deps.Build, p.Deps.Audit,
		p.teamForgeDeps(), p.teamForgeWriteDeps(), p.Deps.Drafts.WorkflowDrafts(operation.BuildRunID),
	)
	toolResult, err := dispatcher.Dispatch(ctx, contract.ToolCall{
		ID: operation.Operation.OperationID, Name: teamforge.ToolWorkflowBlueprintBuild, Args: string(args),
	})
	if err != nil {
		return OperationResult{}, &OperationError{Class: teameval.FailureClassRuntimeInfrastructure,
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
		return OperationResult{}, &OperationError{Class: teameval.FailureClassCompile,
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
		return OperationResult{}, &OperationError{Class: teameval.FailureClassCompile,
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
		return OperationResult{}, err
	}
	return OperationResult{OutputHash: boundHash, Evidence: evidence}, nil
}

func (p *ProductionPhases) handleCompilerDeclarativeWorkflow(
	ctx context.Context,
	operation OperationContext,
) (OperationResult, error) {
	var input compilerWorkflowDeclarativeInput
	if err := json.Unmarshal(operation.Operation.Input, &input); err != nil {
		return OperationResult{}, &OperationError{Class: teameval.FailureClassCompile,
			Code: "workflow_compile_input_invalid", Cause: err}
	}
	if input.Mode != teambuild.BlueprintWorkflowDeclarativeV1 ||
		operation.Blueprint.Workflow.Mode != teambuild.BlueprintWorkflowDeclarativeV1 {
		return OperationResult{}, &OperationError{Class: teameval.FailureClassCompile,
			Code: "workflow_declarative_mode_mismatch"}
	}
	if err := teamforge.ValidateFrozenDeclarativeWorkflowSpecV1(input.FrozenSpec); err != nil {
		return OperationResult{}, &OperationError{Class: teameval.FailureClassCompile,
			Code: "workflow_declarative_frozen_spec_invalid", Cause: err}
	}
	if !compilerDeclarativeSpecHashesMatch(input, operation.Blueprint.Workflow.DeclarativeSpecHash) {
		return OperationResult{}, &OperationError{Class: teameval.FailureClassCompile,
			Code: "workflow_declarative_spec_hash_mismatch"}
	}
	compiledHash, err := declarativeCompiledGraphHash(input.FrozenSpec)
	if err != nil || compiledHash != input.CompiledHash {
		return OperationResult{}, &OperationError{Class: teameval.FailureClassCompile,
			Code: "workflow_declarative_compiled_hash_mismatch", Cause: err}
	}
	binding := input.FrozenSpec.BuildBinding
	if binding.BuildRunID != operation.BuildRunID ||
		binding.BriefHash != operation.Run.BriefHash ||
		binding.ContractHash != operation.Run.ContractHash ||
		binding.BaselineHash != operation.Revision.BaselineHash ||
		!reflect.DeepEqual(binding.AssetScope, operation.Run.AssetScope) {
		return OperationResult{}, &OperationError{Class: teameval.FailureClassCompile,
			Code: "workflow_declarative_build_binding_mismatch"}
	}

	materializedSpec, err := resolveCreateDeclarativeMaterializationSpec(
		ctx, p.Deps.Agents, operation.WorkspaceID, operation.Blueprint, input.FrozenSpec,
	)
	if err != nil {
		return OperationResult{}, &OperationError{Class: teameval.FailureClassCompile,
			Code: "workflow_compile_member_binding_failed", Cause: err}
	}
	materializedHash, err := declarativeCompiledGraphHash(materializedSpec)
	if err != nil {
		return OperationResult{}, &OperationError{Class: teameval.FailureClassCompile,
			Code: "workflow_declarative_materialized_hash_failed", Cause: err}
	}
	if operation.Run.EvaluationOnly {
		return p.verifyEvaluationWorkflowDraft(ctx, operation, materializedHash, materializedSpec.SpecHash)
	}

	workflowID := strings.TrimSpace(operation.Operation.Target)
	version, skip, err := p.materializeDeclarativeWorkflow(ctx, operation, workflowID, materializedSpec)
	if err != nil {
		return OperationResult{}, &OperationError{Class: teameval.FailureClassRuntimeInfrastructure,
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
	operation OperationContext,
	wantCompiledHash, specHash string,
) (OperationResult, error) {
	workflowID := strings.TrimSpace(operation.Operation.Target)
	versionNo, err := p.latestDraftVersion(ctx, operation.WorkspaceID, workflowID)
	if err != nil {
		return OperationResult{}, &OperationError{Class: teameval.FailureClassGovernance,
			Code: "evaluation_workflow_draft_unavailable", Cause: err}
	}
	version, err := p.Deps.Workflows.GetVersion(ctx, operation.WorkspaceID, workflowID, versionNo)
	if err != nil {
		return OperationResult{}, &OperationError{Class: teameval.FailureClassRuntimeInfrastructure,
			Code: "evaluation_workflow_draft_read_failed", Retryable: true, Cause: err}
	}
	raw, err := json.Marshal(struct {
		Trigger json.RawMessage `json:"trigger"`
		Graph   json.RawMessage `json:"graph"`
	}{version.TriggerConfig, version.GraphDefinition})
	if err != nil {
		return OperationResult{}, err
	}
	actualHash, err := frozen.HashCanonicalJSON(raw)
	if err != nil {
		return OperationResult{}, err
	}
	if actualHash != wantCompiledHash {
		return OperationResult{}, &OperationError{Class: teameval.FailureClassGovernance,
			Code:     "evaluation_workflow_draft_changed",
			Evidence: json.RawMessage(`{"evaluation_only":true,"asset_changed":true}`)}
	}
	evidence, err := json.Marshal(compilerWorkflowVersionEvidence{
		Compiler: operation.Operation.Compiler, WorkflowID: workflowID,
		WorkflowVersion: versionNo, WorkflowRef: fmt.Sprintf("workflow-version:%d", versionNo),
		SpecHash: specHash, CompiledHash: actualHash, PlannedShapeHash: wantCompiledHash,
	})
	if err != nil {
		return OperationResult{}, err
	}
	return OperationResult{Skip: true, OutputHash: actualHash, Evidence: evidence}, nil
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
) (OperationResult, error) {
	evidence, err := json.Marshal(evidenceValue)
	if err != nil {
		return OperationResult{}, err
	}
	return OperationResult{Skip: skip, OutputHash: evidenceValue.CompiledHash, Evidence: evidence}, nil
}

func declarativeCompiledGraphHash(spec teamforge.FrozenDeclarativeWorkflowSpecV1) (string, error) {
	raw, err := json.Marshal(struct {
		Trigger json.RawMessage `json:"trigger_config"`
		Graph   json.RawMessage `json:"graph_definition"`
	}{Trigger: spec.TriggerConfig, Graph: spec.GraphDefinition})
	if err != nil {
		return "", err
	}
	return frozen.HashCanonicalJSON(raw)
}

func (p *ProductionPhases) materializeDeclarativeWorkflow(
	ctx context.Context,
	operation OperationContext,
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
			CreatedBy: DefaultActor,
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
		draft, err = p.Deps.Workflows.CreateDraft(ctx, operation.WorkspaceID, workflowID, DefaultActor)
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
			CreatedBy: DefaultActor,
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

func (p *ProductionPhases) handleCompilerCandidate(ctx context.Context, operation OperationContext) (OperationResult, error) {
	workflowID, workflowVersion, err := compilerCandidateWorkflowVersion(operation)
	if err != nil {
		return OperationResult{}, &OperationError{Class: teameval.FailureClassCompile,
			Code: "candidate_workflow_version_unavailable", Cause: err}
	}
	evaluation, err := p.Evaluate(ctx, RoundContext{
		WorkspaceID: operation.WorkspaceID, BuildRunID: operation.BuildRunID,
		RoundNo: operation.Revision.RevisionNo, CandidateAttempt: operation.Step.Attempt,
		FrozenWorkflowID: workflowID, FrozenWorkflowVersion: workflowVersion,
		Run: operation.Run,
	})
	if err != nil {
		return OperationResult{}, &OperationError{Class: teameval.FailureClassRuntimeInfrastructure,
			Code: "candidate_run_failed", Retryable: true, Cause: err}
	}
	evidence, marshalErr := json.Marshal(evaluation)
	if marshalErr != nil {
		return OperationResult{}, marshalErr
	}
	if evaluation.Diagnosis.Class == teameval.FailureClassPass {
		return OperationResult{OutputHash: evaluation.CandidateRef, Evidence: evidence, Evaluation: &evaluation}, nil
	}
	code := strings.TrimSpace(evaluation.Diagnosis.OriginalErrorCode)
	if code == "" {
		code = string(evaluation.Diagnosis.Class)
	}
	return OperationResult{Evaluation: &evaluation}, &OperationError{
		Class: evaluation.Diagnosis.Class, Code: code,
		Retryable: candidateFailureRetryable(evaluation.Diagnosis, operation.Step.Attempt),
		Evidence:  evidence, Evaluation: &evaluation,
	}
}

func compilerCandidateWorkflowVersion(operation OperationContext) (string, int, error) {
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

func (p *ProductionPhases) handleCompilerPublish(ctx context.Context, operation OperationContext) (OperationResult, error) {
	run, err := p.Deps.Build.GetBuildRun(ctx, operation.WorkspaceID, operation.BuildRunID)
	if err != nil {
		return OperationResult{}, &OperationError{Class: teameval.FailureClassRuntimeInfrastructure,
			Code: "publish_run_reload_failed", Retryable: true, Cause: err}
	}
	if run.Status == teambuild.StatusPassed && run.FinalRef != nil {
		return OperationResult{Skip: true, OutputHash: run.FinalRef.Ref,
			Evidence: json.RawMessage(`{"publication":"already_completed"}`)}, nil
	}
	if run.Status == teambuild.StatusRoundRunning {
		if _, err := p.Deps.Build.TransitionStatus(ctx, operation.WorkspaceID, operation.BuildRunID,
			teambuild.StatusRoundRunning, teambuild.StatusPublishing, DefaultActor,
			fmt.Sprintf("compiler revision %d candidate passed", operation.Revision.RevisionNo)); err != nil {
			return OperationResult{}, &OperationError{Class: teameval.FailureClassRuntimeInfrastructure,
				Code: "publish_transition_failed", Retryable: true, Cause: err}
		}
	} else if run.Status != teambuild.StatusPublishing {
		return OperationResult{}, &OperationError{Class: teameval.FailureClassGovernance,
			Code: "publish_run_status_invalid", Cause: fmt.Errorf("status=%s", run.Status)}
	}
	if err := p.restoreCompilerCandidate(ctx, operation); err != nil {
		return OperationResult{}, &OperationError{Class: teameval.FailureClassRuntimeInfrastructure,
			Code: "publish_candidate_restore_failed", Retryable: true, Cause: err}
	}
	if err := p.PublishStep(ctx, operation.WorkspaceID, operation.BuildRunID); err != nil {
		return OperationResult{}, &OperationError{Class: teameval.FailureClassRuntimeInfrastructure,
			Code: "publish_operation_failed", Retryable: true, Cause: err}
	}
	p.mu.Lock()
	candidate := p.lastCandidate
	p.mu.Unlock()
	return OperationResult{OutputHash: candidate.ContentHash,
		Evidence: json.RawMessage(`{"publication":"completed"}`)}, nil
}

func (p *ProductionPhases) restoreCompilerCandidate(ctx context.Context, operation OperationContext) error {
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
	tx, err := p.Deps.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	candidate, err := p.Deps.Workflows.GetCandidateTx(ctx, tx, operation.WorkspaceID, workflowID, candidateHash)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.lastCandidate = candidate
	p.mu.Unlock()
	return nil
}
