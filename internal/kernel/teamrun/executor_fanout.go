package teamrun

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jinyitao123/weave/internal/base/execution"
	"sort"
	"strings"
	"time"

	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

type fanoutResumeTaskPayloadV1 struct {
	SchemaVersion int    `json:"schema_version"`
	Kind          string `json:"kind"`
	ParentRunID   string `json:"parent_run_id"`
	GroupID       string `json:"group_id"`
	ClaimID       string `json:"claim_id"`
}

func (e *Executor) processFanoutLeg(ctx context.Context, task *taskqueue.Task, workerID string) error {
	if e.Fanout == nil {
		return e.Tasks.FailClaimed(ctx, task.ID, workerID, string(ErrorCodeRuntimeIncompatible))
	}
	runner, ok := e.Runtime.(FanoutLegRunner)
	if !ok {
		return e.Tasks.FailClaimed(ctx, task.ID, workerID, string(ErrorCodeRuntimeIncompatible))
	}
	var payload FanoutLegTaskPayloadV1
	if err := decodeExact(task.Payload, &payload); err != nil || payload.SchemaVersion != 1 ||
		payload.Kind != "fanout_leg" || payload.WorkspaceID != task.WorkspaceID ||
		payload.GroupID != task.ContextKey || payload.ParentRunID == "" || payload.IntentID == "" ||
		payload.LegID == "" || payload.BranchID == "" || payload.Generation == "" ||
		payload.BranchOrdinal < 0 {
		return e.Tasks.FailClaimed(ctx, task.ID, workerID, string(ErrorCodeIdentityMismatch))
	}
	result, runErr := e.executeWithHeartbeat(ctx, task, workerID, func(execCtx context.Context) (RuntimeResult, error) {
		output, err := runner.ExecuteFanoutLeg(execCtx, task, payload)
		if err != nil {
			return RuntimeResult{Status: RuntimeFailed}, err
		}
		return RuntimeResult{Status: RuntimeCompleted, Output: output}, nil
	})
	if runErr != nil && (errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) ||
		errors.Is(runErr, errTaskLeaseLost)) {
		return runErr
	}
	physicalResult := result.Output
	if runErr == nil {
		_, _, decodeErr := decodeFanoutLegExecutionResult(result.Output)
		if decodeErr != nil {
			runErr = executionError(ErrorCodeOutputInvalid, decodeErr)
		}
	}
	now := e.now()
	completion := FanoutLegCompletion{
		WorkspaceID: task.WorkspaceID, GroupID: payload.GroupID, LegID: payload.LegID,
		Generation: payload.Generation, CompletedAt: now,
	}
	if runErr != nil {
		failure := ClassifyFailure(runErr)
		// Infrastructure failures remain recoverable while the parent is parked.
		// The failed durable task is the single-stage retry unit; successful
		// sibling legs and their artifacts remain untouched.
		if failure.Retryable {
			return e.Tasks.FailClaimed(ctx, task.ID, workerID, fanoutCompletionError(runErr))
		}
		completion.Terminal = "failed"
		completion.ErrorCode = fanoutCompletionError(runErr)
	} else {
		completion.Terminal = "succeeded"
		// The coordinator stores the physical envelope so the parent can bind
		// each logical result to the exact artifact sources without racing the
		// final task-queue write. The parent unwraps it before projecting the
		// workflow-visible join result.
		completion.Result = physicalResult
	}
	if err := e.Fanout.RecordLegCompletion(ctx, completion); err != nil {
		return err
	}
	if runErr != nil {
		return e.Tasks.FailClaimed(ctx, task.ID, workerID, completion.ErrorCode)
	}
	return e.Tasks.CompleteClaimed(ctx, task.ID, workerID, physicalResult, payload.ParentRunID)
}

const fanoutLegExecutionResultKind = "fanout_leg_execution"

type fanoutLegExecutionResultV1 struct {
	SchemaVersion   int             `json:"schema_version"`
	InternalKind    string          `json:"__weave_internal_kind"`
	Output          json.RawMessage `json:"output"`
	ArtifactTaskIDs []string        `json:"artifact_task_ids"`
}

func decodeFanoutLegExecutionResult(raw json.RawMessage) (json.RawMessage, []string, error) {
	if !json.Valid(raw) {
		return nil, nil, errors.New("fanout leg result is not valid JSON")
	}
	var probe struct {
		InternalKind string `json:"__weave_internal_kind"`
	}
	if json.Unmarshal(raw, &probe) != nil || probe.InternalKind != fanoutLegExecutionResultKind {
		return append(json.RawMessage(nil), raw...), nil, nil
	}
	var result fanoutLegExecutionResultV1
	if err := decodeExact(raw, &result); err != nil || result.SchemaVersion != 1 || !json.Valid(result.Output) || result.ArtifactTaskIDs == nil {
		return nil, nil, errors.New("fanout leg execution result is invalid")
	}
	seen := map[string]bool{}
	for _, id := range result.ArtifactTaskIDs {
		if strings.TrimSpace(id) == "" || seen[id] {
			return nil, nil, errors.New("fanout leg artifact source identity is invalid")
		}
		seen[id] = true
	}
	return append(json.RawMessage(nil), result.Output...), append([]string{}, result.ArtifactTaskIDs...), nil
}

func fanoutCompletionError(err error) string {
	code := string(executionErrorCode(err))
	detail := strings.TrimSpace(err.Error())
	if detail == "" || detail == code {
		return code
	}
	return code + ": " + detail
}

func (e *Executor) processFanoutResume(ctx context.Context, task *taskqueue.Task, workerID string) error {
	var payload fanoutResumeTaskPayloadV1
	if err := decodeExact(task.Payload, &payload); err != nil || payload.SchemaVersion != 1 ||
		payload.Kind != "fanout_resume" || payload.ParentRunID == "" || payload.GroupID != task.ContextKey ||
		payload.ClaimID == "" {
		return e.Tasks.FailClaimed(ctx, task.ID, workerID, string(ErrorCodeIdentityMismatch))
	}
	tx, err := e.Transactions.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin fanout continuation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	run, err := e.Runs.GetForUpdateTx(ctx, tx, task.WorkspaceID, payload.ParentRunID)
	if err != nil {
		return err
	}
	executorID := "teamrun-fanout-resume:" + payload.ClaimID
	if run.Status != StatusRunning || run.CurrentExecutorID == nil || *run.CurrentExecutorID != executorID ||
		run.WorkflowID != task.WorkflowID || run.WorkflowVersion != task.WorkflowVersion ||
		run.RunSnapshotID != task.RunSnapshotID {
		return e.Tasks.FailClaimed(ctx, task.ID, workerID, string(ErrorCodeIdentityMismatch))
	}
	checkpoint, err := e.Checkpoints.GetTx(ctx, tx, task.WorkspaceID, payload.ParentRunID)
	if err != nil {
		return err
	}
	if err := ValidateCheckpointRun(checkpoint, run); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit fanout continuation read: %w", err)
	}
	result, runErr := e.executeWithHeartbeat(ctx, task, workerID, func(execCtx context.Context) (RuntimeResult, error) {
		return e.Runtime.ResumeCheckpoint(execCtx, run, task, checkpoint)
	})
	if runErr != nil && (errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) ||
		errors.Is(runErr, errTaskLeaseLost)) {
		return runErr
	}
	if runErr != nil {
		failed, failErr := e.failRunning(
			ctx, run, task, executorID, runErr,
			result.Usage, result.UsageCoverage, result.UsageComplete, result.UsageIncompleteReason, result.MemberBreakdown,
		)
		if failErr != nil {
			return failErr
		}
		return e.finishTerminalTask(ctx, task, workerID, failed, nil)
	}
	return e.finishRuntimeResult(ctx, run, task, workerID, executorID, result, true)
}

func (r *WorkflowSerialRuntime) ExecuteFanoutLeg(
	ctx context.Context,
	task *taskqueue.Task,
	leg FanoutLegTaskPayloadV1,
) (json.RawMessage, error) {
	if r == nil || r.Transactions == nil || r.Runs == nil || r.Checkpoints == nil || r.Tasks == nil ||
		task == nil || leg.ParentRunID == "" || leg.BranchID == "" {
		return nil, executionError(ErrorCodeRuntimeIncompatible, errors.New("fanout leg runtime dependencies are unavailable"))
	}
	tx, err := r.Transactions.Begin(ctx)
	if err != nil {
		return nil, executionError(ErrorCodeSnapshotUnavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	parent, err := r.Runs.GetForUpdateTx(ctx, tx, task.WorkspaceID, leg.ParentRunID)
	if err != nil {
		return nil, executionError(ErrorCodeIdentityMismatch, err)
	}
	if parent.Status != StatusParked || parent.WaitKind == nil || *parent.WaitKind != WaitFanout {
		return nil, executionError(
			ErrorCodeIdentityMismatch,
			fmt.Errorf("fanout leg parent is not parked on fanout (status=%s)", parent.Status),
		)
	}
	if parent.WorkflowID != task.WorkflowID || parent.WorkflowVersion != task.WorkflowVersion ||
		parent.RunSnapshotID != task.RunSnapshotID {
		return nil, executionError(ErrorCodeIdentityMismatch, errors.New("fanout leg parent identity differs"))
	}
	checkpoint, err := r.Checkpoints.GetTx(ctx, tx, task.WorkspaceID, leg.ParentRunID)
	if err != nil {
		return nil, executionError(ErrorCodeSnapshotUnavailable, err)
	}
	if err := ValidateCheckpointRun(checkpoint, parent); err != nil {
		return nil, executionError(ErrorCodeIdentityMismatch, err)
	}
	sourceTaskID := parent.SourceTaskID
	if err := tx.Commit(ctx); err != nil {
		return nil, executionError(ErrorCodeSnapshotUnavailable, err)
	}
	sourceTask, err := r.Tasks.Get(ctx, task.WorkspaceID, sourceTaskID)
	if err != nil {
		return nil, executionError(ErrorCodeSnapshotUnavailable, err)
	}
	var runInput any
	if len(bytes.TrimSpace(sourceTask.Payload)) == 0 {
		runInput = map[string]any{}
	} else if err := decodeJSONValue(sourceTask.Payload, &runInput); err != nil {
		return nil, executionError(ErrorCodeRuntimeIncompatible, err)
	}
	outputs, err := decodeCheckpointOutputs(checkpoint.CompletedOutputs)
	if err != nil {
		return nil, executionError(ErrorCodeSnapshotUnavailable, err)
	}
	frozenRun := TeamRun{
		WorkspaceID: task.WorkspaceID, WorkflowID: task.WorkflowID,
		WorkflowVersion: task.WorkflowVersion, RunSnapshotID: task.RunSnapshotID,
	}
	loaded, err := r.loadFrozenGraph(ctx, frozenRun, task)
	if err != nil {
		return nil, err
	}
	resolver, err := r.CredentialResolvers(task.WorkspaceID)
	if err != nil {
		return nil, executionError(ErrorCodeRuntimeIncompatible, err)
	}
	loader := *r.Loader
	loader.RunSnapshotID = parent.RunSnapshotID
	hostFactory := workflow.ObserveRuntimeTools(r.hostFactoryFor(loaded), r.toolObserver(parent))
	runtimeArtifact, err := loader.Load(ctx, loaded.envelope, hostFactory, resolver)
	if err != nil {
		return nil, executionError(ErrorCodeRuntimeIncompatible, err)
	}
	defer runtimeArtifact.Close()
	var branch machine.Node
	found := false
	for _, node := range loaded.graph.Nodes {
		if node.ID == leg.BranchID {
			branch, found = node, true
			break
		}
	}
	worker, validWorker := branch.Config.(machine.WorkerConfig)
	if !found || branch.Type != machine.NodeWorker || !validWorker || worker.Kind != machine.WorkerDispatch {
		return nil, executionError(ErrorCodeRuntimeIncompatible, errors.New("fanout branch is not a dispatch worker"))
	}
	if err := validateFanoutLegFrozenIdentity(leg, loaded, branch, worker); err != nil {
		return nil, err
	}
	entries := make(map[string]workflow.RuntimeGraphEntry, len(runtimeArtifact.Entries))
	for _, entry := range runtimeArtifact.Entries {
		entries[runtimeEntryKey(entry.AgentID, entry.AgentVersion)] = entry
	}
	memberID, memberVersion, _ := agentNodeIdentity(branch, loaded.payload)
	startedAt := time.Now().UTC()
	recordActivity := r.activityRecorder(parent)
	if recordActivity != nil {
		inputNames := make([]string, 0, len(branch.Inputs))
		inputSummary := make(map[string]string, len(branch.Inputs))
		for name := range branch.Inputs {
			inputNames = append(inputNames, name)
			if value, resolveErr := resolveValue(branch.Inputs[name].Value, runInput, outputs); resolveErr == nil {
				inputSummary[name] = activityValueSummary(value)
			}
		}
		sort.Strings(inputNames)
		recordActivity(ctx, "member_started", branch, memberID, memberVersion,
			map[string]any{"input_names": inputNames, "input_summary": inputSummary})
	}
	// Fanout legs still do not contribute to run-level usage settlement; that
	// remains owned by T14B-2B. Their measured usage is retained in the
	// activity ledger so the worksite can show honest member-level facts.
	var retry struct {
		Generation int `json:"retry_generation"`
	}
	if len(task.RuntimeAssignment) > 0 {
		if err := json.Unmarshal(task.RuntimeAssignment, &retry); err != nil || retry.Generation < 0 {
			return nil, executionError(ErrorCodeIdentityMismatch, errors.New("invalid explicit retry generation"))
		}
	}
	invocationID := "fanout/" + leg.LegID
	rootID := execution.EngineTaskID(task.WorkspaceID, invocationID)
	if retry.Generation > 0 {
		invocationID += fmt.Sprintf("/retry-%d", retry.Generation)
	}
	var actionOutcomes []BusinessActionOutcomeV1
	if load := r.actionOutcomesLoader(parent); load != nil {
		actionOutcomes, err = load(ctx)
		if err != nil {
			return nil, executionError(ErrorCodeSnapshotUnavailable, fmt.Errorf("load platform business action outcomes: %w", err))
		}
	}
	inputCtx := execution.WithAttemptLineage(execution.WithInputTaskIDs(ctx, nodeInputTaskIDs(branch, checkpoint.ArtifactTaskIDs)), rootID, "")
	output, nodeUsage, err := runAgentNode(
		execution.WithInvocationID(inputCtx, invocationID), branch, loaded.payload, entries, runInput, outputs,
		checkpoint.Corrections, "", actionOutcomes,
		func(ctx context.Context) context.Context { return r.withBusinessActionOutcomeContext(ctx, parent) },
		workbenchResultPromptRequired(loaded.graph, branch.ID),
	)
	if err != nil {
		if recordErr := r.recordWorkflowArtifacts(ctx, parent, workflowArtifactOwner{
			NodeID: branch.ID, NodeLabel: branch.Label, NodeType: string(branch.Type), AgentID: worker.AgentID,
		}, nodeUsage.Artifacts, false); recordErr != nil {
			return nil, executionError(ErrorCodeDeliveryUnavailable, errors.Join(err, recordErr))
		}
		if recordActivity != nil {
			failure := ClassifyFailure(err)
			recordActivity(ctx, "member_failed", branch, memberID, memberVersion, map[string]any{
				"duration_ms": time.Since(startedAt).Milliseconds(), "error_code": string(executionErrorCode(err)),
				"failure_class": failure.Class, "failure_reason": failure.Reason, "retryable": failure.Retryable,
			})
		}
		return nil, err
	}
	if recordActivity != nil {
		recordActivity(ctx, "member_completed", branch, memberID, memberVersion, map[string]any{
			"duration_ms": time.Since(startedAt).Milliseconds(), "tool_calls": nodeUsage.Totals.ToolCalls,
			"input_tokens": nodeUsage.Totals.InputTokens, "output_tokens": nodeUsage.Totals.OutputTokens,
		})
	}
	if recorder := r.workflowOutputRecorder(parent); recorder != nil {
		if err := recorder(ctx, branch, output, false); err != nil {
			return nil, executionError(ErrorCodeDeliveryUnavailable, err)
		}
	}
	if err := r.recordWorkflowArtifacts(ctx, parent, workflowArtifactOwner{
		NodeID: branch.ID, NodeLabel: branch.Label, NodeType: string(branch.Type), AgentID: worker.AgentID,
	}, nodeUsage.Artifacts, false); err != nil {
		return nil, executionError(ErrorCodeDeliveryUnavailable, err)
	}
	encodedOutput, err := json.Marshal(output)
	if err != nil {
		return nil, executionError(ErrorCodeOutputInvalid, err)
	}
	artifactTaskIDs := []string{}
	if nodeUsage.MemberRunID != "" {
		artifactTaskIDs = []string{"member:" + nodeUsage.MemberRunID}
	}
	for _, attempt := range nodeUsage.CLIAttempts {
		if attempt.AttemptID != "" {
			artifactTaskIDs = []string{attempt.AttemptID}
		}
	}
	encoded, err := json.Marshal(fanoutLegExecutionResultV1{
		SchemaVersion: 1, InternalKind: fanoutLegExecutionResultKind,
		Output: encodedOutput, ArtifactTaskIDs: artifactTaskIDs,
	})
	if err != nil {
		return nil, executionError(ErrorCodeOutputInvalid, err)
	}
	return encoded, nil
}

func validateFanoutLegFrozenIdentity(
	leg FanoutLegTaskPayloadV1,
	loaded loadedWorkflowGraph,
	branch machine.Node,
	worker machine.WorkerConfig,
) error {
	var bundleRef struct {
		WorkspaceID         string `json:"workspace_id"`
		WorkflowID          string `json:"workflow_id"`
		WorkflowVersion     int    `json:"workflow_version"`
		RunSnapshotID       string `json:"run_snapshot_id"`
		ArtifactContentHash string `json:"artifact_content_hash"`
		BranchID            string `json:"branch_id"`
		AgentID             string `json:"agent_id"`
		AgentVersion        int64  `json:"agent_version"`
		BundleContentHash   string `json:"bundle_content_hash"`
	}
	if err := decodeExact(leg.FrozenBundleRef, &bundleRef); err != nil ||
		bundleRef.WorkspaceID != loaded.envelope.WorkspaceID || bundleRef.WorkflowID != loaded.envelope.WorkflowID ||
		bundleRef.WorkflowVersion != loaded.envelope.WorkflowVersion ||
		bundleRef.ArtifactContentHash != loaded.envelope.ContentHash || bundleRef.BranchID != branch.ID ||
		bundleRef.AgentID != worker.AgentID || bundleRef.AgentVersion != worker.AgentVersion {
		return executionError(ErrorCodeIdentityMismatch, errors.New("fanout frozen bundle reference differs"))
	}
	var bundle *frozen.FrozenExecutionBundle
	for index := range loaded.payload.Bundles {
		candidate := &loaded.payload.Bundles[index]
		if candidate.Agent.AgentID == worker.AgentID && candidate.Agent.AgentVersion == worker.AgentVersion {
			if bundle != nil {
				return executionError(ErrorCodeRuntimeIncompatible, errors.New("fanout bundle identity is duplicated"))
			}
			bundle = candidate
		}
	}
	if bundle == nil || bundle.Capability.MayYield {
		return executionError(ErrorCodeUnexpectedInteractiveYield, errors.New("fanout bundle may yield proof failed"))
	}
	bundleHash, err := frozen.HashDTO(*bundle, frozen.PreorderFrozenExecutionBundle)
	if err != nil || bundleHash != bundleRef.BundleContentHash {
		return executionError(ErrorCodeIdentityMismatch, errors.New("fanout bundle content hash differs"))
	}
	var inputRef struct {
		NodeID              string `json:"node_id"`
		CheckpointSequence  int64  `json:"checkpoint_sequence"`
		BindingsContentHash string `json:"bindings_content_hash"`
	}
	if err := decodeExact(leg.InputRef, &inputRef); err != nil || inputRef.NodeID != branch.ID ||
		inputRef.CheckpointSequence < 0 {
		return executionError(ErrorCodeIdentityMismatch, errors.New("fanout input reference differs"))
	}
	bindings, err := frozen.CanonicalizePreordered(branch.Inputs)
	if err != nil {
		return executionError(ErrorCodeRuntimeIncompatible, err)
	}
	bindingsHash := sha256.Sum256(bindings)
	if inputRef.BindingsContentHash != hex.EncodeToString(bindingsHash[:]) {
		return executionError(ErrorCodeIdentityMismatch, errors.New("fanout input binding hash differs"))
	}
	var proof struct {
		ValidatorVersion    int    `json:"validator_version"`
		ValidatedBundleHash string `json:"validated_bundle_hash"`
		MayYield            *bool  `json:"may_yield"`
	}
	if err := decodeExact(leg.MayYieldProof, &proof); err != nil || proof.ValidatorVersion != machine.SchemaVersionV1 ||
		proof.ValidatedBundleHash != bundleHash || proof.MayYield == nil || *proof.MayYield {
		return executionError(ErrorCodeUnexpectedInteractiveYield, errors.New("fanout may-yield proof differs"))
	}
	return nil
}
