package teamrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

type UsageTotals = loomruntime.UsageTotals

type RuntimeResultStatus string

const (
	RuntimeCompleted RuntimeResultStatus = "completed"
	RuntimeParked    RuntimeResultStatus = "parked"
	RuntimeFailed    RuntimeResultStatus = "failed"
)

type RuntimePark struct {
	MemberBreakdown  map[string]loomruntime.TerminalChildBreakdownV3
	ActiveMember     *ActiveMemberInvocation
	NodeID           string
	CompletedOutputs map[string]json.RawMessage
	ArtifactTaskIDs  map[string][]string
	DeliveryErrors   map[string]string
	WaitKind         WaitKind
	WaitDetail       json.RawMessage
	// Corrections carries confirmed user directives across every subsequent
	// safe-point park. Without this, a correction resume that immediately
	// re-enters fanout loses the directive before the branch tasks start.
	Corrections []CorrectionDirectiveV1
	// UsageCheckpoint persists the serial machine's usage accumulator so a
	// park/resume cycle never loses or duplicates confirmed usage.
	UsageCheckpoint json.RawMessage
	// UsageComplete is false when the run reached a node whose usage cannot
	// be measured (candidate fanout legs / CLI node without a receipt);
	// UsageIncompleteReason names the unmeasured part.
	UsageComplete         bool
	UsageIncompleteReason string
}

type RuntimeResult struct {
	MemberBreakdown map[string]loomruntime.TerminalChildBreakdownV3
	Status          RuntimeResultStatus
	Output          json.RawMessage
	Park            *RuntimePark
	// Usage is the accumulated confirmed logical usage of the serial machine.
	// It is set on every terminal outcome (completed, parked, failed) so a
	// failed run still charges its observed usage.
	Usage UsageTotals
	// UsageCoverage is present for runtimes using the Phase-2 receipt ABI. A
	// nil value preserves legacy callers; a non-nil value distinguishes an
	// unreported dimension from an explicitly reported zero.
	UsageCoverage *loomruntime.UsageCoverage
	// UsageComplete is false when the run result only covers the measured
	// serial/loop contributions; UsageIncompleteReason names the unmeasured
	// part (see the UsageIncompleteReason* constants).
	UsageComplete         bool
	UsageIncompleteReason string
}

type RuntimeRunner interface {
	Execute(context.Context, TeamRun, *taskqueue.Task) (RuntimeResult, error)
	ResumeCheckpoint(context.Context, TeamRun, *taskqueue.Task, WorkflowCheckpointV1) (RuntimeResult, error)
	TimerResumeTarget(context.Context, TeamRun, *taskqueue.Task, WorkflowCheckpointV1) (string, bool, error)
}

func validateWorkflowCheckpoint(checkpoint WorkflowCheckpointV1) error {
	if checkpoint.SchemaVersion != WorkflowCheckpointSchemaVersion ||
		checkpoint.Stamp.WorkspaceID == "" ||
		checkpoint.Stamp.WorkflowID == "" ||
		checkpoint.Stamp.WorkflowVersion < 1 ||
		checkpoint.Stamp.RunSnapshotID == "" ||
		checkpoint.RunID == "" ||
		checkpoint.TeamRunGeneration < 0 ||
		checkpoint.ExecutionLeaseEpoch < 0 ||
		checkpoint.NodeID == "" ||
		checkpoint.CompletedOutputs == nil ||
		checkpoint.WrittenAt.IsZero() {
		return fmt.Errorf("%w: checkpoint fields are invalid", ErrTeamRunSnapshotUnavailable)
	}
	for nodeID, output := range checkpoint.CompletedOutputs {
		if nodeID == "" || len(output) == 0 || !json.Valid(output) {
			return fmt.Errorf("%w: checkpoint output is invalid", ErrTeamRunSnapshotUnavailable)
		}
	}
	for nodeID, ids := range checkpoint.ArtifactTaskIDs {
		if _, ok := checkpoint.CompletedOutputs[nodeID]; !ok || len(ids) > 512 {
			return fmt.Errorf("%w: checkpoint file sources are invalid", ErrTeamRunSnapshotUnavailable)
		}
		for _, id := range ids {
			valid := strings.HasPrefix(id, "task-") && len(id) <= 128
			if strings.HasPrefix(id, "member:") {
				_, err := uuid.Parse(strings.TrimPrefix(id, "member:"))
				valid = err == nil
			}
			if !valid {
				return fmt.Errorf("%w: checkpoint source identity is invalid", ErrTeamRunSnapshotUnavailable)
			}
		}
	}
	for nodeID, message := range checkpoint.DeliveryErrors {
		if _, ok := checkpoint.CompletedOutputs[nodeID]; !ok || len(message) > 2048 ||
			!strings.HasPrefix(message, "delivery_artifact_uncollected: ") {
			return fmt.Errorf("%w: checkpoint delivery evidence is invalid", ErrTeamRunSnapshotUnavailable)
		}
	}
	if len(checkpoint.Usage) != 0 {
		accumulator, err := loomruntime.UnmarshalUsageAccumulator(checkpoint.Usage)
		if err != nil {
			return fmt.Errorf("%w: checkpoint usage: %v", ErrTeamRunSnapshotUnavailable, err)
		}
		if owner := accumulator.OwnedRunID(); owner != "" && owner != checkpoint.RunID {
			return fmt.Errorf(
				"%w: checkpoint usage belongs to run %q, not %q",
				ErrTeamRunSnapshotUnavailable,
				owner,
				checkpoint.RunID,
			)
		}
		if checkpoint.ActiveMember != nil {
			ordinal, present := accumulator.CallOrdinal(checkpoint.ActiveMember.CallID)
			if !present || checkpoint.ActiveMember.NodeID != checkpoint.NodeID || ordinal != checkpoint.ActiveMember.EntryOrdinal {
				return fmt.Errorf("%w: pending member identity differs from usage checkpoint", ErrTeamRunSnapshotUnavailable)
			}
		}
	} else if checkpoint.ActiveMember != nil {
		return fmt.Errorf("%w: pending member requires a usage checkpoint", ErrTeamRunSnapshotUnavailable)
	}
	if checkpoint.UsageIncompleteReason != "" && checkpoint.UsageComplete {
		return fmt.Errorf(
			"%w: checkpoint usage_incomplete_reason requires usage_complete=false",
			ErrTeamRunSnapshotUnavailable,
		)
	}
	memberUsage := loomruntime.TerminalUsage{}
	for id, child := range checkpoint.MemberBreakdown {
		u := child.SelfExclusive
		if id == "" || id == checkpoint.RunID || child.RunID != id || child.ParentRunID != checkpoint.RunID ||
			child.ParentSeq < 1 || child.Agent == "" || child.WorkflowID == nil || *child.WorkflowID != checkpoint.Stamp.WorkflowID ||
			child.WorkflowVersion == nil || *child.WorkflowVersion != checkpoint.Stamp.WorkflowVersion ||
			child.RunSnapshotID == nil || *child.RunSnapshotID != checkpoint.Stamp.RunSnapshotID ||
			u.InputTokens < 0 || u.OutputTokens < 0 || u.ToolCalls < 0 || u.CostUSD < 0 || math.IsNaN(u.CostUSD) || math.IsInf(u.CostUSD, 0) {
			return fmt.Errorf("%w: checkpoint member contribution is invalid", ErrTeamRunSnapshotUnavailable)
		}
		memberUsage.InputTokens += u.InputTokens
		memberUsage.OutputTokens += u.OutputTokens
		memberUsage.ToolCalls += u.ToolCalls
		memberUsage.CostUSD += u.CostUSD
	}
	if len(checkpoint.MemberBreakdown) > 0 {
		accumulator, err := loomruntime.UnmarshalUsageAccumulator(checkpoint.Usage)
		if err != nil {
			return fmt.Errorf("%w: member contributions require usage", ErrTeamRunSnapshotUnavailable)
		}
		total := accumulator.Totals()
		if memberUsage.InputTokens > total.InputTokens || memberUsage.OutputTokens > total.OutputTokens ||
			memberUsage.ToolCalls > total.ToolCalls || memberUsage.CostUSD-total.CostUSD > 1e-12 {
			return fmt.Errorf("%w: member contribution exceeds recorded usage", ErrTeamRunSnapshotUnavailable)
		}
	}
	for _, correction := range checkpoint.Corrections {
		if correction.SchemaVersion != 1 || correction.CorrectionID == "" ||
			correction.Instruction == "" || len(correction.AffectedNodes) == 0 ||
			(correction.TargetKind != "team" && correction.TargetKind != "member") ||
			(correction.TargetKind == "member" && correction.TargetMemberID == "") ||
			(correction.TargetKind == "team" && correction.TargetMemberID != "") {
			return fmt.Errorf("%w: checkpoint correction is invalid", ErrTeamRunSnapshotUnavailable)
		}
	}
	return nil
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
	Members     *loomruntime.MemberRunner
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
	Corrections            *CorrectionStore
	Activities             ActivityRecorder
	Now                    func() time.Time
}

type runtimeActivityScope struct {
	NodeID        string
	MemberID      string
	MemberVersion int64
}

type runtimeActivityScopeKey struct{}

func (r *WorkflowSerialRuntime) toolObserver(run TeamRun) workflow.RuntimeToolObserver {
	if r.Activities == nil {
		return nil
	}
	return func(ctx context.Context, event workflow.RuntimeToolEvent) {
		scope, ok := ctx.Value(runtimeActivityScopeKey{}).(runtimeActivityScope)
		if !ok || scope.NodeID == "" || scope.MemberID == "" {
			return
		}
		detail, err := json.Marshal(map[string]any{
			"tool_name": event.Tool, "tool_call_id": event.CallID,
			"status": map[bool]string{true: "error", false: "ok"}[event.ResultError],
		})
		if err != nil {
			return
		}
		if err := r.Activities.Record(ctx, ActivityEvent{
			WorkspaceID: run.WorkspaceID, RunID: run.RunID, Kind: event.Kind,
			NodeID: scope.NodeID, MemberID: scope.MemberID, MemberVersion: scope.MemberVersion,
			Detail: detail, OccurredAt: time.Now().UTC(),
		}); err != nil {
			slog.Warn("team run tool activity record failed", "run_id", run.RunID, "node_id", scope.NodeID, "error", err)
		}
	}
}

func (r *WorkflowSerialRuntime) correctionBoundary(
	run TeamRun,
	graph machine.GraphDefinition,
	payload frozen.ArtifactPayloadV1,
) func(context.Context, string, map[string]any) (*CorrectionWaitDetailV1, error) {
	if r.Corrections == nil {
		return nil
	}
	return func(ctx context.Context, currentNodeID string, outputs map[string]any) (*CorrectionWaitDetailV1, error) {
		item, present, err := r.Corrections.GetActive(ctx, run.WorkspaceID, run.RunID)
		if err != nil {
			return nil, err
		}
		if !present || item.Status != CorrectionRequested {
			return nil, nil
		}
		detail, err := buildCorrectionWaitDetail(graph, payload, currentNodeID, outputs, item)
		if err != nil {
			return nil, err
		}
		return &detail, nil
	}
}

func (r *WorkflowSerialRuntime) activityRecorder(run TeamRun) func(context.Context, string, machine.Node, string, int64, map[string]any) {
	if r.Activities == nil {
		return nil
	}
	return func(ctx context.Context, kind string, node machine.Node, memberID string, memberVersion int64, detail map[string]any) {
		encoded, err := json.Marshal(detail)
		if err != nil {
			return
		}
		if err := r.Activities.Record(ctx, ActivityEvent{WorkspaceID: run.WorkspaceID, RunID: run.RunID,
			Kind: kind, NodeID: node.ID, MemberID: memberID, MemberVersion: memberVersion,
			Detail: encoded, OccurredAt: time.Now().UTC()}); err != nil {
			slog.Warn("team run activity record failed", "run_id", run.RunID, "node_id", node.ID, "error", err)
		}
	}
}

type engineExecObservationStore interface {
	ListEngineExecObservations(context.Context, string, string, string) ([]taskqueue.EngineExecObservation, error)
}

func (r *WorkflowSerialRuntime) observedEventLoader(run TeamRun) func(context.Context, machine.Node, string) []workflow.RuntimeCLIEvent {
	store, ok := r.Tasks.(engineExecObservationStore)
	if !ok || run.WorkspaceID == "" || run.RunSnapshotID == "" {
		return nil
	}
	return func(ctx context.Context, node machine.Node, memberID string) []workflow.RuntimeCLIEvent {
		if strings.TrimSpace(memberID) == "" {
			return nil
		}
		observations, err := store.ListEngineExecObservations(ctx, run.WorkspaceID, run.RunSnapshotID, memberID)
		if err != nil {
			slog.Warn("team run observed activity reconciliation failed", "run_id", run.RunID, "node_id", node.ID, "error", err)
			return nil
		}
		return runtimeEventsFromEngineExecObservations(observations, 200)
	}
}

func runtimeEventsFromEngineExecObservations(observations []taskqueue.EngineExecObservation, limit int) []workflow.RuntimeCLIEvent {
	if limit <= 0 || len(observations) == 0 {
		return nil
	}
	type engineExecObservationResult struct {
		Events []workflow.RuntimeCLIEvent `json:"events"`
	}
	events := make([]workflow.RuntimeCLIEvent, 0, min(limit, len(observations)))
	seen := make(map[string]struct{})
	for _, observation := range observations {
		var result engineExecObservationResult
		if len(observation.Result) == 0 || json.Unmarshal(observation.Result, &result) != nil {
			continue
		}
		for _, event := range result.Events {
			if len(events) >= limit {
				return events
			}
			if event.Kind != "tool_call" && event.Kind != "tool_result" {
				continue
			}
			if observation.TaskID != "" && event.CallID != "" &&
				!strings.HasPrefix(event.CallID, observation.TaskID+":") {
				event.CallID = observation.TaskID + ":" + event.CallID
			}
			key := strings.Join([]string{event.Kind, event.CallID, event.Tool}, "\x00")
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			events = append(events, event)
		}
	}
	return events
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
			MemberContext:      r.memberContext(run, prepared.envelope.ContentHash),
			ArtifactHash:       prepared.envelope.ContentHash,
			Candidate:          prepared.roundBoundCandidate(),
			RecordOutput:       r.workflowOutputRecorder(run),
			RecordArtifact:     r.workflowArtifactRecorder(run),
			LoadArtifacts:      r.workflowArtifactLoader(run),
			RecordDelivery:     r.workflowDeliveryRecorder(run),
			RecordCheckpoint:   r.serialCheckpointWriter(run, prepared.artifact),
			CheckCorrection:    r.correctionBoundary(run, prepared.graph, prepared.payload),
			RecordActivity:     r.activityRecorder(run),
			LoadObservedEvents: r.observedEventLoader(run),
		},
	)
	return runtimeResultFromSerial(result, prepared.payload, nil)
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
			MemberBreakdown: checkpoint.MemberBreakdown, NodeID: checkpoint.NodeID, Outputs: outputs,
			ActiveMember:    checkpoint.ActiveMember,
			MemberContext:   r.memberContext(run, prepared.envelope.ContentHash),
			DeliveryErrors:  checkpoint.DeliveryErrors,
			ArtifactTaskIDs: checkpoint.ArtifactTaskIDs,
			SourceKind:      run.SourceKind, Now: r.now(), Run: run,
			ArtifactHash:          prepared.envelope.ContentHash,
			Candidate:             prepared.roundBoundCandidate(),
			Usage:                 seedUsage,
			UsageComplete:         checkpoint.UsageComplete,
			UsageIncompleteReason: checkpoint.UsageIncompleteReason,
			RecordOutput:          r.workflowOutputRecorder(run),
			RecordArtifact:        r.workflowArtifactRecorder(run),
			LoadArtifacts:         r.workflowArtifactLoader(run),
			RecordDelivery:        r.workflowDeliveryRecorder(run),
			RecordCheckpoint:      r.serialCheckpointWriter(run, prepared.artifact),
			CheckCorrection:       r.correctionBoundary(run, prepared.graph, prepared.payload),
			RecordActivity:        r.activityRecorder(run),
			LoadObservedEvents:    r.observedEventLoader(run),
			Corrections:           append([]CorrectionDirectiveV1(nil), checkpoint.Corrections...),
		},
	)
	return runtimeResultFromSerial(result, prepared.payload, checkpoint.Corrections)
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

func (r *WorkflowSerialRuntime) workflowArtifactRecorder(
	run TeamRun,
) func(context.Context, machine.Node, deliverable.WorkflowArtifact, bool) error {
	if r == nil || r.OutputRecorder == nil {
		return nil
	}
	return func(ctx context.Context, node machine.Node, artifact deliverable.WorkflowArtifact, final bool) error {
		agentID := ""
		if worker, ok := node.Config.(machine.WorkerConfig); ok {
			agentID = worker.AgentID
		}
		return r.OutputRecorder.RecordWorkflowOutput(ctx, deliverable.WorkflowOutput{
			WorkspaceID: run.WorkspaceID, RunID: run.RunID, RunSnapshotID: run.RunSnapshotID,
			NodeID: node.ID, NodeLabel: node.Label, NodeType: string(node.Type), AgentID: agentID,
			Artifact: &artifact,
			Final:    final, CreatedAt: r.now(),
		})
	}
}

// recordWorkflowArtifacts gives fanout branches the same immutable artifact
// projection as serial worker nodes. Without this bridge the runtime files
// exist only in the executor workspace while the user-visible run records
// merely the worker's prose summary.
type workflowArtifactOwner struct {
	NodeID    string
	NodeLabel string
	NodeType  string
	AgentID   string
}

func (r *WorkflowSerialRuntime) recordWorkflowArtifacts(
	ctx context.Context,
	run TeamRun,
	owner workflowArtifactOwner,
	artifacts []workflow.RuntimeCLIArtifact,
	final bool,
) error {
	if r == nil || r.OutputRecorder == nil {
		return nil
	}
	for _, artifact := range artifacts {
		if err := r.OutputRecorder.RecordWorkflowOutput(ctx, deliverable.WorkflowOutput{
			WorkspaceID: run.WorkspaceID, RunID: run.RunID, RunSnapshotID: run.RunSnapshotID,
			NodeID: owner.NodeID, NodeLabel: owner.NodeLabel, NodeType: owner.NodeType, AgentID: owner.AgentID,
			Artifact: &deliverable.WorkflowArtifact{
				Path: artifact.Path, ContentType: artifact.ContentType, Content: artifact.Content,
			},
			Final: final, CreatedAt: r.now(),
		}); err != nil {
			return err
		}
	}
	return nil
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
	hostFactory := workflow.ObserveRuntimeTools(r.hostFactoryFor(loaded), r.toolObserver(run))
	runtimeArtifact, err := loader.Load(ctx, loaded.envelope, hostFactory, resolver)
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
	corrections []CorrectionDirectiveV1,
) (RuntimeResult, error) {
	coverage := result.Usage.Coverage()
	coveragePtr := &coverage
	switch result.Status {
	case serialCompleted:
		if len(payload.DeliveryTargets) != 0 {
			return RuntimeResult{MemberBreakdown: result.MemberBreakdown, Status: RuntimeFailed}, executionError(
				ErrorCodeDeliveryUnavailable,
				errors.New("delivery outbox is unavailable"),
			)
		}
		encoded, err := json.Marshal(map[string]any{"output": result.Output})
		if err != nil {
			return RuntimeResult{MemberBreakdown: result.MemberBreakdown, Status: RuntimeFailed}, executionError(ErrorCodeOutputInvalid, err)
		}
		return RuntimeResult{MemberBreakdown: result.MemberBreakdown,
			Status: RuntimeCompleted, Output: encoded, Usage: result.Usage.Totals(),
			UsageCoverage:         coveragePtr,
			UsageComplete:         result.UsageComplete,
			UsageIncompleteReason: result.UsageIncompleteReason,
		}, nil
	case serialParked:
		outputs := make(map[string]json.RawMessage, len(result.Outputs))
		for nodeID, output := range result.Outputs {
			encoded, err := json.Marshal(output)
			if err != nil {
				return RuntimeResult{MemberBreakdown: result.MemberBreakdown, Status: RuntimeFailed}, executionError(ErrorCodeRuntimeIncompatible, err)
			}
			outputs[nodeID] = encoded
		}
		usageCheckpoint, err := result.Usage.MarshalCheckpoint()
		if err != nil {
			return RuntimeResult{MemberBreakdown: result.MemberBreakdown, Status: RuntimeFailed}, executionError(
				ErrorCodeRuntimeIncompatible,
				fmt.Errorf("encode serial usage checkpoint: %w", err),
			)
		}
		return RuntimeResult{MemberBreakdown: result.MemberBreakdown, Status: RuntimeParked, Park: &RuntimePark{MemberBreakdown: result.MemberBreakdown,
			NodeID: result.NodeID, CompletedOutputs: outputs,
			ActiveMember:    result.ActiveMember,
			DeliveryErrors:  result.DeliveryErrors,
			ArtifactTaskIDs: result.ArtifactTaskIDs,
			WaitKind:        result.WaitKind, WaitDetail: result.WaitDetail,
			Corrections:     append([]CorrectionDirectiveV1(nil), corrections...),
			UsageCheckpoint: usageCheckpoint,
			UsageComplete:   result.UsageComplete, UsageIncompleteReason: result.UsageIncompleteReason,
		}, Usage: result.Usage.Totals(),
			UsageCoverage: coveragePtr,
			UsageComplete: result.UsageComplete, UsageIncompleteReason: result.UsageIncompleteReason}, nil
	case serialFailed:
		return RuntimeResult{MemberBreakdown: result.MemberBreakdown,
			Status: RuntimeFailed, Usage: result.Usage.Totals(),
			UsageCoverage:         coveragePtr,
			UsageComplete:         result.UsageComplete,
			UsageIncompleteReason: result.UsageIncompleteReason,
		}, result.Err
	default:
		return RuntimeResult{MemberBreakdown: result.MemberBreakdown, Status: RuntimeFailed}, executionError(
			ErrorCodeRuntimeIncompatible, errors.New("serial runtime returned an invalid status"),
		)
	}
}

// CLI workflows reconcile immutable queue receipts when resuming a node. Keep
// the pre-call ordinal and upstream files before execution begins so reclaim
// can find the same physical task even before the first fanout or wait.
func (r *WorkflowSerialRuntime) serialCheckpointWriter(run TeamRun, artifact *workflow.RuntimeArtifact) func(context.Context, WorkflowCheckpointV1) error {
	if r.Transactions == nil || r.Runs == nil || r.Checkpoints == nil || artifact == nil || len(artifact.Entries) == 0 {
		return nil
	}
	allCLI, hasDurable := true, false
	for _, entry := range artifact.Entries {
		allCLI = allCLI && entry.CLI != nil
		hasDurable = hasDurable || isDurableMemberEntry(entry)
	}
	if !allCLI && !hasDurable {
		return nil
	}
	return r.progressCheckpointWriter(run)
}

func (r *WorkflowSerialRuntime) progressCheckpointWriter(run TeamRun) func(context.Context, WorkflowCheckpointV1) error {
	return func(ctx context.Context, checkpoint WorkflowCheckpointV1) error {
		tx, err := r.Transactions.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		locked, err := r.Runs.GetForUpdateTx(ctx, tx, run.WorkspaceID, run.RunID)
		if err != nil {
			return err
		}
		if locked.Status != StatusRunning || locked.Generation != run.Generation || locked.ExecutionLeaseEpoch != run.ExecutionLeaseEpoch || locked.ResumeGeneration != run.ResumeGeneration {
			return errTaskLeaseLost
		}
		if _, err := r.Checkpoints.PutTx(ctx, tx, checkpoint); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
}

type workflowMemberContext struct {
	Runner       *loomruntime.MemberRunner
	Run          TeamRun
	ArtifactHash string
	Active       ActiveMemberInvocation
	Guard        func(context.Context, pgx.Tx) error
}

type workflowMemberContextKey struct{}

func isDurableMemberEntry(entry workflow.RuntimeGraphEntry) bool {
	return entry.DurableMember()
}

func (r *WorkflowSerialRuntime) memberContext(run TeamRun, artifactHash string) *workflowMemberContext {
	if r.Members == nil || r.Runs == nil {
		return nil
	}
	return &workflowMemberContext{Runner: r.Members, Run: run, ArtifactHash: artifactHash,
		Guard: func(ctx context.Context, tx pgx.Tx) error {
			locked, err := r.Runs.GetForUpdateTx(ctx, tx, run.WorkspaceID, run.RunID)
			if err != nil {
				return err
			}
			if locked.Status != StatusRunning || locked.Generation != run.Generation ||
				locked.ExecutionLeaseEpoch != run.ExecutionLeaseEpoch || locked.ResumeGeneration != run.ResumeGeneration {
				return errTaskLeaseLost
			}
			return nil
		},
	}
}

func (member workflowMemberContext) attribution() (loomruntime.TerminalAttribution, error) {
	if member.Active.EntryOrdinal >= uint64(^uint64(0)>>1) {
		return loomruntime.TerminalAttribution{}, errors.New("member entry ordinal overflow")
	}
	seq := int64(member.Active.EntryOrdinal) + 1
	return loomruntime.NewTerminalAttribution(loomruntime.TerminalAttributionInput{
		Scope: loomruntime.TerminalAttributionFixedWorkflow, WorkspaceID: member.Run.WorkspaceID,
		TeamID: &member.Run.TeamID, WorkflowID: &member.Run.WorkflowID, WorkflowVersion: &member.Run.WorkflowVersion,
		RunSnapshotID: &member.Run.RunSnapshotID, ParentRunID: &member.Run.RunID, ParentSeq: &seq,
		AggregationParentRunID: &member.Run.RunID,
	}, &loomruntime.TerminalSnapshotEvidence{
		WorkspaceID: member.Run.WorkspaceID, RunID: member.Run.RunSnapshotID, TeamID: member.Run.TeamID,
		Mode: "fixed_workflow", WorkflowID: &member.Run.WorkflowID, WorkflowVersion: &member.Run.WorkflowVersion,
	})
}
