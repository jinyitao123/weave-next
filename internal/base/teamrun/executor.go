package teamrun

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

// Agent nodes use one execution budget in both candidate validation and
// published workflows. A candidate must exercise the same timeout contract
// that production will enforce after publication. Twenty minutes keeps the
// execution bounded while allowing a Mac CLI worker to finish a realistic
// requirements pass that can legitimately exceed the former ten-minute cap.
var agentNodeExecutionTimeout = 20 * time.Minute

type ExecutionError struct {
	Code  ErrorCode
	Cause error
}

func (e *ExecutionError) Error() string {
	if e == nil {
		return ""
	}
	if e.Cause == nil {
		return string(e.Code)
	}
	return fmt.Sprintf("%s: %v", e.Code, e.Cause)
}

func (e *ExecutionError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func executionError(code ErrorCode, cause error) error {
	return &ExecutionError{Code: code, Cause: cause}
}

func executorIdentity(taskID, workerID string) string {
	return "teamrun-executor:" + taskID + ":" + workerID
}

func executorClaimKey(taskID string) string {
	return "teamrun-claim:" + taskID
}

func executorTerminalKey(taskID string, status Status) string {
	return "teamrun-terminal:" + string(status) + ":" + taskID
}

func executorParkKey(taskID string, resumeGeneration ResumeGeneration) string {
	return fmt.Sprintf("teamrun-park:%s:%d", taskID, resumeGeneration)
}

func executorReclaimKey(taskID string, epoch ExecutionLeaseEpoch) string {
	return fmt.Sprintf("teamrun-reclaim:%s:%d", taskID, epoch)
}

func (e *Executor) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now().UTC()
}

func (e *Executor) claimRunning(
	ctx context.Context,
	run TeamRun,
	task *taskqueue.Task,
	executorID string,
) (TeamRun, error) {
	tx, err := e.Transactions.Begin(ctx)
	if err != nil {
		return TeamRun{}, fmt.Errorf("begin team run claim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	claimed, err := e.Runs.ClaimRunningTx(ctx, tx, ClaimRequest{
		WorkspaceID:                 run.WorkspaceID,
		RunID:                       run.RunID,
		ExpectedStatus:              StatusQueued,
		ExpectedTeamRunGeneration:   run.Generation,
		ExpectedExecutionLeaseEpoch: run.ExecutionLeaseEpoch,
		ExpectedResumeGeneration:    run.ResumeGeneration,
		ExecutorID:                  executorID,
		IdempotencyKey:              executorClaimKey(task.ID),
		Actor:                       executorID,
		Source:                      consumerSource,
		OccurredAt:                  e.now(),
	})
	if err != nil {
		return TeamRun{}, fmt.Errorf("claim running team run: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return TeamRun{}, fmt.Errorf("commit team run claim: %w", err)
	}
	if _, err := e.admitFrozenAttempt(ctx, claimed); err != nil {
		return TeamRun{}, err
	}
	return claimed, nil
}

var errTaskLeaseLost = errors.New("team workflow task lease lost")

func (e *Executor) executeWithHeartbeat(
	ctx context.Context,
	task *taskqueue.Task,
	workerID string,
	execute func(context.Context) (RuntimeResult, error),
) (RuntimeResult, error) {
	interval := e.HeartbeatInterval
	if interval <= 0 {
		interval = 20 * time.Second
	}
	execCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type outcome struct {
		result RuntimeResult
		err    error
	}
	outcomes := make(chan outcome, 1)
	go func() {
		result, err := execute(execCtx)
		outcomes <- outcome{result: result, err: err}
	}()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return RuntimeResult{}, ctx.Err()
		case <-ticker.C:
			if err := e.Tasks.Heartbeat(ctx, task.ID, workerID); err != nil {
				cancel()
				return RuntimeResult{}, fmt.Errorf("%w: %v", errTaskLeaseLost, err)
			}
		case outcome := <-outcomes:
			return outcome.result, outcome.err
		}
	}
}

func (e *Executor) reclaimRunning(
	ctx context.Context,
	observed TeamRun,
	task *taskqueue.Task,
	executorID string,
) (TeamRun, *WorkflowCheckpointV1, bool, error) {
	now := e.now()
	tx, err := e.Transactions.Begin(ctx)
	if err != nil {
		return TeamRun{}, nil, false, fmt.Errorf("begin team run reclaim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	run, err := e.Runs.GetForUpdateTx(ctx, tx, observed.WorkspaceID, observed.RunID)
	if err != nil {
		return TeamRun{}, nil, false, err
	}
	if run.Status != StatusRunning {
		return TeamRun{}, nil, false, ErrTeamRunStateConflict
	}
	stateStore := loomruntime.NewPGTerminalStateStore()
	if err := stateStore.LockTerminalRun(ctx, tx, run.WorkspaceID, run.RunID); err != nil {
		return TeamRun{}, nil, false, fmt.Errorf("lock frozen terminal run: %w", err)
	}
	marker, markerPresent, err := stateStore.ReadTerminalMarkerForUpdate(
		ctx, tx, run.WorkspaceID, run.RunID,
	)
	if err != nil {
		return TeamRun{}, nil, false, fmt.Errorf("read frozen terminal marker: %w", err)
	}
	currentLease, leasePresent, err := stateStore.ReadAttemptLeaseForUpdate(
		ctx, tx, run.WorkspaceID, run.RunID,
	)
	if err != nil {
		return TeamRun{}, nil, false, fmt.Errorf("read frozen attempt lease: %w", err)
	}
	if markerPresent && marker.Phase == loomruntime.TerminalMarkerPhaseFinal {
		if !leasePresent {
			return TeamRun{}, nil, false, errors.New("frozen final terminal lease is missing")
		}
		terminal, err := e.replayFrozenTerminalTx(
			ctx, tx, run, task, marker, currentLease,
		)
		if err != nil {
			return TeamRun{}, nil, false, fmt.Errorf("replay frozen terminal: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return TeamRun{}, nil, false, fmt.Errorf("commit frozen terminal replay: %w", err)
		}
		return terminal, nil, true, nil
	}

	checkpoint, recoverableErr := e.Checkpoints.GetTx(
		ctx, tx, run.WorkspaceID, run.RunID,
	)
	checkpointPresent := recoverableErr == nil
	if !leasePresent && errors.Is(recoverableErr, ErrWorkflowCheckpointMissing) {
		recoverableErr = nil
	}
	if recoverableErr == nil && checkpointPresent {
		recoverableErr = ValidateCheckpointRun(checkpoint, run)
	}
	if recoverableErr == nil && checkpointPresent {
		typedSnapshot, snapshotErr := e.Consumer.Snapshots.GetByRunID(
			ctx, run.WorkspaceID, run.RunSnapshotID,
		)
		if snapshotErr != nil {
			recoverableErr = fmt.Errorf("read reclaim snapshot: %w", snapshotErr)
		} else {
			recoverableErr = ValidateCheckpointSnapshot(checkpoint, typedSnapshot)
		}
	}
	if recoverableErr != nil {
		abandoned, abandonErr := e.Runs.AbandonUnrecoverableTx(
			ctx, tx, AbandonUnrecoverableRequest{
				WorkspaceID:                 run.WorkspaceID,
				RunID:                       run.RunID,
				ExpectedStatus:              StatusRunning,
				ExpectedTeamRunGeneration:   run.Generation,
				ExpectedExecutionLeaseEpoch: run.ExecutionLeaseEpoch,
				ExpectedResumeGeneration:    run.ResumeGeneration,
				Cause:                       recoverableErr,
				IdempotencyKey:              executorReclaimKey(task.ID, run.ExecutionLeaseEpoch),
				Actor:                       executorID,
				Source:                      consumerSource,
				OccurredAt:                  now,
			},
		)
		if abandonErr != nil {
			return TeamRun{}, nil, false, fmt.Errorf("abandon unrecoverable team run: %w", abandonErr)
		}
		if err := tx.Commit(ctx); err != nil {
			return TeamRun{}, nil, false, fmt.Errorf("commit team run abandon: %w", err)
		}
		return abandoned, nil, true, nil
	}

	reclaimed, err := e.Runs.ReclaimRunningTx(ctx, tx, ReclaimRequest{
		WorkspaceID:                 run.WorkspaceID,
		RunID:                       run.RunID,
		ExpectedStatus:              StatusRunning,
		ExpectedTeamRunGeneration:   run.Generation,
		ExpectedExecutionLeaseEpoch: run.ExecutionLeaseEpoch,
		ExpectedResumeGeneration:    run.ResumeGeneration,
		ExecutorID:                  executorID,
		IdempotencyKey:              executorReclaimKey(task.ID, run.ExecutionLeaseEpoch),
		Actor:                       executorID,
		Source:                      consumerSource,
		OccurredAt:                  now,
	})
	if err != nil {
		return TeamRun{}, nil, false, fmt.Errorf("reclaim team run: %w", err)
	}
	if leasePresent {
		if _, err := advanceFrozenAttemptTx(
			ctx, tx, currentLease, reclaimed,
		); err != nil {
			return TeamRun{}, nil, false, fmt.Errorf(
				"advance reclaimed frozen attempt: %w", err,
			)
		}
	} else {
		records, err := e.runtimeRecordStore()
		if err != nil {
			return TeamRun{}, nil, false, err
		}
		if _, err := loomruntime.AdmitFrozenAttemptTx(
			ctx, records, tx, frozenAttemptAdmission(reclaimed),
		); err != nil {
			return TeamRun{}, nil, false, fmt.Errorf(
				"admit reclaimed frozen attempt: %w", err,
			)
		}
	}
	if checkpointPresent {
		checkpoint.TeamRunGeneration = reclaimed.Generation
		checkpoint.ExecutionLeaseEpoch = reclaimed.ExecutionLeaseEpoch
		checkpoint.WrittenAt = now
		if _, err := e.Checkpoints.PutTx(ctx, tx, checkpoint); err != nil {
			return TeamRun{}, nil, false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return TeamRun{}, nil, false, fmt.Errorf("commit team run reclaim: %w", err)
	}
	if !checkpointPresent {
		return reclaimed, nil, false, nil
	}
	return reclaimed, &checkpoint, false, nil
}

func executionErrorCode(err error) ErrorCode {
	var classified *ExecutionError
	if errors.As(err, &classified) && ValidateErrorCode(classified.Code) {
		return classified.Code
	}
	return ErrorCodeRuntimeIncompatible
}

func (e *Executor) succeedRunning(
	ctx context.Context,
	run TeamRun,
	task *taskqueue.Task,
	executorID string,
	usage loomruntime.UsageTotals,
	usageComplete bool,
	usageIncompleteReason string,
) (TeamRun, error) {
	if err := e.commitFrozenNormalTerminal(
		ctx, run, "success", "completed", usage,
		usageComplete, usageIncompleteReason,
	); err != nil {
		return TeamRun{}, err
	}
	tx, err := e.Transactions.Begin(ctx)
	if err != nil {
		return TeamRun{}, fmt.Errorf("begin team run success: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	succeeded, err := e.Runs.SucceedTx(ctx, tx, SucceedRequest{
		WorkspaceID:                 run.WorkspaceID,
		RunID:                       run.RunID,
		ExpectedStatus:              StatusRunning,
		ExpectedTeamRunGeneration:   run.Generation,
		ExpectedExecutionLeaseEpoch: run.ExecutionLeaseEpoch,
		ExpectedResumeGeneration:    run.ResumeGeneration,
		ExecutorID:                  executorID,
		IdempotencyKey:              executorTerminalKey(task.ID, StatusSucceeded),
		Actor:                       executorID,
		Source:                      consumerSource,
		OccurredAt:                  e.now(),
	})
	if err != nil {
		return TeamRun{}, fmt.Errorf("succeed team run: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return TeamRun{}, fmt.Errorf("commit team run success: %w", err)
	}
	return succeeded, nil
}

func (e *Executor) failRunning(
	ctx context.Context,
	run TeamRun,
	task *taskqueue.Task,
	executorID string,
	runErr error,
	usage loomruntime.UsageTotals,
	usageComplete bool,
	usageIncompleteReason string,
) (TeamRun, error) {
	code := ErrorCodeExecutionUnrecoverable
	var classified *ExecutionError
	if errors.As(runErr, &classified) && ValidateErrorCode(classified.Code) &&
		classified.Code != ErrorCodeCancelled {
		code = classified.Code
	}
	if err := e.commitFrozenNormalTerminal(
		ctx, run, "failed", string(code), usage,
		usageComplete, usageIncompleteReason,
	); err != nil {
		return TeamRun{}, err
	}
	tx, err := e.Transactions.Begin(ctx)
	if err != nil {
		return TeamRun{}, fmt.Errorf("begin team run failure: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	failed, err := e.Runs.FailTx(ctx, tx, FailRequest{
		WorkspaceID:                 run.WorkspaceID,
		RunID:                       run.RunID,
		ExpectedStatus:              StatusRunning,
		ExpectedTeamRunGeneration:   run.Generation,
		ExpectedExecutionLeaseEpoch: run.ExecutionLeaseEpoch,
		ExpectedResumeGeneration:    run.ResumeGeneration,
		ExecutorID:                  executorID,
		ErrorCode:                   code,
		Cause:                       runErr,
		IdempotencyKey:              executorTerminalKey(task.ID, StatusFailed),
		Actor:                       executorID,
		Source:                      consumerSource,
		OccurredAt:                  e.now(),
	})
	if err != nil {
		return TeamRun{}, fmt.Errorf("fail team run: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return TeamRun{}, fmt.Errorf("commit team run failure: %w", err)
	}
	return failed, nil
}

func (e *Executor) finishTerminalTask(
	ctx context.Context,
	task *taskqueue.Task,
	workerID string,
	run TeamRun,
	output json.RawMessage,
) error {
	switch run.Status {
	case StatusSucceeded:
		if len(output) == 0 {
			output = json.RawMessage(`{"status":"succeeded"}`)
		}
		return e.Tasks.CompleteClaimed(ctx, task.ID, workerID, output, run.RunID)
	case StatusFailed, StatusCancelled, StatusAbandoned:
		code := ErrorCodeStateConflict
		if run.ErrorCode != nil {
			code = *run.ErrorCode
		}
		return e.Tasks.FailClaimed(ctx, task.ID, workerID, string(code))
	default:
		return fmt.Errorf("team run %q is not terminal", run.RunID)
	}
}

type ArtifactReader interface {
	GetArtifact(context.Context, string, string, int) (*workflow.PublishedArtifactContent, error)
}

// CandidateArtifactReader reads one frozen publication candidate by its
// content hash. A candidate test run's snapshot carries the content hash, and
// the runtime uses this reader instead of the published artifact reader so
// the tested version is exactly the candidate that publish will release.
type CandidateArtifactReader interface {
	GetCandidateArtifact(
		context.Context,
		string,
		string,
		int,
		string,
	) (*workflow.PublishedArtifactContent, error)
}

type RuntimeCredentialResolverFactory func(
	string,
) (workflow.RuntimeCredentialResolver, error)

type WorkflowOutputRecorder interface {
	RecordWorkflowOutput(context.Context, deliverable.WorkflowOutput) error
}

type WorkflowSerialRuntime struct {
	Artifacts   ArtifactReader
	Loader      *workflow.RuntimeLoader
	HostFactory workflow.RuntimeHostFactory
	// HostFactoryForSnapshot optionally replaces HostFactory when the run
	// consumes a frozen candidate snapshot. It receives the snapshot's build
	// run ID and candidate content hash so a test harness can bind a scripted
	// LLM to exactly the candidate under test. A nil field keeps the
	// production HostFactory for every run (zero behavior change).
	HostFactoryForSnapshot func(buildRunID, candidateHash string) workflow.RuntimeHostFactory
	CredentialResolvers    RuntimeCredentialResolverFactory
	Transactions           TransactionBeginner
	Runs                   *PGStore
	Checkpoints            *PGCheckpointStore
	Tasks                  ExecutorTaskStore
	Snapshots              SnapshotReader
	OutputRecorder         WorkflowOutputRecorder
	Now                    func() time.Time
}

func (r *WorkflowSerialRuntime) Execute(
	ctx context.Context,
	run TeamRun,
	task *taskqueue.Task,
) (RuntimeResult, error) {
	prepared, err := r.prepare(ctx, run, task)
	if err != nil {
		return RuntimeResult{Status: RuntimeFailed}, err
	}
	defer prepared.artifact.Close()
	result := runSerialMachine(
		ctx,
		prepared.graph,
		prepared.payload,
		prepared.artifact,
		prepared.runInput,
		serialMachineStart{
			SourceKind: run.SourceKind, Now: r.now(), Run: run,
			ArtifactHash: prepared.envelope.ContentHash,
			Candidate:    prepared.roundBoundCandidate(),
			RecordOutput: r.workflowOutputRecorder(run),
		},
	)
	return runtimeResultFromSerial(result, prepared.payload)
}

func (r *WorkflowSerialRuntime) ResumeCheckpoint(
	ctx context.Context,
	run TeamRun,
	task *taskqueue.Task,
	checkpoint WorkflowCheckpointV1,
) (RuntimeResult, error) {
	if err := ValidateCheckpointRun(checkpoint, run); err != nil {
		return RuntimeResult{Status: RuntimeFailed}, executionError(ErrorCodeIdentityMismatch, err)
	}
	prepared, err := r.prepare(ctx, run, task)
	if err != nil {
		return RuntimeResult{Status: RuntimeFailed}, err
	}
	defer prepared.artifact.Close()
	outputs, err := decodeCheckpointOutputs(checkpoint.CompletedOutputs)
	if err != nil {
		return RuntimeResult{Status: RuntimeFailed}, executionError(ErrorCodeSnapshotUnavailable, err)
	}
	seedUsage := loomruntime.NewUsageAccumulator()
	if len(checkpoint.Usage) != 0 {
		restored, err := loomruntime.UnmarshalUsageAccumulator(checkpoint.Usage)
		if err != nil {
			return RuntimeResult{Status: RuntimeFailed}, executionError(
				ErrorCodeSnapshotUnavailable,
				fmt.Errorf("restore serial usage checkpoint: %w", err),
			)
		}
		seedUsage = restored
	}
	result := runSerialMachine(
		ctx,
		prepared.graph,
		prepared.payload,
		prepared.artifact,
		prepared.runInput,
		serialMachineStart{
			NodeID: checkpoint.NodeID, Outputs: outputs,
			SourceKind: run.SourceKind, Now: r.now(), Run: run,
			ArtifactHash:          prepared.envelope.ContentHash,
			Candidate:             prepared.roundBoundCandidate(),
			Usage:                 seedUsage,
			UsageComplete:         checkpoint.UsageComplete,
			UsageIncompleteReason: checkpoint.UsageIncompleteReason,
			RecordOutput:          r.workflowOutputRecorder(run),
		},
	)
	return runtimeResultFromSerial(result, prepared.payload)
}

func (r *WorkflowSerialRuntime) TimerResumeTarget(
	ctx context.Context,
	run TeamRun,
	task *taskqueue.Task,
	checkpoint WorkflowCheckpointV1,
) (string, bool, error) {
	loaded, err := r.loadFrozenGraph(ctx, run, task)
	if err != nil {
		return "", false, err
	}
	for _, node := range loaded.graph.Nodes {
		if node.ID == checkpoint.NodeID && node.Type != machine.NodeWait {
			return "", false, executionError(
				ErrorCodeRuntimeIncompatible,
				fmt.Errorf("timer checkpoint node %q is not wait", node.ID),
			)
		}
	}
	for _, edge := range loaded.graph.Edges {
		if edge.FromNodeID == checkpoint.NodeID && edge.Route == machine.RouteTimeout {
			return edge.ToNodeID, true, nil
		}
	}
	return "", false, nil
}

type loadedWorkflowGraph struct {
	payload  frozen.ArtifactPayloadV1
	graph    machine.GraphDefinition
	envelope frozen.ArtifactEnvelopeV1
	// buildRunID and candidateHash identify the frozen candidate snapshot
	// when the run is an admin candidate test run; both are empty for
	// published-artifact runs.
	buildRunID    string
	candidateHash string
	// buildRoundNo is the team build round that produced the candidate. Only
	// round-bound candidate runs (buildRoundNo > 0) charge the TeamBuild
	// budget ledger from their measured usage; parallel/fanout legs and CLI
	// nodes without a usage receipt run normally and the run result is
	// annotated usage-complete=false. Generic candidate test runs and
	// published runs keep their existing behavior without annotation.
	buildRoundNo int
}

func (g loadedWorkflowGraph) roundBoundCandidate() bool {
	return g.buildRoundNo > 0
}

type preparedWorkflowRuntime struct {
	loadedWorkflowGraph
	artifact *workflow.RuntimeArtifact
	runInput any
}

func (r *WorkflowSerialRuntime) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now().UTC()
}

func (r *WorkflowSerialRuntime) workflowOutputRecorder(
	run TeamRun,
) func(context.Context, machine.Node, any, bool) error {
	if r == nil || r.OutputRecorder == nil {
		return nil
	}
	return func(ctx context.Context, node machine.Node, output any, final bool) error {
		agentID := ""
		if worker, ok := node.Config.(machine.WorkerConfig); ok {
			agentID = worker.AgentID
		}
		return r.OutputRecorder.RecordWorkflowOutput(ctx, deliverable.WorkflowOutput{
			WorkspaceID:   run.WorkspaceID,
			RunID:         run.RunID,
			RunSnapshotID: run.RunSnapshotID,
			NodeID:        node.ID,
			NodeLabel:     node.Label,
			NodeType:      string(node.Type),
			AgentID:       agentID,
			Output:        output,
			Final:         final,
			CreatedAt:     r.now(),
		})
	}
}

func (r *WorkflowSerialRuntime) loadFrozenGraph(
	ctx context.Context,
	run TeamRun,
	task *taskqueue.Task,
) (loadedWorkflowGraph, error) {
	if r == nil || r.Artifacts == nil || r.Loader == nil ||
		r.HostFactory == nil || r.CredentialResolvers == nil {
		return loadedWorkflowGraph{}, executionError(
			ErrorCodeRuntimeIncompatible,
			errors.New("workflow runtime dependencies are unavailable"),
		)
	}
	candidateHash := ""
	buildRunID := ""
	buildRoundNo := 0
	if r.Snapshots != nil {
		runSnapshot, err := r.Snapshots.GetByRunID(
			ctx, run.WorkspaceID, run.RunSnapshotID,
		)
		if err != nil {
			return loadedWorkflowGraph{}, executionError(
				ErrorCodeSnapshotUnavailable, err,
			)
		}
		if runSnapshot == nil ||
			runSnapshot.WorkspaceID != run.WorkspaceID ||
			runSnapshot.RunID != run.RunSnapshotID {
			return loadedWorkflowGraph{}, executionError(
				ErrorCodeIdentityMismatch,
				errors.New("runtime snapshot identity differs from TeamRun"),
			)
		}
		candidateHash = runSnapshot.CandidateContentHash
		buildRunID = runSnapshot.BuildRunID
		buildRoundNo = runSnapshot.BuildRoundNo
	}
	var artifact *workflow.PublishedArtifactContent
	var err error
	if candidateHash != "" {
		candidateReader, ok := r.Artifacts.(CandidateArtifactReader)
		if !ok {
			return loadedWorkflowGraph{}, executionError(
				ErrorCodeRuntimeIncompatible,
				errors.New("candidate artifact reader is unavailable"),
			)
		}
		artifact, err = candidateReader.GetCandidateArtifact(
			ctx,
			run.WorkspaceID,
			run.WorkflowID,
			run.WorkflowVersion,
			candidateHash,
		)
	} else {
		artifact, err = r.Artifacts.GetArtifact(
			ctx, run.WorkspaceID, run.WorkflowID, run.WorkflowVersion,
		)
	}
	if err != nil || artifact == nil {
		if err == nil {
			err = errors.New("frozen artifact is unavailable")
		}
		return loadedWorkflowGraph{}, executionError(ErrorCodeSnapshotUnavailable, err)
	}
	envelope := frozen.ArtifactEnvelopeV1{
		WorkspaceID:               artifact.WorkspaceID,
		WorkflowID:                artifact.WorkflowID,
		WorkflowVersion:           artifact.WorkflowVersion,
		ArtifactSchemaVersion:     artifact.ArtifactSchemaVersion,
		CanonicalizationAlgorithm: artifact.CanonicalizationAlgorithm,
		CanonicalizationVersion:   artifact.CanonicalizationVersion,
		HashAlgorithm:             artifact.HashAlgorithm,
		ContentHash:               artifact.ContentHash,
		Payload:                   artifact.Payload,
	}
	payload, err := frozen.DecodeArtifactEnvelopeV1(envelope)
	if err != nil {
		return loadedWorkflowGraph{}, executionError(ErrorCodeRuntimeIncompatible, err)
	}
	if envelope.WorkspaceID != run.WorkspaceID ||
		envelope.WorkflowID != run.WorkflowID ||
		envelope.WorkflowVersion != run.WorkflowVersion ||
		run.RunSnapshotID != task.RunSnapshotID {
		return loadedWorkflowGraph{}, executionError(
			ErrorCodeIdentityMismatch,
			errors.New("runtime artifact identity differs from TeamRun"),
		)
	}
	graph, report := machine.DecodeGraphDefinitionV1(payload.GraphDefinition)
	if report != nil && len(report.Issues) > 0 {
		return loadedWorkflowGraph{}, executionError(
			ErrorCodeRuntimeIncompatible,
			fmt.Errorf("decode frozen graph: %s", report.Issues[0].Code),
		)
	}
	return loadedWorkflowGraph{
		payload: payload, graph: graph, envelope: envelope,
		buildRunID: buildRunID, candidateHash: candidateHash,
		buildRoundNo: buildRoundNo,
	}, nil
}

// hostFactoryFor returns the runtime host factory for one loaded graph. A
// snapshot run whose runtime configures HostFactoryForSnapshot uses the
// replacement factory; every other run (including published-artifact runs)
// keeps HostFactory.
func (r *WorkflowSerialRuntime) hostFactoryFor(loaded loadedWorkflowGraph) workflow.RuntimeHostFactory {
	if r.HostFactoryForSnapshot != nil && loaded.candidateHash != "" {
		return r.HostFactoryForSnapshot(loaded.buildRunID, loaded.candidateHash)
	}
	return r.HostFactory
}

func (r *WorkflowSerialRuntime) prepare(
	ctx context.Context,
	run TeamRun,
	task *taskqueue.Task,
) (*preparedWorkflowRuntime, error) {
	loaded, err := r.loadFrozenGraph(ctx, run, task)
	if err != nil {
		return nil, err
	}
	resolver, err := r.CredentialResolvers(run.WorkspaceID)
	if err != nil {
		return nil, executionError(ErrorCodeRuntimeIncompatible, err)
	}
	loader := *r.Loader
	loader.RunSnapshotID = run.RunSnapshotID
	runtimeArtifact, err := loader.Load(ctx, loaded.envelope, r.hostFactoryFor(loaded), resolver)
	if err != nil {
		return nil, executionError(ErrorCodeRuntimeIncompatible, err)
	}
	runInputPayload := task.Payload
	// Continuation tasks carry an internal control envelope (for example a
	// fanout_resume claim), not the workflow's business input. The TeamRun's
	// immutable source task remains the canonical run_input for every graph
	// advance, including rework after a fanout checkpoint.
	if run.SourceTaskID != "" && task.ID != run.SourceTaskID {
		if r.Tasks == nil {
			runtimeArtifact.Close()
			return nil, executionError(
				ErrorCodeSnapshotUnavailable,
				errors.New("workflow source task reader is unavailable"),
			)
		}
		sourceTask, err := r.Tasks.Get(ctx, run.WorkspaceID, run.SourceTaskID)
		if err != nil {
			runtimeArtifact.Close()
			return nil, executionError(ErrorCodeSnapshotUnavailable, err)
		}
		runInputPayload = sourceTask.Payload
	}
	var runInput any
	if len(bytes.TrimSpace(runInputPayload)) == 0 {
		runInput = map[string]any{}
	} else if err := decodeJSONValue(runInputPayload, &runInput); err != nil {
		runtimeArtifact.Close()
		return nil, executionError(ErrorCodeRuntimeIncompatible, err)
	}
	return &preparedWorkflowRuntime{
		loadedWorkflowGraph: loaded,
		artifact:            runtimeArtifact,
		runInput:            runInput,
	}, nil
}

func decodeCheckpointOutputs(raw map[string]json.RawMessage) (map[string]any, error) {
	outputs := make(map[string]any, len(raw))
	for nodeID, encoded := range raw {
		var value any
		if err := decodeJSONValue(encoded, &value); err != nil {
			return nil, fmt.Errorf("decode checkpoint output for node %q: %w", nodeID, err)
		}
		outputs[nodeID] = value
	}
	return outputs, nil
}

func runtimeResultFromSerial(
	result serialMachineResult,
	payload frozen.ArtifactPayloadV1,
) (RuntimeResult, error) {
	switch result.Status {
	case serialCompleted:
		if len(payload.DeliveryTargets) != 0 {
			return RuntimeResult{Status: RuntimeFailed}, executionError(
				ErrorCodeDeliveryUnavailable,
				errors.New("delivery outbox is unavailable"),
			)
		}
		encoded, err := json.Marshal(map[string]any{"output": result.Output})
		if err != nil {
			return RuntimeResult{Status: RuntimeFailed}, executionError(ErrorCodeOutputInvalid, err)
		}
		return RuntimeResult{
			Status: RuntimeCompleted, Output: encoded, Usage: result.Usage.Totals(),
			UsageComplete:         result.UsageComplete,
			UsageIncompleteReason: result.UsageIncompleteReason,
		}, nil
	case serialParked:
		outputs := make(map[string]json.RawMessage, len(result.Outputs))
		for nodeID, output := range result.Outputs {
			encoded, err := json.Marshal(output)
			if err != nil {
				return RuntimeResult{Status: RuntimeFailed}, executionError(ErrorCodeRuntimeIncompatible, err)
			}
			outputs[nodeID] = encoded
		}
		usageCheckpoint, err := result.Usage.MarshalCheckpoint()
		if err != nil {
			return RuntimeResult{Status: RuntimeFailed}, executionError(
				ErrorCodeRuntimeIncompatible,
				fmt.Errorf("encode serial usage checkpoint: %w", err),
			)
		}
		return RuntimeResult{Status: RuntimeParked, Park: &RuntimePark{
			NodeID: result.NodeID, CompletedOutputs: outputs,
			WaitKind: result.WaitKind, WaitDetail: result.WaitDetail,
			UsageCheckpoint: usageCheckpoint,
			UsageComplete:   result.UsageComplete, UsageIncompleteReason: result.UsageIncompleteReason,
		}, Usage: result.Usage.Totals(),
			UsageComplete: result.UsageComplete, UsageIncompleteReason: result.UsageIncompleteReason}, nil
	case serialFailed:
		return RuntimeResult{
			Status: RuntimeFailed, Usage: result.Usage.Totals(),
			UsageComplete:         result.UsageComplete,
			UsageIncompleteReason: result.UsageIncompleteReason,
		}, result.Err
	default:
		return RuntimeResult{Status: RuntimeFailed}, executionError(
			ErrorCodeRuntimeIncompatible, errors.New("serial runtime returned an invalid status"),
		)
	}
}

type serialMachineStatus string

const (
	serialCompleted serialMachineStatus = "completed"
	serialParked    serialMachineStatus = "parked"
	serialFailed    serialMachineStatus = "failed"
)

type serialMachineStart struct {
	NodeID       string
	Outputs      map[string]any
	SourceKind   SourceKind
	Now          time.Time
	Run          TeamRun
	ArtifactHash string
	// Candidate marks a round-bound team build candidate test run (snapshot
	// build_round_no > 0). These runs charge the TeamBuild budget ledger from
	// their measured usage only; parallel/fanout legs and CLI nodes without a
	// usage receipt run normally and the result is annotated usage-incomplete
	// instead of failing closed.
	Candidate bool
	// Usage seeds the machine's logical usage accumulator on resume so
	// park/resume never loses or duplicates confirmed usage.
	Usage loomruntime.UsageAccumulator
	// UsageComplete seeds the annotation across park/resume: false plus
	// UsageIncompleteReason when an earlier segment already hit an unmeasured
	// node, so the final terminal result still says so.
	UsageComplete         bool
	UsageIncompleteReason string
	RecordOutput          func(context.Context, machine.Node, any, bool) error
}

type serialMachineResult struct {
	Status                serialMachineStatus
	Output                any
	Outputs               map[string]any
	NodeID                string
	WaitKind              WaitKind
	WaitDetail            json.RawMessage
	Usage                 loomruntime.UsageAccumulator
	UsageComplete         bool
	UsageIncompleteReason string
	Err                   error
}

func serialFailure(err error, usage loomruntime.UsageAccumulator) serialMachineResult {
	return serialMachineResult{Status: serialFailed, Usage: usage, Err: err}
}

// nodeUsageAttemptID derives one deterministic physical attempt id per node
// invocation from the logical call id, so a replay of the same invocation
// confirms idempotently instead of creating a second contribution.
func nodeUsageAttemptID(callID string) string {
	digest := sha256.Sum256([]byte("teamrun-node-attempt:" + callID))
	return "ua1_" + hex.EncodeToString(digest[:16])
}

func runSerialMachine(
	ctx context.Context,
	graph machine.GraphDefinition,
	payload frozen.ArtifactPayloadV1,
	artifact *workflow.RuntimeArtifact,
	runInput any,
	start serialMachineStart,
) serialMachineResult {
	usage := start.Usage
	// A fresh or legacy start is usage-complete; only an explicit
	// usage_incomplete_reason (persisted across park/resume) marks the run
	// incomplete. The flag is always paired with a non-empty reason.
	usageComplete := true
	usageIncompleteReason := start.UsageIncompleteReason
	if usageIncompleteReason != "" {
		usageComplete = start.UsageComplete
	}
	fail := func(err error) serialMachineResult {
		return serialMachineResult{
			Status: serialFailed, Usage: usage, Err: err,
			UsageComplete: usageComplete, UsageIncompleteReason: usageIncompleteReason,
		}
	}
	nodes := make(map[string]machine.Node, len(graph.Nodes))
	deliverCount := 0
	for _, node := range graph.Nodes {
		nodes[node.ID] = node
		if node.Type == machine.NodeDeliver {
			deliverCount++
		}
	}
	if deliverCount < 1 {
		return fail(executionError(
			ErrorCodeRuntimeIncompatible,
			errors.New("serial workflow has no deliver node"),
		))
	}
	edges := make(map[string][]machine.Edge)
	for _, edge := range graph.Edges {
		edges[edge.FromNodeID] = append(edges[edge.FromNodeID], edge)
	}
	for nodeID := range edges {
		sort.SliceStable(edges[nodeID], func(i, j int) bool {
			left, right := edges[nodeID][i], edges[nodeID][j]
			if left.Priority == nil {
				return false
			}
			if right.Priority == nil {
				return true
			}
			return *left.Priority < *right.Priority
		})
	}
	entries := make(map[string]workflow.RuntimeGraphEntry, len(artifact.Entries))
	for _, entry := range artifact.Entries {
		entries[runtimeEntryKey(entry.AgentID, entry.AgentVersion)] = entry
	}

	outputs := make(map[string]any, len(start.Outputs))
	for nodeID, output := range start.Outputs {
		outputs[nodeID] = output
	}
	current := start.NodeID
	if current == "" {
		current = graph.EntryNodeID
	}
	stepBudget := serialMachineStepBudget(graph)
	for steps := 0; current != ""; steps++ {
		if steps >= stepBudget {
			return fail(executionError(
				ErrorCodeRuntimeIncompatible,
				errors.New("serial workflow exceeded bounded topology"),
			))
		}
		node, present := nodes[current]
		if !present {
			return fail(executionError(
				ErrorCodeRuntimeIncompatible,
				fmt.Errorf("node %q is unavailable", current),
			))
		}
		switch node.Type {
		case machine.NodeLead, machine.NodeWorker:
			callID, err := usage.NextCall(start.Run.RunID, node.ID)
			if err != nil {
				return fail(executionError(ErrorCodeExecutionUnrecoverable, err))
			}
			attemptID := nodeUsageAttemptID(callID)
			if err := usage.StartAttempt(callID, attemptID); err != nil {
				return fail(executionError(ErrorCodeExecutionUnrecoverable, err))
			}
			output, nodeUsage, nodeUsageComplete, err := runAgentNode(
				ctx, node, payload, entries, runInput, outputs,
			)
			if confirmErr := usage.ConfirmAttempt(callID, attemptID, contract.Usage{
				InputTokens:  nodeUsage.InputTokens,
				OutputTokens: nodeUsage.OutputTokens,
				CostUSD:      nodeUsage.CostUSD,
			}); confirmErr != nil {
				return fail(executionError(ErrorCodeExecutionUnrecoverable, confirmErr))
			}
			// A CLI runtime agent has no usage receipt: the candidate run
			// executes it normally, records zero measured usage, and marks
			// the result usage-incomplete instead of failing closed.
			if start.Candidate && !nodeUsageComplete && usageIncompleteReason == "" {
				usageComplete = false
				usageIncompleteReason = UsageIncompleteReasonCLINode
			}
			if err != nil {
				next, routed := edgeTarget(edges[current], machine.RouteFailure)
				if routed {
					current = next
					continue
				}
				return fail(err)
			}
			outputs[node.ID] = output
			if start.RecordOutput != nil {
				if err := start.RecordOutput(ctx, node, output, false); err != nil {
					return fail(executionError(ErrorCodeDeliveryUnavailable, err))
				}
			}
			if next, routed := edgeTarget(edges[current], machine.RouteBack); routed {
				// A worker may itself be the machine-native loop latch. The
				// validated graph gives that latch a back edge (not a success
				// edge), so return to the loop header after preserving its output.
				current = next
				continue
			}
			next, ok := edgeTarget(edges[current], machine.RouteSuccess)
			if !ok {
				return fail(executionError(
					ErrorCodeRuntimeIncompatible,
					fmt.Errorf("node %q lacks a success edge", node.ID),
				))
			}
			current = next
		case machine.NodeTransform:
			output, err := runTransformNode(node, runInput, outputs)
			if err != nil {
				next, routed := edgeTarget(edges[current], machine.RouteFailure)
				if routed {
					current = next
					continue
				}
				return fail(executionError(ErrorCodeExecutionUnrecoverable, err))
			}
			outputs[node.ID] = output
			if start.RecordOutput != nil {
				if err := start.RecordOutput(ctx, node, output, false); err != nil {
					return fail(executionError(ErrorCodeDeliveryUnavailable, err))
				}
			}
			if next, routed := edgeTarget(edges[current], machine.RouteBack); routed {
				// A transform acting as a machine-native loop latch returns
				// to its loop header through the back edge.
				current = next
				continue
			}
			current, _ = edgeTarget(edges[current], machine.RouteSuccess)
		case machine.NodeCondition:
			next, err := conditionTarget(edges[current], runInput, outputs)
			if err != nil {
				failure, routed := edgeTarget(edges[current], machine.RouteFailure)
				if routed {
					current = failure
					continue
				}
				return fail(executionError(ErrorCodeExecutionUnrecoverable, err))
			}
			current = next
		case machine.NodeLoop:
			next, err := stepLoopNode(node, runInput, outputs, edges[current])
			if err != nil {
				return fail(err)
			}
			if output, present := outputs[node.ID]; present && start.RecordOutput != nil {
				if err := start.RecordOutput(ctx, node, output, false); err != nil {
					return fail(executionError(ErrorCodeDeliveryUnavailable, err))
				}
			}
			current = next
		case machine.NodeParallel:
			// Round-bound candidate runs proceed through fanout like any
			// other run; leg-level usage settlement is owned by T14B-2B, so
			// the measured serial/loop usage is annotated usage-incomplete
			// (unmeasured parallel legs) instead of failing closed.
			if start.Candidate && usageIncompleteReason == "" {
				usageComplete = false
				usageIncompleteReason = UsageIncompleteReasonParallelLegs
			}
			plan, joinNodeID, err := buildFanoutParkPlan(
				node, nodes, edges[current], payload, runInput, outputs, start,
			)
			if err != nil {
				return fail(err)
			}
			detail, err := json.Marshal(plan)
			if err != nil {
				return fail(executionError(ErrorCodeRuntimeIncompatible, err))
			}
			return serialMachineResult{
				Status: serialParked, Outputs: outputs, NodeID: joinNodeID,
				WaitKind: WaitFanout, WaitDetail: detail, Usage: usage,
				UsageComplete: usageComplete, UsageIncompleteReason: usageIncompleteReason,
			}
		case machine.NodeJoin:
			projection, present := outputs[node.ID]
			if !present {
				return fail(executionError(
					ErrorCodeRuntimeIncompatible,
					fmt.Errorf("join node %q has no fanout projection", node.ID),
				))
			}
			encoded, err := json.Marshal(projection)
			if err != nil {
				return fail(executionError(ErrorCodeRuntimeIncompatible, err))
			}
			var projected fanoutJoinProjectionV1
			if err := decodeExact(encoded, &projected); err != nil || projected.SchemaVersion != 1 ||
				(projected.Decision != "succeeded" && projected.Decision != "failed") ||
				projected.Results == nil || projected.Errors == nil {
				return fail(executionError(
					ErrorCodeRuntimeIncompatible,
					fmt.Errorf("join node %q projection is invalid", node.ID),
				))
			}
			if start.RecordOutput != nil {
				if err := start.RecordOutput(ctx, node, projection, false); err != nil {
					return fail(executionError(ErrorCodeDeliveryUnavailable, err))
				}
			}
			route := machine.RouteSuccess
			if projected.Decision == "failed" {
				route = machine.RouteFailure
			}
			next, ok := edgeTarget(edges[current], route)
			if !ok {
				detail := fmt.Sprintf("join node %q lacks %s edge", node.ID, route)
				if route == machine.RouteFailure && len(projected.Errors) > 0 {
					keys := make([]string, 0, len(projected.Errors))
					for key := range projected.Errors {
						keys = append(keys, key)
					}
					sort.Strings(keys)
					failures := make([]string, 0, len(keys))
					for _, key := range keys {
						failures = append(failures, key+"="+projected.Errors[key])
					}
					detail += ": " + strings.Join(failures, "; ")
				}
				return fail(executionError(
					ErrorCodeExecutionUnrecoverable,
					errors.New(detail),
				))
			}
			current = next
		case machine.NodeDeliver:
			config, ok := node.Config.(machine.DeliverConfig)
			if !ok {
				return fail(executionError(
					ErrorCodeRuntimeIncompatible,
					fmt.Errorf("deliver node %q config is invalid", node.ID),
				))
			}
			output, err := resolveValue(config.Result, runInput, outputs)
			if err != nil {
				return fail(executionError(ErrorCodeOutputInvalid, err))
			}
			encoded, err := json.Marshal(output)
			if err != nil {
				return fail(executionError(ErrorCodeOutputInvalid, err))
			}
			if _, problems := machine.ValidateRuntimeOutput(graph.OutputContract, encoded); len(problems) != 0 {
				return fail(executionError(
					ErrorCodeOutputInvalid,
					fmt.Errorf("deliver output violates contract: %s", problems[0].Code),
				))
			}
			if start.RecordOutput != nil {
				if err := start.RecordOutput(ctx, node, output, true); err != nil {
					return fail(executionError(ErrorCodeDeliveryUnavailable, err))
				}
			}
			return serialMachineResult{
				Status: serialCompleted, Output: output, Usage: usage,
				UsageComplete: usageComplete, UsageIncompleteReason: usageIncompleteReason,
			}
		case machine.NodeWait:
			config, ok := node.Config.(machine.WaitConfig)
			if !ok || config.TimeoutSeconds == nil || *config.TimeoutSeconds < 1 ||
				start.SourceKind != SourceSession {
				return fail(executionError(
					ErrorCodeUnexpectedInteractiveYield,
					fmt.Errorf("interactive wait node %q reached in no-session workflow", node.ID),
				))
			}
			now := start.Now
			if now.IsZero() {
				now = time.Now().UTC()
			}
			detail, err := json.Marshal(map[string]any{
				"wake_at": now.Add(time.Duration(*config.TimeoutSeconds) * time.Second).UTC(),
				"node_id": node.ID,
			})
			if err != nil {
				return fail(executionError(ErrorCodeRuntimeIncompatible, err))
			}
			return serialMachineResult{
				Status: serialParked, Outputs: outputs, NodeID: node.ID,
				WaitKind: WaitTimer, WaitDetail: detail, Usage: usage,
				UsageComplete: usageComplete, UsageIncompleteReason: usageIncompleteReason,
			}
		case machine.NodeHandoff:
			return fail(executionError(
				ErrorCodeUnexpectedInteractiveYield,
				fmt.Errorf("handoff node %q is outside fixed workflow execution", node.ID),
			))
		default:
			return fail(executionError(
				ErrorCodeRuntimeIncompatible,
				fmt.Errorf("node %q type %q is outside minimal serial execution", node.ID, node.Type),
			))
		}
	}
	return fail(executionError(
		ErrorCodeOutputInvalid,
		errors.New("serial workflow ended without deliver"),
	))
}

func serialMachineStepBudget(graph machine.GraphDefinition) int {
	nodeCount := len(graph.Nodes)
	if nodeCount < 1 {
		return 1
	}
	loopIterations := int64(0)
	for _, node := range graph.Nodes {
		if node.Type != machine.NodeLoop {
			continue
		}
		config, ok := node.Config.(machine.LoopConfig)
		if !ok || config.MaxIterations < 1 {
			loopIterations++
			continue
		}
		loopIterations += config.MaxIterations
	}
	if loopIterations < 1 {
		return nodeCount
	}
	maxInt := int64(^uint(0) >> 1)
	budget := int64(nodeCount) * (1 + loopIterations)
	if budget > maxInt {
		return int(maxInt)
	}
	return int(budget)
}

type runtimeJoinPolicy struct {
	Kind               string    `json:"kind"`
	Quorum             int       `json:"quorum,omitempty"`
	DeadlineAt         time.Time `json:"deadline_at"`
	MaxDeadlineSeconds int64     `json:"max_deadline_seconds,omitempty"`
}

type fanoutJoinProjectionV1 struct {
	SchemaVersion int                        `json:"schema_version"`
	Decision      string                     `json:"decision"`
	Results       map[string]json.RawMessage `json:"results"`
	Errors        map[string]string          `json:"errors"`
	// Legs mirrors the machine-declared join output shape
	// ({decision, policy, legs[]}) so condition predicates can reference
	// per-branch results through a machine-valid JSON pointer. Fanout resume
	// (fanout_seams.go) fills it; serial join decoding keeps reading
	// Results/Errors for routing and only tolerates the extra field.
	Legs []fanoutJoinLegV1 `json:"legs,omitempty"`
}

type fanoutJoinLegV1 struct {
	NodeID              string          `json:"node_id"`
	DecisionDisposition string          `json:"decision_disposition"`
	Result              json.RawMessage `json:"result"`
	Error               json.RawMessage `json:"error"`
}

func buildFanoutParkPlan(
	parallel machine.Node,
	nodes map[string]machine.Node,
	branchEdges []machine.Edge,
	payload frozen.ArtifactPayloadV1,
	runInput any,
	outputs map[string]any,
	start serialMachineStart,
) (FanoutPrepareRequest, string, error) {
	config, ok := parallel.Config.(machine.ParallelConfig)
	if !ok || config.JoinNodeID == "" {
		return FanoutPrepareRequest{}, "", executionError(
			ErrorCodeRuntimeIncompatible, fmt.Errorf("parallel node %q config is invalid", parallel.ID),
		)
	}
	joinNode, ok := nodes[config.JoinNodeID]
	if !ok || joinNode.Type != machine.NodeJoin {
		return FanoutPrepareRequest{}, "", executionError(
			ErrorCodeRuntimeIncompatible, fmt.Errorf("parallel node %q join is unavailable", parallel.ID),
		)
	}
	joinConfig, ok := joinNode.Config.(machine.JoinConfig)
	if !ok {
		return FanoutPrepareRequest{}, "", executionError(
			ErrorCodeRuntimeIncompatible, fmt.Errorf("join node %q config is invalid", joinNode.ID),
		)
	}
	now := start.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	deadlineSeconds := int64(86400)
	if joinConfig.DeadlineSeconds != nil {
		deadlineSeconds = *joinConfig.DeadlineSeconds
	}
	policy := runtimeJoinPolicy{
		Kind: string(joinConfig.Policy), DeadlineAt: now.Add(time.Duration(deadlineSeconds) * time.Second),
		MaxDeadlineSeconds: deadlineSeconds,
	}
	if joinConfig.SuccessCount != nil {
		policy.Quorum = int(*joinConfig.SuccessCount)
	}
	encodedPolicy, err := json.Marshal(policy)
	if err != nil {
		return FanoutPrepareRequest{}, "", executionError(ErrorCodeRuntimeIncompatible, err)
	}
	bundles := make(map[string]frozen.FrozenExecutionBundle, len(payload.Bundles))
	for _, bundle := range payload.Bundles {
		bundles[runtimeEntryKey(bundle.Agent.AgentID, bundle.Agent.AgentVersion)] = bundle
	}
	legs := make([]FanoutLegPlan, 0)
	for _, edge := range branchEdges {
		if edge.Route != machine.RouteBranch {
			continue
		}
		branch, present := nodes[edge.ToNodeID]
		worker, validWorker := branch.Config.(machine.WorkerConfig)
		if !present || branch.Type != machine.NodeWorker || !validWorker || worker.Kind != machine.WorkerDispatch {
			return FanoutPrepareRequest{}, "", executionError(
				ErrorCodeRuntimeIncompatible, fmt.Errorf("parallel branch %q is not a dispatch worker", edge.ToNodeID),
			)
		}
		bundle, present := bundles[runtimeEntryKey(worker.AgentID, worker.AgentVersion)]
		if !present || bundle.Capability.MayYield {
			return FanoutPrepareRequest{}, "", executionError(
				ErrorCodeUnexpectedInteractiveYield, fmt.Errorf("parallel branch %q lacks may_yield=false bundle", branch.ID),
			)
		}
		bundleHash, err := frozen.HashDTO(bundle, frozen.PreorderFrozenExecutionBundle)
		if err != nil {
			return FanoutPrepareRequest{}, "", executionError(ErrorCodeRuntimeIncompatible, err)
		}
		bindings, err := frozen.CanonicalizePreordered(branch.Inputs)
		if err != nil {
			return FanoutPrepareRequest{}, "", executionError(ErrorCodeRuntimeIncompatible, err)
		}
		bindingsHash := sha256.Sum256(bindings)
		bundleRef, _ := json.Marshal(map[string]any{
			"workspace_id": start.Run.WorkspaceID, "workflow_id": start.Run.WorkflowID,
			"workflow_version": start.Run.WorkflowVersion, "run_snapshot_id": start.Run.RunSnapshotID,
			"artifact_content_hash": start.ArtifactHash, "branch_id": branch.ID,
			"agent_id": worker.AgentID, "agent_version": worker.AgentVersion,
			"bundle_content_hash": bundleHash,
		})
		inputRef, _ := json.Marshal(map[string]any{
			"node_id": branch.ID, "checkpoint_sequence": int64(start.Run.ResumeGeneration),
			"bindings_content_hash": hex.EncodeToString(bindingsHash[:]),
		})
		proof, _ := json.Marshal(map[string]any{
			"validator_version": machine.SchemaVersionV1, "validated_bundle_hash": bundleHash,
			"may_yield": false,
		})
		legHash := sha256.Sum256([]byte(start.Run.RunID + "\x00" + parallel.ID + "\x00" + branch.ID +
			"\x00" + fmt.Sprint(start.Run.ResumeGeneration)))
		legs = append(legs, FanoutLegPlan{
			LegID: "fl1_" + hex.EncodeToString(legHash[:]), BranchID: branch.ID,
			BranchOrdinal: len(legs), FrozenBundleRef: bundleRef, InputRef: inputRef, MayYieldProof: proof,
		})
	}
	if len(legs) < 2 || start.Run.WorkspaceID == "" || start.Run.RunID == "" ||
		start.Run.ExecutionLeaseEpoch < 1 {
		return FanoutPrepareRequest{}, "", executionError(
			ErrorCodeRuntimeIncompatible, errors.New("parallel runtime identity or branches are invalid"),
		)
	}
	_ = runInput
	_ = outputs
	return FanoutPrepareRequest{
		WorkspaceID: start.Run.WorkspaceID, ParentRunID: start.Run.RunID,
		WorkflowID: start.Run.WorkflowID, WorkflowVersion: int64(start.Run.WorkflowVersion),
		RunSnapshotID: start.Run.RunSnapshotID, NodeID: parallel.ID,
		PreviousCheckpointSequence: int64(start.Run.ResumeGeneration), NodeEntryOrdinal: 0,
		CreatorEpoch:             int64(start.Run.ExecutionLeaseEpoch),
		CreatorAttemptGeneration: int64(start.Run.ExecutionLeaseEpoch),
		CreatorAttemptID:         frozenAttemptID(start.Run).String(),
		ActivationDeadline:       now.Add(30 * time.Second), JoinPolicy: encodedPolicy, Legs: legs,
	}, config.JoinNodeID, nil
}

func runtimeEntryKey(agentID string, version int64) string {
	return fmt.Sprintf("%s\x00%d", agentID, version)
}

func runAgentNode(
	ctx context.Context,
	node machine.Node,
	payload frozen.ArtifactPayloadV1,
	entries map[string]workflow.RuntimeGraphEntry,
	runInput any,
	outputs map[string]any,
) (any, loomruntime.UsageTotals, bool, error) {
	// usageComplete reports whether the executed node produced a measurable
	// usage receipt: frozen graphs confirm logical usage, while CLI runtime
	// agents have no receipt and contribute zero measured usage.
	usageComplete := true
	var (
		agentID      string
		agentVersion int64
		instruction  string
	)
	switch config := node.Config.(type) {
	case machine.LeadConfig:
		agentID = payload.Team.LeadAgentID
		instruction = config.Instruction
		for _, bundle := range payload.Bundles {
			if bundle.Agent.AgentID == agentID {
				if agentVersion != 0 {
					return nil, loomruntime.UsageTotals{}, false, executionError(
						ErrorCodeRuntimeIncompatible,
						fmt.Errorf("lead agent %q has multiple bundles", agentID),
					)
				}
				agentVersion = bundle.Agent.AgentVersion
			}
		}
	case machine.WorkerConfig:
		agentID = config.AgentID
		agentVersion = config.AgentVersion
		instruction = config.ResultRequirement
	default:
		return nil, loomruntime.UsageTotals{}, false, executionError(
			ErrorCodeRuntimeIncompatible,
			fmt.Errorf("agent node %q config is invalid", node.ID),
		)
	}
	entry, ok := entries[runtimeEntryKey(agentID, agentVersion)]
	if !ok || (entry.Graph == nil && entry.CLI == nil) || (entry.Graph != nil && entry.CLI != nil) {
		return nil, loomruntime.UsageTotals{}, false, executionError(
			ErrorCodeRuntimeIncompatible,
			fmt.Errorf("frozen runtime entry for node %q is unavailable or ambiguous", node.ID),
		)
	}
	inputs := make(map[string]any, len(node.Inputs))
	for name, binding := range node.Inputs {
		value, err := resolveValue(binding.Value, runInput, outputs)
		if err != nil {
			return nil, loomruntime.UsageTotals{}, false, executionError(ErrorCodeExecutionUnrecoverable, err)
		}
		inputs[name] = value
	}
	encodedInputs, err := json.Marshal(inputs)
	if err != nil {
		return nil, loomruntime.UsageTotals{}, false, executionError(ErrorCodeExecutionUnrecoverable, err)
	}
	prompt := instruction + "\n\nInputs:\n" + string(encodedInputs)
	nodeTimeout := agentNodeExecutionTimeout
	nodeCtx := ctx
	cancel := func() {}
	if nodeTimeout > 0 {
		nodeCtx, cancel = context.WithTimeout(ctx, nodeTimeout)
	}
	defer cancel()
	timeoutErr := func() error {
		if nodeTimeout > 0 && errors.Is(nodeCtx.Err(), context.DeadlineExceeded) {
			return executionError(
				ErrorCodeExecutionUnrecoverable,
				fmt.Errorf("agent node %q timed out after %s", node.ID, nodeTimeout),
			)
		}
		return nil
	}
	if entry.CLI != nil {
		type cliOutcome struct {
			output string
			err    error
		}
		outcomes := make(chan cliOutcome, 1)
		go func() {
			output, err := entry.CLI.Execute(nodeCtx, prompt)
			outcomes <- cliOutcome{output: output, err: err}
		}()
		var outcome cliOutcome
		select {
		case outcome = <-outcomes:
		case <-nodeCtx.Done():
			if timeout := timeoutErr(); timeout != nil {
				return nil, loomruntime.UsageTotals{}, false, timeout
			}
			return nil, loomruntime.UsageTotals{}, false, executionError(ErrorCodeExecutionUnrecoverable, nodeCtx.Err())
		}
		if timeout := timeoutErr(); timeout != nil {
			return nil, loomruntime.UsageTotals{}, false, timeout
		}
		if outcome.err != nil {
			return nil, loomruntime.UsageTotals{}, false, executionError(ErrorCodeExecutionUnrecoverable, outcome.err)
		}
		normalizedOutput, err := normalizeAgentNodeOutput(node, outcome.output)
		if err != nil {
			return nil, loomruntime.UsageTotals{}, false, err
		}
		// A CLI execution has no usage receipt: report zero measured usage
		// and usage_complete=false so callers never mistake it for full usage.
		return normalizedOutput, loomruntime.UsageTotals{}, false, nil
	}
	graphState := loom.State{
		"messages":          []contract.Message{{Role: "user", Content: prompt}},
		"last_user_message": prompt,
		"input":             inputs,
	}
	graphState["__run_id"] = uuid.NewString()
	if err := loomruntime.StoreUsageAccumulator(
		graphState,
		loomruntime.NewUsageAccumulator(),
	); err != nil {
		return nil, loomruntime.UsageTotals{}, false, executionError(
			ErrorCodeExecutionUnrecoverable,
			fmt.Errorf("initialize frozen graph usage for node %q: %w", node.ID, err),
		)
	}
	execCtx := loomruntime.WithUsageRunScope(nodeCtx)
	// Pre-bind the usage scope so hook-less frozen descriptors still confirm
	// usage into graph state; descriptors that install before-step hooks
	// rebind to the real step name on their first step.
	if err := loomruntime.BindUsageBeforeStep(execCtx, "graph_entry", graphState); err != nil {
		return nil, loomruntime.UsageTotals{}, false, executionError(
			ErrorCodeExecutionUnrecoverable,
			fmt.Errorf("bind frozen graph usage for node %q: %w", node.ID, err),
		)
	}
	type graphOutcome struct {
		result *loom.RunResult
		err    error
	}
	graphOutcomes := make(chan graphOutcome, 1)
	go func() {
		result, err := entry.Graph.Run(execCtx, graphState, loom.NewMemStore())
		graphOutcomes <- graphOutcome{result: result, err: err}
	}()
	var graphResult graphOutcome
	select {
	case graphResult = <-graphOutcomes:
	case <-nodeCtx.Done():
		if timeout := timeoutErr(); timeout != nil {
			return nil, loomruntime.UsageTotals{}, usageComplete, timeout
		}
		return nil, loomruntime.UsageTotals{}, usageComplete, executionError(ErrorCodeExecutionUnrecoverable, nodeCtx.Err())
	}
	result, err := graphResult.result, graphResult.err
	usage, usageErr := frozenNodeUsage(result)
	if usageErr != nil {
		return nil, loomruntime.UsageTotals{}, false, executionError(
			ErrorCodeExecutionUnrecoverable,
			fmt.Errorf("read frozen graph usage for node %q: %w", node.ID, usageErr),
		)
	}
	if err != nil {
		return nil, usage, usageComplete, executionError(ErrorCodeExecutionUnrecoverable, err)
	}
	if result == nil {
		return nil, usage, usageComplete, executionError(
			ErrorCodeExecutionUnrecoverable,
			errors.New("frozen graph returned no result"),
		)
	}
	if result.Yielded || result.StopReason == loom.StopYielded {
		return nil, usage, usageComplete, executionError(
			ErrorCodeUnexpectedInteractiveYield,
			fmt.Errorf("frozen graph for node %q yielded", node.ID),
		)
	}
	if result.StopReason != loom.StopCompleted {
		return nil, usage, usageComplete, executionError(
			ErrorCodeExecutionUnrecoverable,
			fmt.Errorf("frozen graph stopped with %q", result.StopReason),
		)
	}
	output, present := result.State["output"]
	if !present {
		return nil, usage, usageComplete, executionError(
			ErrorCodeOutputInvalid,
			fmt.Errorf("frozen graph for node %q returned no output", node.ID),
		)
	}
	normalizedOutput, err := normalizeAgentNodeOutput(node, output)
	if err != nil {
		return nil, usage, usageComplete, err
	}
	return normalizedOutput, usage, usageComplete, nil
}

// frozenNodeUsage extracts the confirmed logical usage a frozen graph run
// accumulated into its final state. A nil or incomplete result yields zero
// usage (nothing was observed), never an error.
func frozenNodeUsage(result *loom.RunResult) (loomruntime.UsageTotals, error) {
	if result == nil || result.State == nil {
		return loomruntime.UsageTotals{}, nil
	}
	accumulator, err := loomruntime.LoadUsageAccumulator(result.State)
	if err != nil {
		return loomruntime.UsageTotals{}, err
	}
	return accumulator.Totals(), nil
}

// validateAgentNodeOutput enforces the node-level Output contract at runtime.
// It is deliberately scoped: only agent nodes that declare an Output contract
// with a frozen schema are validated, so existing teams without node schemas
// keep their previous behavior exactly. The validated encoding is the same
// shape runAgentNode stores in outputs (and fanout leg results), so a node
// that passes this gate is safe for downstream condition/deliver evaluation.
func normalizeAgentNodeOutput(node machine.Node, output any) (any, error) {
	if node.Output != nil && node.Output.Type == machine.ValueJSON {
		if encoded, ok := output.(string); ok {
			decoder := json.NewDecoder(strings.NewReader(encoded))
			decoder.UseNumber()
			var decoded any
			if err := decoder.Decode(&decoded); err != nil {
				return nil, executionError(
					ErrorCodeNodeOutputInvalid,
					fmt.Errorf("node %q JSON output cannot be decoded: %w", node.ID, err),
				)
			}
			if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
				if err == nil {
					err = errors.New("multiple JSON values")
				}
				return nil, executionError(
					ErrorCodeNodeOutputInvalid,
					fmt.Errorf("node %q JSON output has trailing content: %w", node.ID, err),
				)
			}
			output = decoded
		}
	}
	if err := validateAgentNodeOutput(node, output); err != nil {
		return nil, err
	}
	return output, nil
}

func validateAgentNodeOutput(node machine.Node, output any) error {
	if node.Output == nil || len(node.Output.Schema) == 0 {
		return nil
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		return executionError(
			ErrorCodeNodeOutputInvalid,
			fmt.Errorf("node %q output cannot be encoded: %w", node.ID, err),
		)
	}
	if _, problems := machine.ValidateRuntimeInput(node.Output.Schema, encoded); len(problems) != 0 {
		return executionError(
			ErrorCodeNodeOutputInvalid,
			fmt.Errorf("node %q output violates contract: %s", node.ID, problems[0].Code),
		)
	}
	return nil
}

func runTransformNode(
	node machine.Node,
	runInput any,
	outputs map[string]any,
) (any, error) {
	config, ok := node.Config.(machine.TransformConfig)
	if !ok {
		return nil, fmt.Errorf("transform node %q config is invalid", node.ID)
	}
	switch config.Operation {
	case machine.TransformIdentity:
		if config.Value == nil {
			return nil, errors.New("identity transform value is missing")
		}
		return resolveValue(*config.Value, runInput, outputs)
	case machine.TransformObject:
		result := make(map[string]any, len(config.Fields))
		for name, ref := range config.Fields {
			value, err := resolveValue(ref, runInput, outputs)
			if err != nil {
				return nil, err
			}
			result[name] = value
		}
		return result, nil
	case machine.TransformArray:
		result := make([]any, 0, len(config.Items))
		for _, ref := range config.Items {
			value, err := resolveValue(ref, runInput, outputs)
			if err != nil {
				return nil, err
			}
			result = append(result, value)
		}
		return result, nil
	default:
		return nil, fmt.Errorf("transform operation %q is unsupported", config.Operation)
	}
}

const loopStateKeyPrefix = "__teamrun_loop_state:"

// stepLoopNode implements the machine-native loop header semantics of the
// minimal serial runtime: the body edge always runs once; each arrival
// through the latch back edge re-evaluates the loop's continue predicate and
// the iteration cap, then either enters the body again or exits through the
// exit edge. On exit the loop node publishes its derived output
// {iteration_count, limit_reached, latch_result} so downstream nodes can read
// it through machine-valid value references.
func stepLoopNode(
	node machine.Node,
	runInput any,
	outputs map[string]any,
	edges []machine.Edge,
) (string, error) {
	config, ok := node.Config.(machine.LoopConfig)
	if !ok {
		return "", executionError(
			ErrorCodeRuntimeIncompatible,
			fmt.Errorf("loop node %q config is invalid", node.ID),
		)
	}
	completed := 0
	if raw, present := outputs[loopStateKeyPrefix+node.ID]; present {
		completed = readLoopIterationCount(raw) + 1
	}
	if completed > 0 {
		continueLoop, err := evaluatePredicate(config.ContinuePredicate, runInput, outputs)
		if err != nil {
			return "", executionError(
				ErrorCodeExecutionUnrecoverable,
				fmt.Errorf("loop node %q continue predicate: %w", node.ID, err),
			)
		}
		limitReached := completed >= int(config.MaxIterations)
		if !continueLoop || limitReached {
			outputs[node.ID] = map[string]any{
				"iteration_count": completed,
				"limit_reached":   limitReached,
				"latch_result":    outputs[config.LatchNodeID],
			}
			next, routed := edgeTarget(edges, machine.RouteExit)
			if !routed {
				return "", executionError(
					ErrorCodeRuntimeIncompatible,
					fmt.Errorf("loop node %q lacks an exit edge", node.ID),
				)
			}
			return next, nil
		}
	}
	outputs[loopStateKeyPrefix+node.ID] = map[string]any{"iteration_count": completed}
	next, routed := edgeTarget(edges, machine.RouteBody)
	if !routed {
		return "", executionError(
			ErrorCodeRuntimeIncompatible,
			fmt.Errorf("loop node %q lacks a body edge", node.ID),
		)
	}
	return next, nil
}

func readLoopIterationCount(raw any) int {
	switch value := raw.(type) {
	case map[string]any:
		return readLoopIterationCount(value["iteration_count"])
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	case json.Number:
		if parsed, err := value.Int64(); err == nil {
			return int(parsed)
		}
	}
	return 0
}

func conditionTarget(
	edges []machine.Edge,
	runInput any,
	outputs map[string]any,
) (string, error) {
	var defaultTarget string
	for _, edge := range edges {
		switch edge.Route {
		case machine.RouteDefault:
			defaultTarget = edge.ToNodeID
		case machine.RouteCase:
			if edge.Predicate == nil {
				return "", errors.New("condition case predicate is missing")
			}
			matched, err := evaluatePredicate(*edge.Predicate, runInput, outputs)
			if err != nil {
				return "", err
			}
			if matched {
				return edge.ToNodeID, nil
			}
		}
	}
	if defaultTarget == "" {
		return "", errors.New("condition default edge is missing")
	}
	return defaultTarget, nil
}

func evaluatePredicate(
	predicate machine.Predicate,
	runInput any,
	outputs map[string]any,
) (bool, error) {
	left, leftErr := resolveValue(predicate.Left, runInput, outputs)
	if predicate.Operator == machine.OperatorExists {
		return leftErr == nil, nil
	}
	if leftErr != nil {
		return false, leftErr
	}
	if predicate.Right == nil {
		return false, errors.New("predicate right value is missing")
	}
	right, err := resolveValue(*predicate.Right, runInput, outputs)
	if err != nil {
		return false, err
	}
	switch predicate.Operator {
	case machine.OperatorEQ:
		return reflect.DeepEqual(left, right), nil
	case machine.OperatorNEQ:
		return !reflect.DeepEqual(left, right), nil
	case machine.OperatorGT, machine.OperatorGTE, machine.OperatorLT, machine.OperatorLTE:
		comparison, err := compareJSONNumbers(left, right)
		if err != nil {
			return false, err
		}
		switch predicate.Operator {
		case machine.OperatorGT:
			return comparison > 0, nil
		case machine.OperatorGTE:
			return comparison >= 0, nil
		case machine.OperatorLT:
			return comparison < 0, nil
		default:
			return comparison <= 0, nil
		}
	case machine.OperatorContains:
		switch container := left.(type) {
		case string:
			value, ok := right.(string)
			return ok && strings.Contains(container, value), nil
		case []any:
			for _, item := range container {
				if reflect.DeepEqual(item, right) {
					return true, nil
				}
			}
			return false, nil
		default:
			return false, errors.New("contains left value is not text or array")
		}
	case machine.OperatorIn:
		values, ok := right.([]any)
		if !ok {
			return false, errors.New("in right value is not an array")
		}
		for _, item := range values {
			if reflect.DeepEqual(left, item) {
				return true, nil
			}
		}
		return false, nil
	default:
		return false, fmt.Errorf("predicate operator %q is unsupported", predicate.Operator)
	}
}

func compareJSONNumbers(left, right any) (int, error) {
	leftNumber, leftOK := jsonNumberText(left)
	rightNumber, rightOK := jsonNumberText(right)
	if !leftOK || !rightOK {
		return 0, errors.New("ordered predicate operands are not numbers")
	}
	leftRat, leftOK := new(big.Rat).SetString(leftNumber)
	rightRat, rightOK := new(big.Rat).SetString(rightNumber)
	if !leftOK || !rightOK {
		return 0, errors.New("ordered predicate operands are invalid numbers")
	}
	return leftRat.Cmp(rightRat), nil
}

func jsonNumberText(value any) (string, bool) {
	switch typed := value.(type) {
	case json.Number:
		return typed.String(), true
	case float64:
		return fmt.Sprintf("%.17g", typed), true
	case float32:
		return fmt.Sprintf("%.9g", typed), true
	case int:
		return fmt.Sprintf("%d", typed), true
	case int64:
		return fmt.Sprintf("%d", typed), true
	default:
		return "", false
	}
}

func edgeTarget(edges []machine.Edge, route machine.EdgeRoute) (string, bool) {
	for _, edge := range edges {
		if edge.Route == route {
			return edge.ToNodeID, true
		}
	}
	return "", false
}

func resolveValue(
	ref machine.ValueRef,
	runInput any,
	outputs map[string]any,
) (any, error) {
	var root any
	switch ref.Source {
	case machine.ValueRunInput:
		root = runInput
	case machine.ValueNodeOutput:
		value, ok := outputs[ref.NodeID]
		if !ok {
			if ref.Default != nil {
				return resolveValue(*ref.Default, runInput, outputs)
			}
			return nil, fmt.Errorf("node output %q is unavailable", ref.NodeID)
		}
		root = value
	case machine.ValueLiteral:
		var value any
		if err := decodeJSONValue(ref.Value, &value); err != nil {
			return nil, err
		}
		return value, nil
	default:
		return nil, fmt.Errorf("value source %q is unsupported", ref.Source)
	}
	return resolveJSONPointer(root, ref.Path)
}

func resolveJSONPointer(value any, pointer string) (any, error) {
	if pointer == "" {
		return value, nil
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, errors.New("JSON pointer must be empty or start with slash")
	}
	current := value
	for _, rawToken := range strings.Split(pointer[1:], "/") {
		for index := 0; index < len(rawToken); index++ {
			if rawToken[index] == '~' &&
				(index+1 >= len(rawToken) ||
					(rawToken[index+1] != '0' && rawToken[index+1] != '1')) {
				return nil, fmt.Errorf("JSON pointer token %q has an invalid escape", rawToken)
			}
		}
		token := strings.ReplaceAll(strings.ReplaceAll(rawToken, "~1", "/"), "~0", "~")
		switch typed := current.(type) {
		case map[string]any:
			next, ok := typed[token]
			if !ok {
				return nil, fmt.Errorf("JSON pointer member %q is unavailable", token)
			}
			current = next
		case []any:
			index := new(big.Int)
			if _, ok := index.SetString(token, 10); !ok || !index.IsInt64() {
				return nil, fmt.Errorf("JSON pointer array index %q is invalid", token)
			}
			position := index.Int64()
			if position < 0 || position >= int64(len(typed)) {
				return nil, fmt.Errorf("JSON pointer array index %q is unavailable", token)
			}
			current = typed[position]
		default:
			return nil, fmt.Errorf("JSON pointer cannot traverse %T", current)
		}
	}
	return current, nil
}

func decodeJSONValue(raw json.RawMessage, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}
