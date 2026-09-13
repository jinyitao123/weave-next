package teamorch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/build/teameval"
	"github.com/jinyitao123/weave/internal/build/teamforge"
)

// CompilerOperationStore is the durable operation-DAG surface used by the
// compiler executor. *teambuild.Store is the production implementation.
type CompilerOperationStore interface {
	GetBuildRun(context.Context, string, string) (teambuild.TeamBuildRun, error)
	GetLatestBlueprintRevision(context.Context, string, string) (teambuild.BlueprintRevision, error)
	ListOperationSteps(context.Context, string, string, int) ([]teambuild.OperationStep, error)
	NextReadyOperationStep(context.Context, string, string, int) (teambuild.OperationStep, error)
	FinishOperationStep(context.Context, string, string, int, string, string, string, string, string, json.RawMessage, teambuild.ExecutionFence) (teambuild.OperationStep, error)
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
	// PhysicalAttempt comes from the platform task claim. It is never stored as
	// a second Builder-owned attempt or lease.
	PhysicalAttempt int
}

// OperationHandler performs one closed ChangeSet operation. Product construction
// adapters implement this port; the builder does not receive their concrete stores.
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
// Retryable failures leave business progress pending; the platform task owns
// the physical attempt receipt and schedules the next claim.
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

// CompilerExecutor executes one deterministic business step inside the
// platform task that already owns claim, lease, deadline, retry, and cancel.
type CompilerExecutor struct {
	Store   CompilerOperationStore
	Handler OperationHandler
}

func NewCompilerExecutor(store CompilerOperationStore, handler OperationHandler) *CompilerExecutor {
	return &CompilerExecutor{Store: store, Handler: handler}
}

type compilerExecutionFenceKey struct{}

// WithCompilerExecutionFence binds the platform's current-claim validator to
// one task handler invocation without importing taskqueue into Builder.
func WithCompilerExecutionFence(ctx context.Context, fence teambuild.ExecutionFence) context.Context {
	return context.WithValue(ctx, compilerExecutionFenceKey{}, fence)
}

func compilerExecutionFence(ctx context.Context) (teambuild.ExecutionFence, error) {
	fence, _ := ctx.Value(compilerExecutionFenceKey{}).(teambuild.ExecutionFence)
	if fence == nil {
		return nil, errors.New("compiler executor: platform execution fence is required")
	}
	return fence, nil
}

func (e *CompilerExecutor) Execute(ctx context.Context, workspaceID, buildRunID string) (CompilerExecutionResult, error) {
	if e == nil || e.Store == nil || e.Handler == nil {
		return CompilerExecutionResult{}, errors.New("compiler executor: store and operation handler are required")
	}
	fence, err := compilerExecutionFence(ctx)
	if err != nil {
		return CompilerExecutionResult{}, err
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
	step, err := e.Store.NextReadyOperationStep(ctx, workspaceID, buildRunID, revision.RevisionNo)
	if errors.Is(err, teambuild.ErrNoReadyOperationStep) {
		return summarizeCompilerSteps(revision.RevisionNo, steps), nil
	}
	if err != nil {
		return CompilerExecutionResult{}, fmt.Errorf("compiler executor: select ready operation: %w", err)
	}
	physicalAttempt := 1
	if current, ok := execution.CurrentTaskFromContext(ctx); ok && current.ClaimEpoch > 0 {
		physicalAttempt = int(current.ClaimEpoch)
	}
	operation, ok := operations[step.OperationID]
	if !ok || string(operation.Type) != step.OperationType || !knownCompilerOperation(operation.Type) {
		failure := &OperationError{Class: teameval.FailureClassCompile, Code: "compiler_operation_identity_mismatch", Evidence: json.RawMessage(`{"source":"persisted_change_set"}`)}
		if finishErr := e.finishFailure(context.WithoutCancel(ctx), step, failure, fence); finishErr != nil {
			return CompilerExecutionResult{}, finishErr
		}
		return CompilerExecutionResult{RevisionNo: revision.RevisionNo, Failure: failure}, nil
	}
	result, opErr := e.Handler.HandleOperation(ctx, OperationContext{
		WorkspaceID: workspaceID, BuildRunID: buildRunID, Run: run,
		Revision: revision, Blueprint: blueprint, ChangeSet: changeSet,
		Operation: operation, Step: step, Steps: steps, PhysicalAttempt: physicalAttempt,
	})
	if ctx.Err() != nil {
		return CompilerExecutionResult{RevisionNo: revision.RevisionNo, Cancelled: true}, nil
	}
	if opErr != nil {
		failure := normalizeOperationError(opErr, result.Evaluation)
		if !failure.Retryable {
			if finishErr := e.finishFailure(context.WithoutCancel(ctx), step, failure, fence); finishErr != nil {
				return CompilerExecutionResult{}, finishErr
			}
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
		if finishErr := e.finishFailure(context.WithoutCancel(ctx), step, failure, fence); finishErr != nil {
			return CompilerExecutionResult{}, finishErr
		}
		return CompilerExecutionResult{RevisionNo: revision.RevisionNo, Failure: failure}, nil
	}
	outputHash := result.OutputHash
	if outputHash == "" {
		outputHash = sha256Hex(evidence)
	}
	status := teambuild.OperationStatusSucceeded
	if result.Skip {
		status = teambuild.OperationStatusSkipped
	}
	if _, err = e.Store.FinishOperationStep(context.WithoutCancel(ctx), workspaceID, buildRunID, revision.RevisionNo,
		step.OperationID, status, outputHash, "", "", evidence, fence); err != nil {
		return CompilerExecutionResult{}, fmt.Errorf("compiler executor: persist operation completion: %w", err)
	}
	if operation.Type == teamforge.OperationCandidateRun && result.Evaluation != nil {
		return CompilerExecutionResult{RevisionNo: revision.RevisionNo, Pending: true, Evaluation: result.Evaluation}, nil
	}
	steps, err = e.Store.ListOperationSteps(ctx, workspaceID, buildRunID, revision.RevisionNo)
	if err != nil {
		return CompilerExecutionResult{}, fmt.Errorf("compiler executor: reload operation steps: %w", err)
	}
	return summarizeCompilerSteps(revision.RevisionNo, steps), nil
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

func (e *CompilerExecutor) finishFailure(ctx context.Context, step teambuild.OperationStep, failure *OperationError, fence teambuild.ExecutionFence) error {
	evidence, err := normalizeFailureEvidence(failure)
	if err != nil {
		return fmt.Errorf("compiler executor: encode typed failure evidence: %w", err)
	}
	_, err = e.Store.FinishOperationStep(ctx, step.WorkspaceID, step.BuildRunID, step.RevisionNo,
		step.OperationID, teambuild.OperationStatusFailed, "", string(failure.Class), failure.Code, evidence, fence)
	if err != nil {
		return fmt.Errorf("compiler executor: persist typed operation failure: %w", err)
	}
	return nil
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
