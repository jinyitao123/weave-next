package loomruntime

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
)

const ReplayConfigurationUnavailable = "configuration_unavailable"
const ReplayEvidenceUnavailable = "evidence_unavailable"

var errReplaySegmentBoundary = errors.New("replay reached the recorded segment boundary")

type MemberReplayCheckpoint struct {
	Graph   string
	Seq     int64
	Segment string
	Step    string
	State   json.RawMessage
}

// A replay configuration is built from the immutable publication. The compile
// closure is private so callers cannot replace missing configuration with the
// first recorded request and report a full replay as successful.
type MemberReplayConfiguration struct {
	Digest  string
	compile func(context.Context, *replayJournal, []contract.ToolDef, loom.StepHook) (*loom.Graph, error)
}

func FrozenMemberReplayConfiguration(bundle frozen.FrozenExecutionBundle) (MemberReplayConfiguration, error) {
	if err := frozen.ValidateFrozenExecutionBundle(bundle); err != nil {
		return MemberReplayConfiguration{}, err
	}
	if bundle.Agent.Engine != "loom" {
		return MemberReplayConfiguration{}, errors.New("member is not a journaled Loom execution")
	}
	raw, err := json.Marshal(bundle)
	if err != nil {
		return MemberReplayConfiguration{}, err
	}
	digest, err := frozen.HashCanonicalJSON(raw)
	if err != nil {
		return MemberReplayConfiguration{}, err
	}
	return MemberReplayConfiguration{Digest: digest, compile: func(ctx context.Context, journal *replayJournal, defs []contract.ToolDef, boundary loom.StepHook) (*loom.Graph, error) {
		return compiler.CompileFrozen(ctx, bundle, frozenReplayResolver{bundle}, compiler.FrozenBuildOpts{
			LLM: replayLiveLLM{}, Tools: stdlib.NewJournaledToolDispatcher(replayTools{defs: defs}, journal, stdlib.JournaledToolOpts{SerializeWhenActive: true}),
			ExecutionLLMWrapper: func(inner contract.LLM) contract.LLM { return stdlib.NewJournaledLLM(inner, journal) },
			Hooks:               compiler.FrozenHookPoints{BeforeStepHooks: []loom.StepHook{boundary}},
		})
	}}, nil
}

type frozenReplayResolver struct{ bundle frozen.FrozenExecutionBundle }

func (r frozenReplayResolver) Agent(_ context.Context, id string, version int64) (frozen.FrozenAgentRecord, error) {
	if r.bundle.Agent.AgentID == id && r.bundle.Agent.AgentVersion == version {
		return r.bundle.Agent, nil
	}
	return frozen.FrozenAgentRecord{}, errors.New("unavailable frozen subagent")
}
func (r frozenReplayResolver) Skill(_ context.Context, ref frozen.FrozenDependencyRef) (frozen.FrozenSkill, error) {
	for _, v := range r.bundle.Skills {
		if v.ContentHash == ref.ContentHash {
			return v, nil
		}
	}
	return frozen.FrozenSkill{}, errors.New("unavailable frozen skill")
}
func (r frozenReplayResolver) MCPBinding(_ context.Context, ref frozen.FrozenDependencyRef) (frozen.FrozenMCPBinding, error) {
	for _, v := range r.bundle.MCPBindings {
		if v.ContentHash == ref.ContentHash {
			return v, nil
		}
	}
	return frozen.FrozenMCPBinding{}, errors.New("unavailable frozen tool binding")
}
func (r frozenReplayResolver) ModelBinding(_ context.Context, ref frozen.FrozenDependencyRef) (frozen.FrozenModelBinding, error) {
	for _, v := range append([]frozen.FrozenModelBinding{r.bundle.PrimaryModel}, r.bundle.FallbackModels...) {
		if v.ContentHash == ref.ContentHash {
			return v, nil
		}
	}
	return frozen.FrozenModelBinding{}, errors.New("unavailable frozen model binding")
}
func (r frozenReplayResolver) RuntimeBinding(_ context.Context, ref frozen.FrozenDependencyRef) (frozen.FrozenRuntimeBinding, error) {
	if r.bundle.Runtime != nil && r.bundle.Runtime.ContentHash == ref.ContentHash {
		return *r.bundle.Runtime, nil
	}
	return frozen.FrozenRuntimeBinding{}, errors.New("unavailable frozen runtime binding")
}
func (r frozenReplayResolver) DeliveryTarget(context.Context, frozen.FrozenDependencyRef) (frozen.FrozenDeliveryTarget, error) {
	return frozen.FrozenDeliveryTarget{}, errors.New("delivery is not executed by member replay")
}

func ReadMemberReplayCheckpoints(ctx context.Context, db memberJournalQuerier, workspace, runID string) ([]MemberReplayCheckpoint, error) {
	rows, err := db.Query(ctx, `SELECT value FROM loom_store WHERE namespace IN (SELECT 'checkpoint:'||graph_name FROM weave_run_attempt_leases WHERE workspace_id=$1 AND run_id=$2) AND starts_with(key,$2||'/') ORDER BY key`, workspace, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MemberReplayCheckpoint{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var cp memberCheckpoint
		if err := json.Unmarshal(raw, &cp); err != nil {
			return nil, err
		}
		if cp.RunID != runID {
			return nil, ErrMemberIdentityConflict
		}
		segment, _ := cp.State["__member_step_segment"].(string)
		state, err := json.Marshal(cp.State)
		if err != nil {
			return nil, err
		}
		out = append(out, MemberReplayCheckpoint{Graph: cp.Graph, Seq: cp.Seq, Segment: segment, Step: cp.LastStep, State: state})
	}
	return out, rows.Err()
}

// ReplayMemberJournal covers every recorded segment. Entry checkpoints retain
// material, prompt and private controlled-loop state; the frozen compiler owns
// loop limits, guards, compaction, schemas and policy. No live implementation or
// database writer is installed, even when the original graph used them.
func ReplayMemberJournal(ctx context.Context, runID string, entries []MemberJournalEntry, checkpoints []MemberReplayCheckpoint, configuration MemberReplayConfiguration) *MemberReplayReport {
	report := &MemberReplayReport{MemberRunID: runID, Outcome: ReplayConfigurationUnavailable, ConfigurationSHA256: configuration.Digest, ReplayScope: "model_tool_journal", NoLiveCalls: true}
	if configuration.compile == nil || configuration.Digest == "" {
		report.Detail = "the immutable execution configuration is missing"
		return report
	}
	if len(entries) == 0 {
		report.Outcome = ReplayEvidenceUnavailable
		report.Detail = "the member has no recorded operation journal"
		return report
	}
	grouped := map[string][]MemberJournalEntry{}
	for _, entry := range entries {
		if _, seen := grouped[entry.Segment]; !seen {
			report.Segments = append(report.Segments, entry.Segment)
		}
		if entry.Cursor != int64(len(grouped[entry.Segment])+1) {
			report.Outcome = ReplayDiverged
			report.Detail = "journal cursors are not contiguous"
			return report
		}
		hash, err := frozen.HashCanonicalJSON(entry.Input)
		if err != nil || hash != entry.InputHash {
			report.Outcome = ReplayDiverged
			report.Detail = "recorded request hash does not match its input"
			return report
		}
		grouped[entry.Segment] = append(grouped[entry.Segment], entry)
	}
	sort.Strings(report.Segments)
	for _, segment := range report.Segments {
		report.Segment = segment
		report.Outcome = ReplayConfigurationUnavailable
		items := grouped[segment]
		report.Operations += len(items)
		parts := strings.SplitN(segment, "/", 2)
		if len(parts) != 2 {
			report.Detail = "journal segment identity is invalid"
			return report
		}
		start, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil || start < 1 {
			report.Detail = "journal segment checkpoint sequence is invalid"
			return report
		}
		var entry *MemberReplayCheckpoint
		for index := range checkpoints {
			candidate := &checkpoints[index]
			if candidate.Seq == start && candidate.Segment == segment {
				if entry != nil {
					report.Detail = "ambiguous journal entry checkpoint"
					return report
				}
				entry = candidate
			}
		}
		if entry == nil || entry.Step != parts[1] {
			report.Detail = "the exact segment entry checkpoint is missing"
			return report
		}
		state, err := decodeReplayState(entry.State)
		if err != nil {
			report.Detail = "segment checkpoint cannot be decoded"
			return report
		}
		var request contract.ChatRequest
		found := false
		for _, item := range items {
			if item.Kind == string(stdlib.OperationModel) {
				if json.Unmarshal(item.Input, &request) != nil {
					report.Detail = "recorded model request is unreadable"
					return report
				}
				found = true
				break
			}
		}
		if !found {
			report.Detail = "segment contains no model request or tool definition snapshot"
			return report
		}
		journal := &replayJournal{entries: items, report: report}
		boundary := func(_ context.Context, step string, _ loom.State) error {
			if step != entry.Step {
				return errReplaySegmentBoundary
			}
			return nil
		}
		graph, err := configuration.compile(ctx, journal, request.Tools, boundary)
		if err != nil {
			report.Detail = "the frozen graph configuration cannot be compiled offline"
			return report
		}
		if graph.Name != entry.Graph {
			report.Detail = "compiled graph differs from the checkpoint graph"
			return report
		}
		store := loom.NewMemStore()
		cp := memberCheckpoint{Schema: loom.CurrentCheckpointSchema, RunID: runID, Graph: entry.Graph, Seq: entry.Seq, LastStep: entry.Step, State: state, YieldPhase: "mid_step"}
		raw, _ := json.Marshal(cp)
		if err := store.Put(ctx, "checkpoint:"+entry.Graph, runID, raw); err != nil {
			report.Detail = "offline checkpoint setup failed"
			return report
		}
		replayCtx := context.WithValue(ctx, replayActiveKey{}, true)
		result, stepErr := graph.Resume(replayCtx, runID, nil, store)
		report.Replayed += journal.cursor
		report.NetworkAttempts += journal.liveAttempts
		switch {
		case errors.Is(stepErr, errReplayInvalidHistory):
			report.Outcome = ReplayInvalidHistory
			report.Detail = "recorded tool result identity is invalid"
		case errors.Is(stepErr, errReplayDiverged):
			report.Outcome = ReplayDiverged
			report.Detail = stepErr.Error()
		case errors.Is(stepErr, errReplayModelLost):
			report.Outcome = ReplayModelLost
			report.Detail = "recorded model intent has no response"
		case errors.Is(stepErr, stdlib.ErrJournalOutcomeUnknown):
			report.Outcome = ReplayOutcomeUnknown
			report.Detail = "recorded tool intent has no confirmed response"
		case errors.Is(stepErr, errReplayJournalEnded):
			report.Outcome = ReplayJournalEnded
			report.Detail = "the journal ended before the member completed"
		case errors.Is(stepErr, errReplayLive):
			report.Outcome = ReplayLoopError
			report.Detail = "offline replay rejected an unrecorded live operation"
		case stepErr != nil && !errors.Is(stepErr, errReplaySegmentBoundary):
			report.Outcome = ReplayLoopError
			report.Detail = "the frozen graph stopped with an execution error"
		case journal.cursor != len(items):
			report.Outcome = ReplayDiverged
			report.Detail = "compiled execution left recorded operations unused"
		case len(report.HistoryProblems) > 0:
			report.Outcome = ReplayInvalidHistory
			report.Detail = "recorded provider history is invalid"
		default:
			report.VerifiedSegments++
			if result != nil {
				if output, ok := result.State["output"].(string); ok {
					report.Output = output
				}
			}
			report.Outcome = ReplayCompleted
			if result != nil && result.StopReason != loom.StopCompleted && segment == report.Segments[len(report.Segments)-1] {
				report.Outcome = ReplayJournalEnded
				report.Detail = "the final recorded segment ends before the member completes"
			}
		}
		if report.Outcome != ReplayCompleted {
			return report
		}
	}
	report.ConfigVerified = true
	report.NoLiveCalls = report.NetworkAttempts == 0
	return report
}
