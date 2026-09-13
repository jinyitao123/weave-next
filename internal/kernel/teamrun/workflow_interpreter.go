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
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/fanout"
	"github.com/jinyitao123/weave/internal/base/fileartifact"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

// Agent nodes use one execution budget in both candidate validation and
// published workflows. A candidate must exercise the same timeout contract
// that production will enforce after publication. Forty-five minutes keeps
// execution bounded while allowing a CLI worker to finish a realistic complex
// professional pass that can legitimately exceed the former short ceilings.
// Complex professional nodes may legitimately spend more than twenty minutes
// reading evidence, building an application, and running bounded checks. Keep
// the execution finite, but do not turn useful progress into a false failure at
// the former short ceiling.
var agentNodeExecutionTimeout = 45 * time.Minute

type serialMachineStatus string

const (
	serialCompleted serialMachineStatus = "completed"
	serialParked    serialMachineStatus = "parked"
	serialFailed    serialMachineStatus = "failed"
)

type serialMachineStart struct {
	MemberBreakdown map[string]loomruntime.TerminalChildBreakdownV3
	ActiveMember    *ActiveMemberInvocation
	MemberContext   *workflowMemberContext
	NodeID          string
	Outputs         map[string]any
	ArtifactTaskIDs map[string][]string
	DeliveryErrors  map[string]string
	SourceKind      SourceKind
	Now             time.Time
	Run             TeamRun
	ArtifactHash    string
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
	UsageComplete            bool
	UsageIncompleteReason    string
	LoadArtifacts            func(context.Context, []string) ([]deliverable.WorkflowArtifact, error)
	LoadArtifactObservations func(context.Context, []string) ([]deliverable.SourceObservation, error)
	RecordCheckpoint         func(context.Context, WorkflowCheckpointV1) error
	RecordDelivery           func(context.Context, string, string, string, any, []deliverable.WorkflowArtifact, []deliverable.SourceObservation, *deliverable.OutputSelection) error
	RecordOutput             func(context.Context, machine.Node, any, bool) error
	RecordArtifact           func(context.Context, machine.Node, deliverable.WorkflowArtifact, bool) error
	CheckCorrection          func(context.Context, string, map[string]any) (*CorrectionWaitDetailV1, error)
	RecordActivity           func(context.Context, string, machine.Node, string, int64, map[string]any)
	LoadObservedEvents       func(context.Context, machine.Node, string) []workflow.RuntimeCLIEvent
	Corrections              []CorrectionDirectiveV1
}

type serialMachineResult struct {
	MemberBreakdown       map[string]loomruntime.TerminalChildBreakdownV3
	ActiveMember          *ActiveMemberInvocation
	Status                serialMachineStatus
	Output                any
	Outputs               map[string]any
	ArtifactTaskIDs       map[string][]string
	DeliveryErrors        map[string]string
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

func nodePhysicalUsageAttemptID(callID, physicalID string, index int) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("teamrun-node-physical-attempt:%s:%s:%d", callID, physicalID, index)))
	return "ua1_" + hex.EncodeToString(digest[:16])
}

type nodeUsageReport struct {
	MemberPause           *loomruntime.MemberBudgetPause
	MemberRunID           string
	MemberReceipts        []loomruntime.ConfirmedUsageReceipt
	MemberUsageIncomplete bool
	Totals                loomruntime.UsageTotals
	Coverage              loomruntime.UsageCoverage
	CLIAttempts           []workflow.RuntimeCLIUsageAttempt
	Events                []workflow.RuntimeCLIEvent
	Artifacts             []workflow.RuntimeCLIArtifact
	DeliveryError         string
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
	activeMember := start.ActiveMember
	memberBreakdown := make(map[string]loomruntime.TerminalChildBreakdownV3, len(start.MemberBreakdown))
	for id, contribution := range start.MemberBreakdown {
		memberBreakdown[id] = contribution
	}
	// A fresh or legacy start is usage-complete; only an explicit
	// usage_incomplete_reason (persisted across park/resume) marks the run
	// incomplete. The flag is always paired with a non-empty reason.
	usageComplete := true
	usageIncompleteReason := start.UsageIncompleteReason
	if usageIncompleteReason != "" {
		usageComplete = start.UsageComplete
	}
	fail := func(err error) serialMachineResult {
		return serialMachineResult{ActiveMember: activeMember, MemberBreakdown: memberBreakdown,
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
	artifactTaskIDs := make(map[string][]string, len(start.ArtifactTaskIDs))
	for nodeID, ids := range start.ArtifactTaskIDs {
		artifactTaskIDs[nodeID] = append([]string(nil), ids...)
	}
	deliveryErrors := make(map[string]string, len(start.DeliveryErrors))
	for nodeID, message := range start.DeliveryErrors {
		deliveryErrors[nodeID] = message
	}
	current := start.NodeID
	if current == "" {
		current = graph.EntryNodeID
	}
	writeCheckpoint := func() error {
		if start.RecordCheckpoint == nil {
			return nil
		}
		completed := make(map[string]json.RawMessage, len(outputs))
		for id, output := range outputs {
			encoded, err := json.Marshal(output)
			if err != nil {
				return executionError(ErrorCodeOutputInvalid, err)
			}
			completed[id] = encoded
		}
		usageCheckpoint, err := usage.MarshalCheckpoint()
		if err != nil {
			return executionError(ErrorCodeExecutionUnrecoverable, err)
		}
		checkpoint := checkpointFromPark(start.Run, RuntimePark{MemberBreakdown: memberBreakdown,
			ActiveMember: activeMember, NodeID: current, CompletedOutputs: completed, ArtifactTaskIDs: artifactTaskIDs,
			DeliveryErrors: deliveryErrors, Corrections: start.Corrections, UsageCheckpoint: usageCheckpoint,
			UsageComplete: usageComplete, UsageIncompleteReason: usageIncompleteReason,
		}, time.Now().UTC())
		if err := start.RecordCheckpoint(ctx, checkpoint); err != nil {
			return executionError(ErrorCodeExecutionUnrecoverable, err)
		}
		return nil
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
		if err := writeCheckpoint(); err != nil {
			return fail(err)
		}
		if start.CheckCorrection != nil {
			detail, err := start.CheckCorrection(ctx, current, outputs)
			if err != nil {
				return fail(executionError(ErrorCodeExecutionUnrecoverable, err))
			}
			if detail != nil {
				encoded, err := json.Marshal(detail)
				if err != nil {
					return fail(executionError(ErrorCodeExecutionUnrecoverable, err))
				}
				return serialMachineResult{ActiveMember: activeMember, MemberBreakdown: memberBreakdown, Status: serialParked, Outputs: outputs, ArtifactTaskIDs: artifactTaskIDs, NodeID: current,
					DeliveryErrors: deliveryErrors,
					WaitKind:       WaitCorrection, WaitDetail: encoded, Usage: usage,
					UsageComplete: usageComplete, UsageIncompleteReason: usageIncompleteReason}
			}
		}
		switch node.Type {
		case machine.NodeLead, machine.NodeWorker:
			memberID, memberVersion, _ := agentNodeIdentity(node, payload)
			startedAt := time.Now().UTC()
			if start.RecordActivity != nil {
				inputNames := make([]string, 0, len(node.Inputs))
				inputSummary := make(map[string]string, len(node.Inputs))
				for name := range node.Inputs {
					inputNames = append(inputNames, name)
					if value, err := resolveValue(node.Inputs[name].Value, runInput, outputs); err == nil {
						inputSummary[name] = activityValueSummary(value)
					}
				}
				sort.Strings(inputNames)
				start.RecordActivity(ctx, "member_started", node, memberID, memberVersion,
					map[string]any{"input_names": inputNames, "input_summary": inputSummary})
			}
			durable := isDurableMemberEntry(entries[runtimeEntryKey(memberID, memberVersion)])
			callID := ""
			if activeMember != nil {
				if !durable || activeMember.NodeID != node.ID {
					return fail(executionError(ErrorCodeIdentityMismatch, errors.New("pending member differs from current node")))
				}
				callID = activeMember.CallID
			} else {
				var err error
				callID, err = usage.NextCall(start.Run.RunID, node.ID)
				if err != nil {
					return fail(executionError(ErrorCodeExecutionUnrecoverable, err))
				}
				if durable {
					ordinal, _ := usage.CallOrdinal(callID)
					activeMember = &ActiveMemberInvocation{NodeID: node.ID, CallID: callID, EntryOrdinal: ordinal}
				}
			}
			nodeCtx := execution.WithInvocationID(execution.WithInputTaskIDs(ctx, nodeInputTaskIDs(node, artifactTaskIDs)), fmt.Sprintf("%s/%d/%s", start.Run.RunSnapshotID, start.Run.ResumeGeneration, callID))
			if durable {
				if start.MemberContext == nil || start.RecordCheckpoint == nil {
					return fail(executionError(ErrorCodeRuntimeIncompatible, errors.New("durable member runner and parent checkpoint writer are required")))
				}
				if err := writeCheckpoint(); err != nil {
					return fail(err)
				}
				memberCtx := *start.MemberContext
				memberCtx.Active = *activeMember
				nodeCtx = context.WithValue(nodeCtx, workflowMemberContextKey{}, memberCtx)
			}
			correctionContext, correctionErr := buildCorrectionFrozenContext(ctx, node, payload, start.Corrections, start.LoadArtifacts)
			if correctionErr != nil {
				return fail(executionError(ErrorCodeSnapshotUnavailable, correctionErr))
			}
			output, nodeUsage, err := runAgentNode(nodeCtx, node, payload, entries, runInput, outputs, start.Corrections, correctionContext)
			if durable {
				if nodeUsage.MemberRunID != "" {
					teamID, workflowID, version, snapshotID := start.Run.TeamID, start.Run.WorkflowID, start.Run.WorkflowVersion, start.Run.RunSnapshotID
					memberBreakdown[nodeUsage.MemberRunID] = loomruntime.TerminalChildBreakdownV3{
						RunID: nodeUsage.MemberRunID, ParentRunID: start.Run.RunID, ParentSeq: int64(activeMember.EntryOrdinal) + 1,
						Agent:  entries[runtimeEntryKey(memberID, memberVersion)].Bundle.Agent.Name,
						TeamID: &teamID, WorkflowID: &workflowID, WorkflowVersion: &version, RunSnapshotID: &snapshotID,
						SelfExclusive: loomruntime.TerminalUsage{InputTokens: nodeUsage.Totals.InputTokens, OutputTokens: nodeUsage.Totals.OutputTokens, CostUSD: nodeUsage.Totals.CostUSD, ToolCalls: nodeUsage.Totals.ToolCalls},
					}
				}
				for _, receipt := range nodeUsage.MemberReceipts {
					attemptID := nodePhysicalUsageAttemptID(callID, receipt.AttemptID, 0)
					if usageErr := usage.StartAttempt(callID, attemptID); usageErr != nil {
						return fail(usageErr)
					}
					if usageErr := usage.ConfirmAttemptWithMetadata(callID, attemptID, receipt.Usage, receipt.ToolCalls, receipt.Metadata); usageErr != nil {
						return fail(usageErr)
					}
				}
				if nodeUsage.MemberUsageIncomplete {
					usageComplete, usageIncompleteReason = false, "member_model_response_lost"
				}
			} else if len(nodeUsage.CLIAttempts) == 0 {
				attemptID := nodeUsageAttemptID(callID)
				if startErr := usage.StartAttempt(callID, attemptID); startErr != nil {
					return fail(executionError(ErrorCodeExecutionUnrecoverable, startErr))
				}
				if confirmErr := usage.ConfirmAttemptWithMetadata(callID, attemptID, contract.Usage{
					InputTokens: nodeUsage.Totals.InputTokens, OutputTokens: nodeUsage.Totals.OutputTokens,
					CostUSD: nodeUsage.Totals.CostUSD,
				}, nodeUsage.Totals.ToolCalls, loomruntime.UsageAttemptMetadata{
					HasTokens: nodeUsage.Coverage.HasTokens, HasCost: nodeUsage.Coverage.HasCost,
				}); confirmErr != nil {
					return fail(executionError(ErrorCodeExecutionUnrecoverable, confirmErr))
				}
			} else {
				for index, physical := range nodeUsage.CLIAttempts {
					attemptID := nodePhysicalUsageAttemptID(callID, physical.AttemptID, index)
					if startErr := usage.StartAttempt(callID, attemptID); startErr != nil {
						return fail(executionError(ErrorCodeExecutionUnrecoverable, startErr))
					}
					if confirmErr := usage.ConfirmAttemptWithMetadata(callID, attemptID, contract.Usage{
						InputTokens: physical.InputTokens, OutputTokens: physical.OutputTokens, CostUSD: physical.CostUSD,
					}, physical.ToolCalls, loomruntime.UsageAttemptMetadata{
						HasTokens: physical.HasTokens, HasCost: physical.HasCost, Source: physical.Source,
					}); confirmErr != nil {
						return fail(executionError(ErrorCodeExecutionUnrecoverable, confirmErr))
					}
				}
			}
			// A CLI runtime agent has no usage receipt: the candidate run
			// executes it normally, records zero measured usage, and marks
			// the result usage-incomplete instead of failing closed.
			if len(nodeUsage.CLIAttempts) > 0 && (!nodeUsage.Coverage.HasTokens || !nodeUsage.Coverage.HasCost) && usageIncompleteReason == "" {
				usageComplete = false
				usageIncompleteReason = UsageIncompleteReasonCLINode
				if nodeUsage.Coverage.HasTokens || nodeUsage.Coverage.HasCost {
					usageIncompleteReason = UsageIncompleteReasonCLIDimensions
				}
			}
			if len(nodeUsage.Events) == 0 && start.LoadObservedEvents != nil {
				nodeUsage.Events = start.LoadObservedEvents(ctx, node, memberID)
			}
			if start.RecordActivity != nil {
				for eventIndex, event := range nodeUsage.Events {
					kind := ""
					switch event.Kind {
					case "tool_call":
						kind = "tool_started"
					case "tool_result":
						kind = "tool_completed"
					default:
						continue
					}
					callID := strings.TrimSpace(event.CallID)
					if callID == "" {
						callID = fmt.Sprintf("%s:%d", node.ID, eventIndex)
					}
					detail := map[string]any{
						"tool_call_id": callID, "tool_name": event.Tool,
						"input": event.Input, "output": event.Output,
					}
					if kind == "tool_completed" {
						status := event.Status
						if status != "error" {
							status = "ok"
						}
						detail["status"] = status
					}
					start.RecordActivity(ctx, kind, node, memberID, memberVersion, detail)
				}
			}
			if err != nil {
				if start.RecordArtifact != nil {
					for _, artifact := range nodeUsage.Artifacts {
						if recordErr := start.RecordArtifact(ctx, node, deliverable.WorkflowArtifact{
							Path: artifact.Path, ContentType: artifact.ContentType, Content: artifact.Content,
						}, false); recordErr != nil {
							return fail(executionError(ErrorCodeDeliveryUnavailable, errors.Join(err, recordErr)))
						}
					}
				}
				next, routed := edgeTarget(edges[current], machine.RouteFailure)
				recoveryBlocked := durable && errors.Is(err, loomruntime.ErrMemberOutcomeUnknown)
				if recoveryBlocked {
					routed = false
				}
				failure := ClassifyFailure(err)
				retryable := !routed && failure.Class == FailureClassInfrastructure && failure.Retryable
				if start.RecordActivity != nil {
					start.RecordActivity(ctx, "member_failed", node, memberID, memberVersion, map[string]any{
						"duration_ms": time.Since(startedAt).Milliseconds(), "error_code": string(executionErrorCode(err)),
						"failure_class": failure.Class, "failure_reason": failure.Reason, "retryable": retryable,
					})
				}
				if routed {
					activeMember = nil
					current = next
					continue
				}
				if retryable || recoveryBlocked {
					detail, encodeErr := json.Marshal(RuntimeWaitDetailV1{
						SchemaVersion: 1, WaitType: "runtime", NodeID: node.ID,
						RecoveryBlocked: recoveryBlocked,
					})
					if encodeErr != nil {
						return fail(executionError(ErrorCodeExecutionUnrecoverable, encodeErr))
					}
					return serialMachineResult{ActiveMember: activeMember, MemberBreakdown: memberBreakdown, Status: serialParked, Outputs: outputs, ArtifactTaskIDs: artifactTaskIDs, NodeID: node.ID,
						DeliveryErrors: deliveryErrors,
						WaitKind:       WaitRuntime, WaitDetail: detail, Usage: usage,
						UsageComplete: usageComplete, UsageIncompleteReason: usageIncompleteReason}
				}
				return fail(err)
			}
			if nodeUsage.MemberPause != nil {
				detail, encodeErr := json.Marshal(RuntimeWaitDetailV1{SchemaVersion: 1, WaitType: "runtime", NodeID: node.ID, MemberBudgetPause: nodeUsage.MemberPause})
				if encodeErr != nil {
					return fail(encodeErr)
				}
				if start.RecordActivity != nil {
					start.RecordActivity(ctx, "member_paused", node, memberID, memberVersion, map[string]any{"budget": nodeUsage.MemberPause})
				}
				return serialMachineResult{ActiveMember: activeMember, MemberBreakdown: memberBreakdown, Status: serialParked, Outputs: outputs, ArtifactTaskIDs: artifactTaskIDs, NodeID: node.ID,
					DeliveryErrors: deliveryErrors, WaitKind: WaitRuntime, WaitDetail: detail, Usage: usage, UsageComplete: usageComplete, UsageIncompleteReason: usageIncompleteReason}
			}
			activeMember = nil
			outputs[node.ID] = output
			delete(artifactTaskIDs, node.ID)
			if durable && nodeUsage.MemberRunID != "" {
				artifactTaskIDs[node.ID] = []string{"member:" + nodeUsage.MemberRunID}
			}
			for _, attempt := range nodeUsage.CLIAttempts {
				if attempt.AttemptID != "" {
					artifactTaskIDs[node.ID] = []string{attempt.AttemptID}
				}
			}
			delete(deliveryErrors, node.ID)
			if nodeUsage.DeliveryError != "" {
				deliveryErrors[node.ID] = nodeUsage.DeliveryError
			}
			if start.RecordActivity != nil {
				start.RecordActivity(ctx, "member_completed", node, memberID, memberVersion, map[string]any{
					"duration_ms": time.Since(startedAt).Milliseconds(), "tool_calls": nodeUsage.Totals.ToolCalls,
					"input_tokens": nodeUsage.Totals.InputTokens, "output_tokens": nodeUsage.Totals.OutputTokens,
				})
			}
			if start.RecordOutput != nil {
				if err := start.RecordOutput(ctx, node, output, false); err != nil {
					return fail(executionError(ErrorCodeDeliveryUnavailable, err))
				}
			}
			if start.RecordArtifact != nil {
				for _, artifact := range nodeUsage.Artifacts {
					if err := start.RecordArtifact(ctx, node, deliverable.WorkflowArtifact{
						Path: artifact.Path, ContentType: artifact.ContentType, Content: artifact.Content,
					}, false); err != nil {
						return fail(executionError(ErrorCodeDeliveryUnavailable, err))
					}
				}
			}
			if projection, joinNodeID, joinedArtifactIDs, replayed, replayErr := mergeFanoutCorrectionResult(
				node, payload, output, artifactTaskIDs[node.ID], start.Corrections,
			); replayErr != nil {
				return fail(executionError(ErrorCodeSnapshotUnavailable, replayErr))
			} else if replayed {
				outputs[joinNodeID] = projection
				artifactTaskIDs[joinNodeID] = joinedArtifactIDs
				delete(deliveryErrors, joinNodeID)
				current = joinNodeID
				continue
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
			delete(artifactTaskIDs, node.ID)
			delete(deliveryErrors, node.ID)
			config := node.Config.(machine.TransformConfig)
			references := append([]machine.ValueRef(nil), config.Items...)
			if config.Value != nil {
				references = append(references, *config.Value)
			}
			for _, reference := range config.Fields {
				references = append(references, reference)
			}
			for _, reference := range references {
				if reference.Source == machine.ValueNodeOutput {
					artifactTaskIDs[node.ID] = append(artifactTaskIDs[node.ID], artifactTaskIDs[reference.NodeID]...)
				}
				if message := valueDeliveryError(reference, outputs, deliveryErrors); message != "" &&
					(deliveryErrors[node.ID] == "" || message < deliveryErrors[node.ID]) {
					deliveryErrors[node.ID] = message
				}
			}
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
			return serialMachineResult{ActiveMember: activeMember, MemberBreakdown: memberBreakdown,
				Status: serialParked, Outputs: outputs, ArtifactTaskIDs: artifactTaskIDs, NodeID: joinNodeID,
				DeliveryErrors: deliveryErrors,
				WaitKind:       WaitFanout, WaitDetail: detail, Usage: usage,
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
			// Historical collection diagnostics remain in the execution record.
			// Business file requirements are evaluated by the delivery verifier;
			// a model's prose about a file cannot change the engine's result.
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
			var artifacts []deliverable.WorkflowArtifact
			var observations []deliverable.SourceObservation
			var selection *deliverable.OutputSelection
			if start.LoadArtifacts != nil && config.Result.Source == machine.ValueNodeOutput {
				artifacts, err = start.LoadArtifacts(ctx, artifactTaskIDs[config.Result.NodeID])
				if err != nil {
					return fail(executionError(ErrorCodeDeliveryUnavailable, err))
				}
			}
			if config.Result.Source == machine.ValueNodeOutput && start.LoadArtifactObservations != nil {
				observations, err = start.LoadArtifactObservations(ctx, artifactTaskIDs[config.Result.NodeID])
				if err != nil {
					return fail(executionError(ErrorCodeDeliveryUnavailable, err))
				}
			} else if config.Result.Source == machine.ValueRunInput || config.Result.Source == machine.ValueLiteral {
				digest, err := deliverable.CanonicalJSONDigest(encoded)
				if err != nil {
					return fail(executionError(ErrorCodeOutputInvalid, err))
				}
				selection = &deliverable.OutputSelection{Kind: string(config.Result.Source), ValueDigest: digest}
			}
			if start.RecordDelivery != nil {
				if err := start.RecordDelivery(ctx, node.ID, node.Label, string(node.Type), output, artifacts, observations, selection); err != nil {
					return fail(executionError(ErrorCodeDeliveryUnavailable, err))
				}
			} else {
				if start.RecordArtifact != nil {
					for _, artifact := range artifacts {
						if err := start.RecordArtifact(ctx, node, artifact, true); err != nil {
							return fail(executionError(ErrorCodeDeliveryUnavailable, err))
						}
					}
				}
				if start.RecordOutput != nil {
					if err := start.RecordOutput(ctx, node, output, true); err != nil {
						return fail(executionError(ErrorCodeDeliveryUnavailable, err))
					}
				}
			}
			return serialMachineResult{ActiveMember: activeMember, MemberBreakdown: memberBreakdown,
				Status: serialCompleted, Output: output, Usage: usage,
				UsageComplete: usageComplete, UsageIncompleteReason: usageIncompleteReason,
			}
		case machine.NodeWait:
			config, ok := node.Config.(machine.WaitConfig)
			if !ok {
				return fail(executionError(ErrorCodeRuntimeIncompatible, fmt.Errorf("wait node %q config is invalid", node.ID)))
			}
			if config.EffectiveKind() == machine.WaitKindHuman {
				if config.Task == nil {
					return fail(executionError(ErrorCodeRuntimeIncompatible, fmt.Errorf("human wait node %q task is invalid", node.ID)))
				}
				successNodeID, present := edgeTarget(edges[current], machine.RouteSuccess)
				if !present {
					return fail(executionError(ErrorCodeRuntimeIncompatible, fmt.Errorf("human wait node %q lacks a success edge", node.ID)))
				}
				now := start.Now
				if now.IsZero() {
					now = time.Now().UTC()
				}
				var deadlineAt *time.Time
				timeoutNodeID := ""
				if config.TimeoutSeconds != nil {
					var present bool
					timeoutNodeID, present = edgeTarget(edges[current], machine.RouteTimeout)
					if !present {
						return fail(executionError(ErrorCodeRuntimeIncompatible, fmt.Errorf("human wait node %q lacks a timeout edge", node.ID)))
					}
					deadline := now.Add(time.Duration(*config.TimeoutSeconds) * time.Second).UTC().Truncate(time.Second)
					deadlineAt = &deadline
				}
				detail, err := json.Marshal(HumanWaitDetailV1{
					SchemaVersion: 1, WaitType: "human", NodeID: node.ID, SuccessNodeID: successNodeID, TimeoutNodeID: timeoutNodeID,
					ResumeSchema: config.ResumeSchema,
					Task:         HumanTaskDetail{Title: config.Task.Title, Instructions: config.Task.Instructions, AudienceRef: config.Task.AudienceRef},
					DeadlineAt:   deadlineAt,
				})
				if err != nil || len(detail) > HumanWaitDetailMaxBytes {
					if err == nil {
						err = errors.New("human wait detail exceeds 16KiB")
					}
					return fail(executionError(ErrorCodeRuntimeIncompatible, err))
				}
				return serialMachineResult{ActiveMember: activeMember, MemberBreakdown: memberBreakdown,
					Status: serialParked, Outputs: outputs, ArtifactTaskIDs: artifactTaskIDs, NodeID: node.ID,
					DeliveryErrors: deliveryErrors,
					WaitKind:       WaitHuman, WaitDetail: detail, Usage: usage,
					UsageComplete: usageComplete, UsageIncompleteReason: usageIncompleteReason,
				}
			}
			if config.TimeoutSeconds == nil || *config.TimeoutSeconds < 1 ||
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
			return serialMachineResult{ActiveMember: activeMember, MemberBreakdown: memberBreakdown,
				Status: serialParked, Outputs: outputs, ArtifactTaskIDs: artifactTaskIDs, NodeID: node.ID,
				DeliveryErrors: deliveryErrors,
				WaitKind:       WaitTimer, WaitDetail: detail, Usage: usage,
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

func valueDeliveryError(ref machine.ValueRef, outputs map[string]any, deliveryErrors map[string]string) string {
	if ref.Source != machine.ValueNodeOutput {
		return ""
	}
	if _, present := outputs[ref.NodeID]; !present && ref.Default != nil {
		return valueDeliveryError(*ref.Default, outputs, deliveryErrors)
	}
	return deliveryErrors[ref.NodeID]
}

func activityValueSummary(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	const maxRunes = 320
	runes := []rune(string(encoded))
	if len(runes) <= maxRunes {
		return string(runes)
	}
	return string(runes[:maxRunes]) + "…"
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

func buildCorrectionWaitDetail(
	graph machine.GraphDefinition,
	payload frozen.ArtifactPayloadV1,
	currentNodeID string,
	outputs map[string]any,
	item Correction,
) (CorrectionWaitDetailV1, error) {
	if currentNodeID == "" || item.Status != CorrectionRequested {
		return CorrectionWaitDetailV1{}, errors.New("correction cannot be planned at this boundary")
	}
	restartNodeID := currentNodeID
	var fanoutReplay *CorrectionFanoutReplayV1
	if item.TargetKind == "team" {
		restartNodeID = graph.EntryNodeID
	} else if item.TargetKind == "member" {
		matching := make([]string, 0)
		for _, node := range graph.Nodes {
			memberID, _, present := agentNodeIdentity(node, payload)
			if present && memberID == item.TargetMemberID {
				matching = append(matching, node.ID)
			}
		}
		if len(matching) == 0 {
			return CorrectionWaitDetailV1{}, errors.New("target member has no node in the frozen workflow")
		}
		if replay, ok := buildFanoutCorrectionReplay(graph, currentNodeID, matching, outputs[currentNodeID]); ok {
			restartNodeID = replay.TargetNodeID
			fanoutReplay = &replay
		} else {
			for _, nodeID := range matching {
				if _, completed := outputs[nodeID]; completed {
					restartNodeID = nodeID
					break
				}
			}
			// Older fanout projections do not carry per-leg immutable source
			// identities. Preserve the conservative whole-segment behavior for
			// those runs instead of guessing which artifacts belong to siblings.
			if restartNodeID == currentNodeID {
				if parallelNodeID := machine.OwningParallelNode(graph, currentNodeID, matching); parallelNodeID != "" {
					restartNodeID = parallelNodeID
				}
			}
		}
	}
	affectedSet := downstreamNodeSet(graph, restartNodeID)
	if len(affectedSet) == 0 {
		return CorrectionWaitDetailV1{}, errors.New("correction restart node is outside the frozen workflow")
	}
	affected := make([]string, 0, len(affectedSet))
	for nodeID := range affectedSet {
		affected = append(affected, nodeID)
	}
	sort.Strings(affected)
	preserved := make([]string, 0)
	for nodeID := range outputs {
		if _, affected := affectedSet[nodeID]; !affected {
			preserved = append(preserved, nodeID)
		}
	}
	if fanoutReplay != nil {
		for _, leg := range fanoutReplay.Legs {
			if leg.NodeID != fanoutReplay.TargetNodeID {
				preserved = append(preserved, leg.NodeID)
			}
		}
		sort.Strings(preserved)
		preserved = compactStrings(preserved)
	}
	sort.Strings(preserved)
	return CorrectionWaitDetailV1{
		SchemaVersion: 1, WaitType: "correction", CorrectionID: item.CorrectionID,
		TargetKind: item.TargetKind, TargetMemberID: item.TargetMemberID, Instruction: item.Instruction,
		SafeNodeID: currentNodeID, RestartNodeID: restartNodeID,
		AffectedNodeIDs: affected, PreservedNodeIDs: preserved, FanoutReplay: fanoutReplay,
	}, nil
}

func buildFanoutCorrectionReplay(
	graph machine.GraphDefinition,
	joinNodeID string,
	matching []string,
	value any,
) (CorrectionFanoutReplayV1, bool) {
	if len(matching) != 1 || value == nil {
		return CorrectionFanoutReplayV1{}, false
	}
	parallelNodeID := machine.OwningParallelNode(graph, joinNodeID, matching)
	if parallelNodeID == "" {
		return CorrectionFanoutReplayV1{}, false
	}
	expected := map[string]bool{}
	for _, edge := range graph.Edges {
		if edge.FromNodeID == parallelNodeID && edge.Route == machine.RouteBranch {
			expected[edge.ToNodeID] = true
		}
	}
	return buildFanoutCorrectionReplaySnapshot(parallelNodeID, joinNodeID, matching[0], expected, value)
}

func buildFanoutCorrectionReplaySnapshot(
	parallelNodeID, joinNodeID, targetNodeID string,
	expected map[string]bool,
	value any,
) (CorrectionFanoutReplayV1, bool) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return CorrectionFanoutReplayV1{}, false
	}
	var projection fanoutJoinProjectionV1
	if decodeExact(encoded, &projection) != nil || projection.SchemaVersion != 1 || projection.Decision != "succeeded" || len(projection.Errors) != 0 {
		return CorrectionFanoutReplayV1{}, false
	}
	if len(expected) < 2 || len(projection.Legs) != len(expected) || len(projection.Results) != len(expected) {
		return CorrectionFanoutReplayV1{}, false
	}
	replay := CorrectionFanoutReplayV1{SchemaVersion: 1, ParallelNodeID: parallelNodeID, JoinNodeID: joinNodeID, TargetNodeID: targetNodeID}
	seen, seenOrdinals := map[string]bool{}, map[int]bool{}
	for _, leg := range projection.Legs {
		projectedResult, resultPresent := projection.Results[leg.NodeID]
		legDigest, legDigestErr := deliverable.CanonicalJSONDigest(leg.Result)
		projectedDigest, projectedDigestErr := deliverable.CanonicalJSONDigest(projectedResult)
		if !expected[leg.NodeID] || seen[leg.NodeID] || seenOrdinals[leg.BranchOrdinal] || leg.LegID == "" || leg.BranchOrdinal < 0 ||
			leg.DecisionDisposition != string(fanout.LegDecisionSucceeded) || !json.Valid(leg.Result) || leg.ArtifactTaskIDs == nil {
			return CorrectionFanoutReplayV1{}, false
		}
		if !resultPresent || legDigestErr != nil || projectedDigestErr != nil || legDigest != projectedDigest {
			return CorrectionFanoutReplayV1{}, false
		}
		seen[leg.NodeID], seenOrdinals[leg.BranchOrdinal] = true, true
		replay.Legs = append(replay.Legs, CorrectionFanoutReplayLegV1{
			LegID: leg.LegID, NodeID: leg.NodeID, BranchOrdinal: leg.BranchOrdinal,
			Result: append(json.RawMessage(nil), leg.Result...), ArtifactTaskIDs: append([]string{}, leg.ArtifactTaskIDs...),
		})
	}
	for ordinal := 0; ordinal < len(replay.Legs); ordinal++ {
		if !seenOrdinals[ordinal] {
			return CorrectionFanoutReplayV1{}, false
		}
	}
	sort.Slice(replay.Legs, func(i, j int) bool { return replay.Legs[i].BranchOrdinal < replay.Legs[j].BranchOrdinal })
	hash, err := correctionFanoutSourceProjectionHash(replay)
	if err != nil {
		return CorrectionFanoutReplayV1{}, false
	}
	replay.SourceProjectionHash = hash
	return replay, true
}

func correctionFanoutSourceProjectionHash(replay CorrectionFanoutReplayV1) (string, error) {
	projection := fanoutJoinProjectionV1{
		SchemaVersion: 1,
		Decision:      "succeeded",
		Results:       make(map[string]json.RawMessage, len(replay.Legs)),
		Errors:        map[string]string{},
		Legs:          make([]fanoutJoinLegV1, 0, len(replay.Legs)),
	}
	for _, leg := range replay.Legs {
		result := append(json.RawMessage(nil), leg.Result...)
		projection.Results[leg.NodeID] = result
		projection.Legs = append(projection.Legs, fanoutJoinLegV1{
			NodeID: leg.NodeID, LegID: leg.LegID, BranchOrdinal: leg.BranchOrdinal,
			DecisionDisposition: string(fanout.LegDecisionSucceeded), Result: result,
			Error: json.RawMessage("null"), ArtifactTaskIDs: append([]string{}, leg.ArtifactTaskIDs...),
		})
	}
	sort.Slice(projection.Legs, func(i, j int) bool { return projection.Legs[i].BranchOrdinal < projection.Legs[j].BranchOrdinal })
	encoded, err := json.Marshal(projection)
	if err != nil {
		return "", err
	}
	return deliverable.CanonicalJSONDigest(encoded)
}

func compactStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	result := values[:1]
	for _, value := range values[1:] {
		if value != result[len(result)-1] {
			result = append(result, value)
		}
	}
	return result
}

func fanoutReplayForNode(
	node machine.Node,
	payload frozen.ArtifactPayloadV1,
	corrections []CorrectionDirectiveV1,
) *CorrectionFanoutReplayV1 {
	for index := len(corrections) - 1; index >= 0; index-- {
		correction := corrections[index]
		if correction.FanoutReplay != nil && correction.FanoutReplay.TargetNodeID == node.ID &&
			correctionAppliesToNode(correction, node, payload) {
			return correction.FanoutReplay
		}
	}
	return nil
}

func buildCorrectionFrozenContext(
	ctx context.Context,
	node machine.Node,
	payload frozen.ArtifactPayloadV1,
	corrections []CorrectionDirectiveV1,
	loader func(context.Context, []string) ([]deliverable.WorkflowArtifact, error),
) (string, error) {
	replay := fanoutReplayForNode(node, payload, corrections)
	if replay == nil {
		return "", nil
	}
	if err := validateCorrectionFanoutReplay(*replay, "member", "frozen-member", node.ID); err != nil {
		return "", err
	}
	var target *CorrectionFanoutReplayLegV1
	for i := range replay.Legs {
		if replay.Legs[i].NodeID == replay.TargetNodeID {
			target = &replay.Legs[i]
			break
		}
	}
	if target == nil || loader == nil {
		return "", errors.New("frozen correction target inputs are unavailable")
	}
	artifacts, err := verifiedCorrectionTargetArtifacts(ctx, *replay, loader)
	if err != nil {
		return "", err
	}
	return correctionFrozenPromptContext(*replay, *target, artifacts)
}

func verifiedCorrectionTargetArtifacts(
	ctx context.Context,
	replay CorrectionFanoutReplayV1,
	loader func(context.Context, []string) ([]deliverable.WorkflowArtifact, error),
) ([]deliverable.WorkflowArtifact, error) {
	var artifacts []deliverable.WorkflowArtifact
	for _, leg := range replay.Legs {
		loaded, err := loader(ctx, leg.ArtifactTaskIDs)
		if err != nil {
			return nil, fmt.Errorf("load frozen fanout leg %s artifacts: %w", leg.NodeID, err)
		}
		actualManifest, err := correctionArtifactManifest(loaded)
		if err != nil {
			return nil, err
		}
		if !reflect.DeepEqual(actualManifest, leg.Artifacts) {
			return nil, fmt.Errorf("frozen correction artifact manifest changed for leg %s", leg.NodeID)
		}
		if leg.NodeID == replay.TargetNodeID {
			artifacts = loaded
		}
	}
	return artifacts, nil
}

func correctionFrozenPromptContext(
	replay CorrectionFanoutReplayV1,
	target CorrectionFanoutReplayLegV1,
	artifacts []deliverable.WorkflowArtifact,
) (string, error) {
	type promptArtifact struct {
		Path        string `json:"path"`
		ContentType string `json:"content_type"`
		SHA256      string `json:"sha256"`
		Content     string `json:"content"`
	}
	promptArtifacts := make([]promptArtifact, 0, len(artifacts))
	total := 0
	for _, artifact := range artifacts {
		size := len([]byte(artifact.Content))
		total += size
		if total > maxCorrectionFrozenArtifactBytes {
			return "", fmt.Errorf("correction artifacts exceed %d bytes", maxCorrectionFrozenArtifactBytes)
		}
		digest := sha256.Sum256([]byte(artifact.Content))
		hash := hex.EncodeToString(digest[:])
		promptArtifacts = append(promptArtifacts, promptArtifact{
			Path: artifact.Path, ContentType: artifact.ContentType, SHA256: hash, Content: artifact.Content,
		})
	}
	sort.Slice(promptArtifacts, func(i, j int) bool { return promptArtifacts[i].Path < promptArtifacts[j].Path })
	encoded, err := json.Marshal(struct {
		SourceProjectionHash string           `json:"source_projection_hash"`
		PriorResult          json.RawMessage  `json:"prior_result"`
		Artifacts            []promptArtifact `json:"artifacts"`
	}{
		SourceProjectionHash: replay.SourceProjectionHash,
		PriorResult:          append(json.RawMessage(nil), target.Result...),
		Artifacts:            promptArtifacts,
	})
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func mergeFanoutCorrectionResult(
	node machine.Node,
	payload frozen.ArtifactPayloadV1,
	output any,
	targetArtifactTaskIDs []string,
	corrections []CorrectionDirectiveV1,
) (fanoutJoinProjectionV1, string, []string, bool, error) {
	replay := fanoutReplayForNode(node, payload, corrections)
	if replay == nil {
		return fanoutJoinProjectionV1{}, "", nil, false, nil
	}
	projection, joinedArtifactIDs, err := mergeFanoutCorrectionReplayResult(*replay, output, targetArtifactTaskIDs)
	if err != nil {
		return fanoutJoinProjectionV1{}, "", nil, false, err
	}
	return projection, replay.JoinNodeID, joinedArtifactIDs, true, nil
}

func mergeFanoutCorrectionReplayResult(
	replay CorrectionFanoutReplayV1,
	output any,
	targetArtifactTaskIDs []string,
) (fanoutJoinProjectionV1, []string, error) {
	encodedOutput, err := json.Marshal(output)
	if err != nil {
		return fanoutJoinProjectionV1{}, nil, err
	}
	projection := fanoutJoinProjectionV1{
		SchemaVersion: 1, Decision: "succeeded", Results: make(map[string]json.RawMessage, len(replay.Legs)),
		Errors: map[string]string{}, Legs: make([]fanoutJoinLegV1, 0, len(replay.Legs)),
	}
	joinedArtifactIDs := make([]string, 0)
	seenArtifactIDs := map[string]bool{}
	for _, leg := range replay.Legs {
		result := append(json.RawMessage(nil), leg.Result...)
		artifactIDs := append([]string{}, leg.ArtifactTaskIDs...)
		if leg.NodeID == replay.TargetNodeID {
			result = append(json.RawMessage(nil), encodedOutput...)
			artifactIDs = append([]string{}, targetArtifactTaskIDs...)
		}
		projection.Results[leg.NodeID] = result
		projection.Legs = append(projection.Legs, fanoutJoinLegV1{
			NodeID: leg.NodeID, LegID: leg.LegID, BranchOrdinal: leg.BranchOrdinal,
			DecisionDisposition: string(fanout.LegDecisionSucceeded), Result: result,
			Error: json.RawMessage("null"), ArtifactTaskIDs: artifactIDs,
		})
		for _, id := range artifactIDs {
			if id != "" && !seenArtifactIDs[id] {
				seenArtifactIDs[id] = true
				joinedArtifactIDs = append(joinedArtifactIDs, id)
			}
		}
	}
	sort.Slice(projection.Legs, func(i, j int) bool { return projection.Legs[i].BranchOrdinal < projection.Legs[j].BranchOrdinal })
	return projection, joinedArtifactIDs, nil
}

func downstreamNodeSet(graph machine.GraphDefinition, start string) map[string]struct{} {
	adjacent := make(map[string][]string)
	present := false
	for _, node := range graph.Nodes {
		if node.ID == start {
			present = true
		}
	}
	if !present {
		return nil
	}
	for _, edge := range graph.Edges {
		adjacent[edge.FromNodeID] = append(adjacent[edge.FromNodeID], edge.ToNodeID)
	}
	visited := map[string]struct{}{start: {}}
	queue := []string{start}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, next := range adjacent[current] {
			if _, seen := visited[next]; seen {
				continue
			}
			visited[next] = struct{}{}
			queue = append(queue, next)
		}
	}
	return visited
}

func agentNodeIdentity(node machine.Node, payload frozen.ArtifactPayloadV1) (string, int64, bool) {
	switch config := node.Config.(type) {
	case machine.LeadConfig:
		memberID := payload.Team.LeadAgentID
		for _, bundle := range payload.Bundles {
			if bundle.Agent.AgentID == memberID {
				return memberID, bundle.Agent.AgentVersion, true
			}
		}
	case machine.WorkerConfig:
		return config.AgentID, config.AgentVersion, true
	}
	return "", 0, false
}

func correctionAppliesToNode(correction CorrectionDirectiveV1, node machine.Node, payload frozen.ArtifactPayloadV1) bool {
	affected := false
	for _, nodeID := range correction.AffectedNodes {
		if nodeID == node.ID {
			affected = true
			break
		}
	}
	if !affected {
		return false
	}
	if correction.TargetKind == "team" {
		return true
	}
	memberID, _, present := agentNodeIdentity(node, payload)
	return present && memberID == correction.TargetMemberID
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
	LegID               string          `json:"leg_id,omitempty"`
	BranchOrdinal       int             `json:"branch_ordinal,omitempty"`
	DecisionDisposition string          `json:"decision_disposition"`
	Result              json.RawMessage `json:"result"`
	Error               json.RawMessage `json:"error"`
	// A non-nil slice proves that the projection was created by a runtime
	// which recorded the immutable physical sources for this exact leg. Empty
	// is a valid, known inventory for a result that exported no files.
	ArtifactTaskIDs []string `json:"artifact_task_ids"`
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
	corrections []CorrectionDirectiveV1,
	frozenCorrectionContext string,
) (any, nodeUsageReport, error) {
	ctx = execution.WithNodeID(ctx, node.ID)
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
					return nil, nodeUsageReport{}, executionError(
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
		return nil, nodeUsageReport{}, executionError(
			ErrorCodeRuntimeIncompatible,
			fmt.Errorf("agent node %q config is invalid", node.ID),
		)
	}
	entry, ok := entries[runtimeEntryKey(agentID, agentVersion)]
	if !ok || (entry.Graph == nil && entry.CLI == nil) || (entry.Graph != nil && entry.CLI != nil) {
		return nil, nodeUsageReport{}, executionError(
			ErrorCodeRuntimeIncompatible,
			fmt.Errorf("frozen runtime entry for node %q is unavailable or ambiguous", node.ID),
		)
	}
	inputs := make(map[string]any, len(node.Inputs))
	for name, binding := range node.Inputs {
		value, err := resolveValue(binding.Value, runInput, outputs)
		if err != nil {
			return nil, nodeUsageReport{}, executionError(ErrorCodeExecutionUnrecoverable, err)
		}
		inputs[name] = value
	}
	encodedInputs, err := json.Marshal(inputs)
	if err != nil {
		return nil, nodeUsageReport{}, executionError(ErrorCodeExecutionUnrecoverable, err)
	}
	prompt := instruction + "\n\nInputs:\n" + string(encodedInputs)
	for _, correction := range corrections {
		if correctionAppliesToNode(correction, node, payload) {
			prompt += "\n\nConfirmed user correction (apply to this execution):\n" + correction.Instruction
		}
	}
	if frozenCorrectionContext != "" {
		prompt += "\n\nFrozen prior execution snapshot (read-only; revise only within the confirmed correction):\n" + frozenCorrectionContext
	}
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
			result workflow.RuntimeCLIResult
			err    error
		}
		outcomes := make(chan cliOutcome, 1)
		go func() {
			result, err := entry.CLI.ExecuteAccounted(nodeCtx, prompt)
			outcomes <- cliOutcome{result: result, err: err}
		}()
		var outcome cliOutcome
		select {
		case outcome = <-outcomes:
		case <-nodeCtx.Done():
			// Engine adapters finish their parser loop after cancellation so a
			// terminal receipt already read from stdout is not discarded here.
			outcome = <-outcomes
		}
		usage, usageErr := runtimeCLIUsageReport(outcome.result)
		if usageErr != nil {
			return nil, nodeUsageReport{}, executionError(ErrorCodeExecutionUnrecoverable, usageErr)
		}
		if timeout := timeoutErr(); timeout != nil {
			return nil, usage, timeout
		}
		if outcome.err != nil {
			return nil, usage, executionError(ErrorCodeExecutionUnrecoverable, outcome.err)
		}
		output := outcome.result.Output
		if node.Output != nil && node.Output.Type == machine.ValueText {
			var err error
			output, err = outcome.result.TextOutput()
			if err != nil {
				return nil, usage, executionError(ErrorCodeOutputInvalid, err)
			}
		}
		normalizedOutput, err := normalizeAgentNodeOutput(node, output)
		if err != nil {
			return nil, usage, err
		}
		return normalizedOutput, usage, nil
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
		return nil, nodeUsageReport{}, executionError(
			ErrorCodeExecutionUnrecoverable,
			fmt.Errorf("initialize frozen graph usage for node %q: %w", node.ID, err),
		)
	}
	execCtx := loomruntime.WithUsageRunScope(nodeCtx)
	execCtx = context.WithValue(execCtx, runtimeActivityScopeKey{}, runtimeActivityScope{
		NodeID: node.ID, MemberID: agentID, MemberVersion: agentVersion,
	})
	// Pre-bind the usage scope so hook-less frozen descriptors still confirm
	// usage into graph state; descriptors that install before-step hooks
	// rebind to the real step name on their first step.
	if err := loomruntime.BindUsageBeforeStep(execCtx, "graph_entry", graphState); err != nil {
		return nil, nodeUsageReport{}, executionError(
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
		var result *loom.RunResult
		var err error
		if isDurableMemberEntry(entry) {
			member, ok := execCtx.Value(workflowMemberContextKey{}).(workflowMemberContext)
			if !ok || member.Runner == nil {
				err = errors.New("durable member entry requires a serial workflow owner; parallel entry is unsupported")
			} else {
				var attribution loomruntime.TerminalAttribution
				attribution, err = member.attribution()
				if err == nil {
					delete(graphState, "__run_id")
					result, err = member.Runner.Run(execCtx, loomruntime.MemberRequest{
						WorkspaceID: member.Run.WorkspaceID, ParentRunID: member.Run.RunID,
						RunSnapshotID: member.Run.RunSnapshotID, NodeID: node.ID, CallID: member.Active.CallID,
						ParentGeneration: int64(member.Run.Generation), Bundle: *entry.Bundle, ResumeGrantID: member.Active.ResumeGrantID,
						ArtifactHash: member.ArtifactHash, Graph: entry.Graph, Input: graphState,
						Attribution: attribution, ParentGuard: member.Guard,
						RetryableFailure: func(err error) bool { return ClassifyFailure(err).Retryable },
					})
				}
			}
		} else {
			result, err = entry.Graph.Run(execCtx, graphState, loom.NewMemStore())
		}
		graphOutcomes <- graphOutcome{result: result, err: err}
	}()
	var graphResult graphOutcome
	select {
	case graphResult = <-graphOutcomes:
	case <-nodeCtx.Done():
		if timeout := timeoutErr(); timeout != nil {
			return nil, nodeUsageReport{}, timeout
		}
		return nil, nodeUsageReport{}, executionError(ErrorCodeExecutionUnrecoverable, nodeCtx.Err())
	}
	result, err := graphResult.result, graphResult.err
	usage, usageErr := frozenNodeUsage(result)
	if usageErr != nil {
		return nil, nodeUsageReport{}, executionError(
			ErrorCodeExecutionUnrecoverable,
			fmt.Errorf("read frozen graph usage for node %q: %w", node.ID, usageErr),
		)
	}
	if err != nil {
		return nil, usage, executionError(ErrorCodeExecutionUnrecoverable, err)
	}
	if result == nil {
		return nil, usage, executionError(
			ErrorCodeExecutionUnrecoverable,
			errors.New("frozen graph returned no result"),
		)
	}
	if isDurableMemberEntry(entry) {
		pause, pauseErr := loomruntime.ReadMemberBudgetPause(result, entry.Graph.Name)
		if pauseErr != nil {
			return nil, usage, executionError(ErrorCodeExecutionUnrecoverable, pauseErr)
		}
		if pause != nil {
			usage.MemberPause = pause
			return nil, usage, nil
		}
	}
	if result.Yielded || result.StopReason == loom.StopYielded {
		return nil, usage, executionError(
			ErrorCodeUnexpectedInteractiveYield,
			fmt.Errorf("frozen graph for node %q yielded", node.ID),
		)
	}
	if result.StopReason != loom.StopCompleted {
		return nil, usage, executionError(
			ErrorCodeExecutionUnrecoverable,
			fmt.Errorf("frozen graph stopped with %q", result.StopReason),
		)
	}
	output, present := result.State["output"]
	if !present {
		return nil, usage, executionError(
			ErrorCodeOutputInvalid,
			fmt.Errorf("frozen graph for node %q returned no output", node.ID),
		)
	}
	normalizedOutput, err := normalizeAgentNodeOutput(node, output)
	if err != nil {
		return nil, usage, err
	}
	return normalizedOutput, usage, nil
}

func runtimeCLIUsageReport(result workflow.RuntimeCLIResult) (nodeUsageReport, error) {
	accumulator := loomruntime.NewUsageAccumulator()
	callID, err := accumulator.NextCall("cli-node-usage", "engine")
	if err != nil {
		return nodeUsageReport{}, err
	}
	for index, attempt := range result.Attempts {
		attemptID := fmt.Sprintf("cli-attempt-%d", index)
		if err := accumulator.StartAttempt(callID, attemptID); err != nil {
			return nodeUsageReport{}, err
		}
		if err := accumulator.ConfirmAttemptWithMetadata(callID, attemptID, contract.Usage{
			InputTokens: attempt.InputTokens, OutputTokens: attempt.OutputTokens, CostUSD: attempt.CostUSD,
		}, attempt.ToolCalls, loomruntime.UsageAttemptMetadata{
			HasTokens: attempt.HasTokens, HasCost: attempt.HasCost, Source: attempt.Source,
		}); err != nil {
			return nodeUsageReport{}, err
		}
	}
	return nodeUsageReport{
		Totals: accumulator.Totals(), Coverage: accumulator.Coverage(),
		CLIAttempts:   append([]workflow.RuntimeCLIUsageAttempt(nil), result.Attempts...),
		Events:        result.ObservedEvents(200),
		Artifacts:     append([]workflow.RuntimeCLIArtifact(nil), result.Artifacts...),
		DeliveryError: result.DeliveryError,
	}, nil
}

// frozenNodeUsage extracts the confirmed logical usage a frozen graph run
// accumulated into its final state. A nil or incomplete result yields zero
// usage (nothing was observed), never an error.
func frozenNodeUsage(result *loom.RunResult) (nodeUsageReport, error) {
	if result == nil || result.State == nil {
		return nodeUsageReport{Coverage: loomruntime.UsageCoverage{HasTokens: true, HasCost: true}}, nil
	}
	accumulator, err := loomruntime.LoadUsageAccumulator(result.State)
	if err != nil {
		return nodeUsageReport{}, err
	}
	files, err := fileartifact.MemberFiles(result.State)
	if err != nil {
		return nodeUsageReport{}, err
	}
	artifacts := make([]workflow.RuntimeCLIArtifact, 0, len(files))
	for _, file := range files {
		artifacts = append(artifacts, workflow.RuntimeCLIArtifact{Path: file.Path, ContentType: file.ContentType, Content: file.Content})
	}
	incomplete, _ := result.State["__member_usage_incomplete"].(bool)
	return nodeUsageReport{MemberRunID: result.RunID, Totals: accumulator.Totals(), Coverage: accumulator.Coverage(),
		MemberReceipts: accumulator.ConfirmedReceipts(), MemberUsageIncomplete: incomplete, Artifacts: artifacts}, nil
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

func nodeInputTaskIDs(node machine.Node, sources map[string][]string) []string {
	seen := map[string]bool{}
	var ids []string
	for _, binding := range node.Inputs {
		if binding.Value.Source != machine.ValueNodeOutput {
			continue
		}
		for _, id := range sources[binding.Value.NodeID] {
			if !seen[id] {
				ids = append(ids, id)
				seen[id] = true
			}
		}
	}
	sort.Strings(ids)
	return ids
}
