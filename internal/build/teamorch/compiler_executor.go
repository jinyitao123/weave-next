package teamorch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/build/teameval"
	"github.com/jinyitao123/weave/internal/build/teamforge"
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
}

func NewCompilerExecutor(store CompilerOperationStore, handler OperationHandler) *CompilerExecutor {
	return &CompilerExecutor{
		Store: store, Handler: handler,
		WorkerID: "compiler-executor", LeaseDuration: 30 * time.Second,
	}
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
		if result.Skip {
			_, err = e.Store.SkipOperationStep(finishCtx, workspaceID, buildRunID, revision.RevisionNo, step.OperationID, workerID, step.LeaseEpoch, outputHash, evidence)
		} else {
			_, err = e.Store.SucceedOperationStep(finishCtx, workspaceID, buildRunID, revision.RevisionNo, step.OperationID, workerID, step.LeaseEpoch, outputHash, evidence)
		}
		if err != nil {
			return CompilerExecutionResult{}, fmt.Errorf("compiler executor: persist operation completion: %w", err)
		}
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
	if failure.Retryable {
		_, err = e.Store.RetryOperationStep(ctx, step.WorkspaceID, step.BuildRunID, step.RevisionNo,
			step.OperationID, workerID, step.LeaseEpoch, string(failure.Class), failure.Code, evidence)
	} else {
		_, err = e.Store.FailOperationStep(ctx, step.WorkspaceID, step.BuildRunID, step.RevisionNo,
			step.OperationID, workerID, step.LeaseEpoch, string(failure.Class), failure.Code, evidence)
	}
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
