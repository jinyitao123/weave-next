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

	"github.com/jackc/pgx/v5"
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
	// Steps an earlier run of this leg finished are read with the parent.
	finished, err := finishedFanoutLegStepsTx(ctx, tx, task.WorkspaceID, leg)
	if err != nil {
		return nil, executionError(ErrorCodeSnapshotUnavailable, err)
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
	ctx = r.withCredentialAuthorization(ctx, parent, task)
	// Legs observe tools and model calls on the parent run, as serial
	// members do, so a slow branch shows where its time went.
	hostFactory := workflow.ObserveRuntimeTools(r.hostFactoryFor(loaded), r.toolObserver(parent))
	hostFactory = workflow.ObserveRuntimeModelsWithOptions(hostFactory, r.modelObserver(parent), r.ModelObservationOptions)
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
	// A branch may run several dispatch workers in turn; the leg's result is
	// the last one's output. Later steps were frozen with the same artifact, so
	// each only has to be proven unable to park the leg.
	steps, err := machine.ParallelBranchSteps(loaded.graph, leg.BranchID)
	if err != nil {
		return nil, executionError(ErrorCodeRuntimeIncompatible, err)
	}
	bundles := make(map[string]frozen.FrozenExecutionBundle, len(loaded.payload.Bundles))
	for _, bundle := range loaded.payload.Bundles {
		bundles[runtimeEntryKey(bundle.Agent.AgentID, bundle.Agent.AgentVersion)] = bundle
	}
	for _, step := range steps[1:] {
		config, _ := step.Config.(machine.WorkerConfig)
		if bundle, present := bundles[runtimeEntryKey(config.AgentID, config.AgentVersion)]; !present || bundle.Capability.MayYield {
			return nil, executionError(ErrorCodeUnexpectedInteractiveYield, fmt.Errorf("parallel branch step %q lacks a may_yield=false bundle", step.ID))
		}
	}
	entries := make(map[string]workflow.RuntimeGraphEntry, len(runtimeArtifact.Entries))
	for _, entry := range runtimeArtifact.Entries {
		entries[runtimeEntryKey(entry.AgentID, entry.AgentVersion)] = entry
	}
	// Each finished step stores what it spent with its output; the parent
	// adds it to its own usage when the group joins.
	var retry struct {
		Generation int `json:"retry_generation"`
	}
	if len(task.RuntimeAssignment) > 0 {
		if err := json.Unmarshal(task.RuntimeAssignment, &retry); err != nil || retry.Generation < 0 {
			return nil, executionError(ErrorCodeIdentityMismatch, errors.New("invalid explicit retry generation"))
		}
	}
	var actionOutcomes []BusinessActionOutcomeV1
	if load := r.actionOutcomesLoader(parent); load != nil {
		actionOutcomes, err = load(ctx)
		if err != nil {
			return nil, executionError(ErrorCodeSnapshotUnavailable, fmt.Errorf("load platform business action outcomes: %w", err))
		}
	}
	var encodedOutput json.RawMessage
	artifactTaskIDs := []string{}
	// A later step reads files an earlier step of the same branch produced.
	stepArtifacts := make(map[string][]string, len(checkpoint.ArtifactTaskIDs)+len(steps))
	for nodeID, ids := range checkpoint.ArtifactTaskIDs {
		stepArtifacts[nodeID] = ids
	}
	checkpoint.ArtifactTaskIDs = stepArtifacts
	rerunChecked := false
	for ordinal, step := range steps {
		if done, present := finished[step.ID]; present {
			// A step that finished before is not run again; its output feeds the next.
			var output any
			if err := decodeJSONValue(done.Output, &output); err != nil {
				return nil, executionError(ErrorCodeSnapshotUnavailable, err)
			}
			outputs[step.ID] = output
			stepArtifacts[step.ID] = done.ArtifactTaskIDs
			encodedOutput, artifactTaskIDs = done.Output, done.ArtifactTaskIDs
			continue
		}
		// The first step keeps the leg's own invocation identity, as before
		// branches had several steps; each later step has one of its own.
		invocationID := "fanout/" + leg.LegID
		if ordinal > 0 {
			invocationID += "/" + step.ID
		}
		output, stepArtifactTaskIDs, nodeUsage, err := r.runFanoutLegStep(ctx, parent, loaded, entries, step, invocationID, retry.Generation,
			runInput, outputs, checkpoint, actionOutcomes)
		if err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(output)
		if err != nil {
			return nil, executionError(ErrorCodeOutputInvalid, err)
		}
		stepUsage := newFanoutLegStepUsage(leg.GroupID+"/"+invocationID, nodeUsage)
		// An earlier claim of this task, or an earlier retry generation, may
		// have started this step and spent tokens that left no receipt.
		if !rerunChecked && (task.ClaimEpoch > 1 || retry.Generation > 0) && stepUsage.IncompleteReason == "" {
			stepUsage.IncompleteReason = UsageIncompleteReasonLegRerun
		}
		rerunChecked = true
		if err := r.recordFanoutLegStep(ctx, task.WorkspaceID, leg, step.ID, ordinal, encoded, stepArtifactTaskIDs, stepUsage); err != nil {
			return nil, executionError(ErrorCodeSnapshotUnavailable, err)
		}
		outputs[step.ID] = output
		stepArtifacts[step.ID] = stepArtifactTaskIDs
		encodedOutput, artifactTaskIDs = encoded, stepArtifactTaskIDs
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

// runFanoutLegStep runs one dispatch worker of a parallel branch and records
// its activity, output and artifacts like any other step of the run.
func (r *WorkflowSerialRuntime) runFanoutLegStep(
	ctx context.Context,
	parent TeamRun,
	loaded loadedWorkflowGraph,
	entries map[string]workflow.RuntimeGraphEntry,
	branch machine.Node,
	invocationID string,
	retryGeneration int,
	runInput any,
	outputs map[string]any,
	checkpoint WorkflowCheckpointV1,
	actionOutcomes []BusinessActionOutcomeV1,
) (any, []string, nodeUsageReport, error) {
	worker, _ := branch.Config.(machine.WorkerConfig)
	memberID, memberVersion, _ := agentNodeIdentity(branch, loaded.payload)
	if err := loadRuntimeGraph(entries, memberID, memberVersion); err != nil {
		return nil, nil, nodeUsageReport{}, executionError(ErrorCodeRuntimeIncompatible, err)
	}
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
	rootID := execution.EngineTaskID(parent.WorkspaceID, invocationID)
	if retryGeneration > 0 {
		invocationID += fmt.Sprintf("/retry-%d", retryGeneration)
	}
	inputCtx := execution.WithAttemptLineage(execution.WithInputTaskIDs(ctx, nodeInputTaskIDs(branch, checkpoint.ArtifactTaskIDs)), rootID, "")
	output, nodeUsage, err := runAgentNode(
		execution.WithInvocationID(inputCtx, invocationID), branch, loaded.payload, entries, runInput, outputs,
		checkpoint.Corrections, "", actionOutcomes,
		func(ctx context.Context) context.Context { return r.withBusinessActionOutcomeContext(ctx, parent) },
		workbenchResultPromptRequired(loaded.graph, branch.ID),
	)
	owner := workflowArtifactOwner{NodeID: branch.ID, NodeLabel: branch.Label, NodeType: string(branch.Type), AgentID: worker.AgentID}
	if err != nil {
		if recordErr := r.recordWorkflowArtifacts(ctx, parent, owner, nodeUsage.Artifacts, false); recordErr != nil {
			return nil, nil, nodeUsage, executionError(ErrorCodeDeliveryUnavailable, errors.Join(err, recordErr))
		}
		if recordActivity != nil {
			failure := ClassifyFailure(err)
			recordActivity(ctx, "member_failed", branch, memberID, memberVersion, map[string]any{
				"duration_ms": time.Since(startedAt).Milliseconds(), "error_code": string(executionErrorCode(err)),
				"failure_class": failure.Class, "failure_reason": failure.Reason, "retryable": failure.Retryable,
				"output_validation": nodeOutputViolation(err),
			})
		}
		return nil, nil, nodeUsage, err
	}
	if recordActivity != nil {
		recordActivity(ctx, "member_completed", branch, memberID, memberVersion, map[string]any{
			"duration_ms": time.Since(startedAt).Milliseconds(), "tool_calls": nodeUsage.Totals.ToolCalls,
			"input_tokens": nodeUsage.Totals.InputTokens, "output_tokens": nodeUsage.Totals.OutputTokens,
		})
	}
	if recorder := r.workflowOutputRecorder(parent); recorder != nil {
		if err := recorder(ctx, branch, output, false); err != nil {
			return nil, nil, nodeUsage, executionError(ErrorCodeDeliveryUnavailable, err)
		}
	}
	if err := r.recordWorkflowArtifacts(ctx, parent, owner, nodeUsage.Artifacts, false); err != nil {
		return nil, nil, nodeUsage, executionError(ErrorCodeDeliveryUnavailable, err)
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
	return output, artifactTaskIDs, nodeUsage, nil
}

type finishedFanoutLegStep struct {
	Output          json.RawMessage
	ArtifactTaskIDs []string
}

func (r *WorkflowSerialRuntime) finishedFanoutLegSteps(ctx context.Context, workspaceID string, leg FanoutLegTaskPayloadV1) (map[string]finishedFanoutLegStep, error) {
	tx, err := r.Transactions.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	finished, err := finishedFanoutLegStepsTx(ctx, tx, workspaceID, leg)
	if err != nil {
		return nil, err
	}
	return finished, tx.Commit(ctx)
}

func finishedFanoutLegStepsTx(ctx context.Context, tx pgx.Tx, workspaceID string, leg FanoutLegTaskPayloadV1) (map[string]finishedFanoutLegStep, error) {
	rows, err := tx.Query(ctx, `SELECT node_id,output,artifact_task_ids FROM weave_fanout_leg_steps
		WHERE workspace_id=$1 AND group_id=$2 AND leg_id=$3`, workspaceID, leg.GroupID, leg.LegID)
	if err != nil {
		return nil, fmt.Errorf("read finished branch steps: %w", err)
	}
	finished := map[string]finishedFanoutLegStep{}
	for rows.Next() {
		var nodeID string
		var step finishedFanoutLegStep
		var ids []byte
		if err := rows.Scan(&nodeID, &step.Output, &ids); err != nil {
			rows.Close()
			return nil, err
		}
		if err := json.Unmarshal(ids, &step.ArtifactTaskIDs); err != nil {
			rows.Close()
			return nil, err
		}
		finished[nodeID] = step
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return finished, nil
}

// recordFanoutLegStep keeps a finished branch step. A step recorded by an
// earlier run of the same leg wins; the leg then continues from its output.
func (r *WorkflowSerialRuntime) recordFanoutLegStep(ctx context.Context, workspaceID string, leg FanoutLegTaskPayloadV1, nodeID string, ordinal int, output json.RawMessage, artifactTaskIDs []string, usage fanoutLegStepUsageV1) error {
	if artifactTaskIDs == nil {
		artifactTaskIDs = []string{}
	}
	ids, err := json.Marshal(artifactTaskIDs)
	if err != nil {
		return err
	}
	encodedUsage, err := encodeFanoutLegStepUsage(usage)
	if err != nil {
		return err
	}
	tx, err := r.Transactions.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, `INSERT INTO weave_fanout_leg_steps(workspace_id,group_id,leg_id,node_id,step_ordinal,output,artifact_task_ids,usage)
		VALUES($1,$2,$3,$4,$5,$6::jsonb,$7::jsonb,$8::jsonb) ON CONFLICT (workspace_id,group_id,leg_id,node_id) DO NOTHING`,
		workspaceID, leg.GroupID, leg.LegID, nodeID, ordinal, string(output), string(ids), encodedUsage); err != nil {
		return fmt.Errorf("record finished branch step: %w", err)
	}
	return tx.Commit(ctx)
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
