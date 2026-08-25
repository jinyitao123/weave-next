package loomruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/stdlib"
)

// NestedStepOptions configures how the parent will merge a child step update.
type NestedStepOptions struct {
	ParentMergeConfig *loom.MergeConfig
}

// NestedStep returns a host-managed child step that retains Loom's existing
// continuation protocol while isolating run-private state at the boundary.
func (p PreparedRun) NestedStep(opts NestedStepOptions) (loom.Step, error) {
	if p.graph == nil {
		return nil, fmt.Errorf("nested child graph is required")
	}
	if p.tenant == "" {
		return nil, fmt.Errorf("nested child tenant is required")
	}
	if p.agent == "" {
		return nil, fmt.Errorf("nested child agent is required")
	}
	if p.stamp == nil || p.stamp.AgentID == "" || p.stamp.AgentVersion < 1 ||
		!p.stamp.ExecutionScope.Valid() {
		return nil, fmt.Errorf("nested child execution stamp is incomplete")
	}

	subGraphOpts := stdlib.SubGraphOpts{
		YieldPolicy:       stdlib.YieldBubble,
		ParentMergeConfig: opts.ParentMergeConfig,
	}
	if p.terminalSink != nil || p.lifecycleHook != nil {
		hooks := &loom.LifecycleHooks{
			CheckpointObserver: preparedCheckpointObserver{prepared: p},
		}
		if p.terminalSink != nil {
			hooks.Terminalizer = nestedRunTerminalizer{prepared: p}
			subGraphOpts.ChildObserver = nestedTerminalFailClosedObserver
		}
		subGraphOpts.LifecycleHooks = hooks
	}
	inner := stdlib.NewSubGraphStep(p.graph, p.store, subGraphOpts)
	return func(ctx context.Context, parent loom.State) (loom.State, error) {
		input, mode, err := p.nestedInput(ctx, parent)
		if err != nil {
			return nil, err
		}
		switch mode {
		case nestedLegacyResume:
			ctx = context.WithValue(ctx, usageRunScopeContextKey{}, (*usageRunScope)(nil))
		default:
			ctx = withUsageRunScope(ctx)
		}
		var attempt *nestedTerminalAttempt
		if mode != nestedLegacyResume {
			coordinator, err := p.normalTerminalCoordinator()
			if err != nil {
				return nil, err
			}
			attempt, err = p.prepareNestedTerminalAttempt(ctx, parent, input, mode)
			if err != nil {
				return nil, err
			}
			attempt.coordinator = coordinator
			if attempt.lease != nil {
				attempt.heartbeatScope, err = startAttemptHeartbeat(
					ctx,
					attempt.heartbeat,
					*attempt.lease,
					attempt.admissionAnchor,
					p.heartbeatConfig(),
				)
				if err != nil {
					return nil, err
				}
				ctx = attempt.heartbeatScope.Context()
			}
			if p.terminalSink != nil {
				ctx = context.WithValue(ctx, nestedTerminalAttemptContextKey{}, attempt)
			}
		}
		update, innerErr := inner(ctx, input)
		if attempt != nil {
			innerErr = joinNestedHeartbeatError(innerErr, attempt.stopHeartbeat())
			if update == nil && attempt.heartbeatLossUpdate != nil {
				update = attempt.heartbeatLossUpdate
			}
		}
		projected, projectErr := projectNestedUpdate(update)
		if projectErr != nil {
			return nil, errors.Join(projectErr, innerErr)
		}
		return projected, innerErr
	}, nil
}

type nestedTerminalAttemptContextKey struct{}

type nestedTerminalAttempt struct {
	parentAttribution   TerminalAttribution
	childAttribution    TerminalAttribution
	runID               string
	startedAt           time.Time
	mode                nestedMode
	lease               *RunAttemptLease
	heartbeat           AttemptLeaseHeartbeat
	admissionAnchor     time.Time
	heartbeatScope      *attemptHeartbeatScope
	terminalCtx         context.Context
	heartbeatLossUpdate loom.State
	coordinator         NormalTerminalCoordinator
}

func (attempt *nestedTerminalAttempt) stopHeartbeat() error {
	if attempt == nil || attempt.heartbeatScope == nil {
		return nil
	}
	return attempt.heartbeatScope.Stop()
}

func joinNestedHeartbeatError(runErr, heartbeatErr error) error {
	if heartbeatErr == nil {
		return runErr
	}
	if errors.Is(runErr, ErrAttemptLeaseHeartbeatOwnerLost) ||
		errors.Is(runErr, ErrAttemptLeaseHeartbeatTTLExceeded) {
		return runErr
	}
	if runErr == nil {
		return heartbeatErr
	}
	return errors.Join(runErr, heartbeatErr)
}

func (p PreparedRun) prepareNestedTerminalAttempt(
	ctx context.Context,
	parent loom.State,
	input loom.State,
	mode nestedMode,
) (*nestedTerminalAttempt, error) {
	raw, exists := parent[terminalAttributionStateKey]
	if !exists {
		return nil, fmt.Errorf("nested parent terminal attribution is required")
	}
	attribution, err := decodeTerminalAttributionCheckpointStamp(raw)
	if err != nil {
		return nil, fmt.Errorf("decode nested parent terminal attribution: %w", err)
	}
	if attribution.workspaceID != p.tenant {
		return nil, fmt.Errorf("nested parent terminal attribution workspace does not match child tenant")
	}
	attempt := &nestedTerminalAttempt{
		parentAttribution: attribution,
		startedAt:         time.Now(),
		mode:              mode,
		terminalCtx:       ctx,
	}
	if mode == nestedResume {
		_, childRunID, err := nestedContinuationIdentity(parent)
		if err != nil {
			return nil, err
		}
		attempt.runID = childRunID
		lifecycle, hasLifecycle := p.expectedRuns.(AttemptLeaseLifecycle)
		if p.a4FreshRequired &&
			(!hasLifecycle || !lifecycle.A4AttemptLeaseLifecycleGuaranteed()) {
			return nil, &ExpectedRunPersistenceError{
				Err: ErrA4AttemptLeaseLifecycleUnsupported,
			}
		}
		if !hasLifecycle || !lifecycle.A4AttemptLeaseLifecycleGuaranteed() {
			return attempt, nil
		}
		childAttribution, expected, err := p.verifyNestedResumeIdentity(
			ctx,
			parent,
			attribution,
			childRunID,
		)
		if err != nil {
			return nil, err
		}
		admitter, ok := p.expectedRuns.(nestedResumeAttemptAdmitter)
		if !ok {
			return nil, &ExpectedRunPersistenceError{
				Err: ErrA4AttemptLeaseLifecycleUnsupported,
			}
		}
		heartbeat, err := p.resumeAttemptHeartbeat()
		if err != nil {
			return nil, err
		}
		admissionAnchor := p.heartbeatConfig().Clock.Now()
		lease, err := admitter.admitResumeExpected(ctx, ResumeAttemptAdmission{
			WorkspaceID: p.tenant,
			RunID:       childRunID,
			GraphName:   p.graph.Name,
			AttemptID:   uuid.New(),
			LeaseTTL:    120 * time.Second,
		}, expected)
		if err != nil {
			return nil, &ExpectedRunPersistenceError{Err: err}
		}
		if err := p.runLifecycleHook(
			ctx,
			RunLifecycleStageAttemptAdmitted,
		); err != nil {
			return nil, err
		}
		startedAt, err := time.Parse(time.RFC3339Nano, lease.RunStartedAt)
		if err != nil {
			return nil, &ExpectedRunPersistenceError{
				Err: fmt.Errorf("parse committed child lease run_started_at: %w", err),
			}
		}
		attempt.childAttribution = childAttribution
		attempt.startedAt = startedAt
		attempt.lease = &lease
		attempt.heartbeat = heartbeat
		attempt.admissionAnchor = admissionAnchor
		return attempt, nil
	}
	childRunID, ok := input["__run_id"].(string)
	if !ok || childRunID == "" {
		return nil, fmt.Errorf("fresh nested child runtime-owned __run_id is required")
	}
	runStartedAt, ok := input["__run_started_at"].(string)
	if !ok || runStartedAt == "" {
		return nil, &ExpectedRunPersistenceError{
			Err: fmt.Errorf("fresh nested child __run_started_at is required"),
		}
	}
	if err := canonicalRunStartedAt(runStartedAt); err != nil {
		return nil, &ExpectedRunPersistenceError{
			Err: fmt.Errorf("fresh nested child __run_started_at: %w", err),
		}
	}
	parsedStartedAt, err := time.Parse(time.RFC3339Nano, runStartedAt)
	if err != nil {
		return nil, &ExpectedRunPersistenceError{Err: err}
	}
	attempt.startedAt = parsedStartedAt
	childAttribution, err := nestedChildTerminalAttribution(attribution, input)
	if err != nil {
		return nil, err
	}
	if err := bindTerminalAttribution(input, childAttribution); err != nil {
		return nil, err
	}
	if p.expectedRuns == nil {
		return nil, &ExpectedRunPersistenceError{Err: fmt.Errorf("expected run registry is required for fresh nested child")}
	}
	record := expectedRunRecordFromAttribution(
		childRunID,
		p.agent,
		childAttribution,
		time.Now().UTC(),
	)
	admitter, hasAdmitter := p.expectedRuns.(FreshExpectedRunAdmitter)
	lifecycle, hasLifecycle := p.expectedRuns.(AttemptLeaseLifecycle)
	if p.a4FreshRequired &&
		(!hasAdmitter || !admitter.A4AdmissionGuaranteed()) {
		return nil, &ExpectedRunPersistenceError{Err: ErrA4FreshAdmissionUnsupported}
	}
	if p.a4FreshRequired &&
		(!hasLifecycle || !lifecycle.A4AttemptLeaseLifecycleGuaranteed()) {
		return nil, &ExpectedRunPersistenceError{
			Err: ErrA4AttemptLeaseLifecycleUnsupported,
		}
	}
	if hasAdmitter && admitter.A4AdmissionGuaranteed() {
		if !hasLifecycle || !lifecycle.A4AttemptLeaseLifecycleGuaranteed() {
			return nil, &ExpectedRunPersistenceError{
				Err: ErrA4AttemptLeaseLifecycleUnsupported,
			}
		}
		heartbeat, err := p.freshAttemptHeartbeat()
		if err != nil {
			return nil, err
		}
		admissionAnchor := p.heartbeatConfig().Clock.Now()
		lease, err := admitter.AdmitFresh(ctx, FreshExpectedRunAdmission{
			Record:       record,
			GraphName:    p.graph.Name,
			RunStartedAt: runStartedAt,
			AttemptID:    uuid.New(),
			LeaseTTL:     120 * time.Second,
		})
		if err != nil {
			return nil, &ExpectedRunPersistenceError{Err: err}
		}
		if err := p.runLifecycleHook(
			ctx,
			RunLifecycleStageAttemptAdmitted,
		); err != nil {
			return nil, err
		}
		attempt.lease = &lease
		attempt.heartbeat = heartbeat
		attempt.admissionAnchor = admissionAnchor
		attempt.startedAt, err = time.Parse(time.RFC3339Nano, lease.RunStartedAt)
		if err != nil {
			return nil, &ExpectedRunPersistenceError{
				Err: fmt.Errorf("parse committed child lease run_started_at: %w", err),
			}
		}
	} else if err := p.expectedRuns.Register(ctx, record); err != nil {
		return nil, &ExpectedRunPersistenceError{Err: err}
	}
	attempt.runID = childRunID
	attempt.childAttribution = childAttribution
	return attempt, nil
}

func (p PreparedRun) observeNestedTerminal(
	ctx context.Context,
	runResult *loom.RunResult,
	runErr error,
	resumed bool,
) error {
	attempt, ok := ctx.Value(nestedTerminalAttemptContextKey{}).(*nestedTerminalAttempt)
	if !ok || attempt == nil {
		return nil
	}
	if (attempt.mode == nestedResume) != resumed {
		return fmt.Errorf("nested child observer resume mode does not match prepared attempt")
	}
	if heartbeatErr := attempt.stopHeartbeat(); heartbeatErr != nil {
		attempt.heartbeatLossUpdate = cloneState(runResult.State)
		return heartbeatErr
	}
	ctx = attempt.terminalCtx
	result := FromRunResult(runResult)
	if result.RunID == "" || result.RunID != attempt.runID {
		return fmt.Errorf("nested child observer run identity does not match registered attempt")
	}
	if err := p.validateNestedResultStamp(result.State); err != nil {
		return err
	}
	attribution, err := nestedChildTerminalAttribution(
		attempt.parentAttribution,
		result.State,
	)
	if err != nil {
		return err
	}
	if (attempt.mode == nestedFresh || attempt.lease != nil) &&
		!reflect.DeepEqual(attribution.inputCopy(), attempt.childAttribution.inputCopy()) {
		return fmt.Errorf("nested child observer attribution does not match registered attempt")
	}
	terminalStartedAt := attempt.startedAt
	if attempt.mode == nestedResume && attempt.lease == nil {
		terminalStartedAt = persistedRunStartedAt(result.State, attempt.startedAt)
	}
	candidate, err := AssembleTerminalV3(TerminalAssemblyInput{
		Tenant:      p.tenant,
		Agent:       p.agent,
		StartedAt:   terminalStartedAt,
		EndedAt:     time.Now(),
		Result:      result,
		RunErr:      runErr,
		Attribution: attribution,
	})
	if err != nil {
		return fmt.Errorf("assemble nested child terminal: %w", err)
	}
	coordinated, err := persistNormalTerminalCandidate(
		ctx,
		p.terminalSink,
		attempt.coordinator,
		attempt.lease,
		candidate,
	)
	if err != nil {
		return fmt.Errorf("write nested child terminal: %w", err)
	}
	if !coordinated {
		if err := p.handoffYieldedAttempt(
			ctx,
			attempt.lease,
			result,
			nil,
		); err != nil {
			return err
		}
	}
	return nil
}

func (p PreparedRun) verifyNestedResumeIdentity(
	ctx context.Context,
	parent loom.State,
	parentAttribution TerminalAttribution,
	childRunID string,
) (TerminalAttribution, ExpectedRunRecordV1, error) {
	checkpoint, err := p.loadNestedCheckpoint(ctx, childRunID)
	if err != nil {
		return TerminalAttribution{}, ExpectedRunRecordV1{}, err
	}
	if err := p.validateNestedContinuation(parent, checkpoint); err != nil {
		return TerminalAttribution{}, ExpectedRunRecordV1{}, err
	}
	checkpointAttribution, err := p.nestedCheckpointAttribution(childRunID, checkpoint.State)
	if err != nil {
		return TerminalAttribution{}, ExpectedRunRecordV1{}, err
	}
	currentAttribution, err := nestedCurrentChildTerminalAttribution(
		parentAttribution,
		parent,
		checkpointAttribution,
	)
	if err != nil {
		return TerminalAttribution{}, ExpectedRunRecordV1{}, err
	}
	if p.expectedRuns == nil {
		return TerminalAttribution{}, ExpectedRunRecordV1{}, &ExpectedRunPersistenceError{
			Err: fmt.Errorf("expected run registry is required for nested child resume"),
		}
	}
	checkpointRecord := expectedRunRecordFromAttribution(
		childRunID,
		p.agent,
		checkpointAttribution,
		time.Time{},
	)
	currentRecord := expectedRunRecordFromAttribution(
		childRunID,
		p.agent,
		currentAttribution,
		time.Time{},
	)
	if err := compareExpectedRunIdentity(checkpointRecord, currentRecord); err != nil {
		return TerminalAttribution{}, ExpectedRunRecordV1{}, nestedResumeIdentityConflict(err)
	}
	return currentAttribution, currentRecord, nil
}

func nestedResumeIdentityConflict(err error) error {
	return &ExpectedRunPersistenceError{
		Err: fmt.Errorf("%w: nested child identity mismatch: %v", ErrResumeAttemptConflict, err),
	}
}

func nestedCurrentChildTerminalAttribution(
	parentAttribution TerminalAttribution,
	parent loom.State,
	checkpointAttribution TerminalAttribution,
) (TerminalAttribution, error) {
	parentRunID, ok := parent["__run_id"].(string)
	if !ok || parentRunID == "" {
		return TerminalAttribution{}, fmt.Errorf("nested parent __run_id must be a non-empty string")
	}
	checkpointInput := checkpointAttribution.inputCopy()
	if checkpointInput.ParentSeq == nil || *checkpointInput.ParentSeq < 0 {
		return TerminalAttribution{}, fmt.Errorf(
			"nested child checkpoint attribution has invalid parent_seq",
		)
	}
	lineage := loom.State{
		"__parent_run": parentRunID,
		"__parent_seq": *checkpointInput.ParentSeq,
	}
	return nestedChildTerminalAttribution(parentAttribution, lineage)
}

func (p PreparedRun) validateNestedResultStamp(state loom.State) error {
	if state == nil {
		return fmt.Errorf("nested child terminal State is required")
	}
	agentID, idOK := state["__agent_id"].(string)
	version, versionOK := checkpointInt64(state["__agent_version"])
	scope, scopeOK := state["__execution_scope"].(string)
	if !idOK || agentID == "" || !versionOK || version < 1 || !scopeOK || scope == "" {
		return fmt.Errorf("nested child terminal execution stamp is incomplete")
	}
	if agentID != p.stamp.AgentID || version != int64(p.stamp.AgentVersion) ||
		scope != string(p.stamp.ExecutionScope) {
		return fmt.Errorf("nested child terminal execution stamp does not match prepared child")
	}
	return nil
}

func nestedChildTerminalAttribution(
	parent TerminalAttribution,
	state loom.State,
) (TerminalAttribution, error) {
	parentRunID, parentSeq, err := terminalCanonicalParentFromState(state)
	if err != nil {
		return TerminalAttribution{}, fmt.Errorf("derive nested child terminal lineage: %w", err)
	}
	if !parentRunID.present || !parentSeq.present {
		return TerminalAttribution{}, fmt.Errorf("derive nested child terminal lineage: canonical parent pair is required")
	}
	input := parent.inputCopy()
	input.ParentRunID = parentRunID.pointer()
	input.ParentSeq = parentSeq.pointer()
	input.AggregationParentRunID = parentRunID.pointer()

	var snapshot *TerminalSnapshotEvidence
	if parent.snapshot != nil {
		snapshot = &TerminalSnapshotEvidence{
			RunID:           parent.snapshot.runID,
			WorkspaceID:     parent.snapshot.workspaceID,
			TeamID:          parent.snapshot.teamID,
			Mode:            parent.snapshot.mode,
			WorkflowID:      parent.snapshot.workflowID.pointer(),
			WorkflowVersion: parent.snapshot.workflowVersion.pointer(),
			ParentRunID:     parent.snapshot.parentRunID.pointer(),
			TaskGroupID:     parent.snapshot.taskGroupID.pointer(),
		}
	}
	attribution, err := NewTerminalAttribution(input, snapshot)
	if err != nil {
		return TerminalAttribution{}, fmt.Errorf("derive nested child terminal attribution: %w", err)
	}
	return attribution, nil
}

type nestedMode uint8

const (
	nestedFresh nestedMode = iota
	nestedResume
	nestedLegacyResume
)

func (p PreparedRun) nestedInput(ctx context.Context, parent loom.State) (loom.State, nestedMode, error) {
	if parent == nil {
		return nil, nestedFresh, fmt.Errorf("nested parent state is required")
	}
	runID, ok := parent["__run_id"].(string)
	if !ok || runID == "" {
		return nil, nestedFresh, fmt.Errorf("nested parent __run_id must be a non-empty string")
	}
	parentSeq, err := nestedParentSeq(parent["__seq"])
	if err != nil {
		return nil, nestedFresh, err
	}
	for key := range parent {
		if strings.HasPrefix(key, "__") && !knownNestedPrivateKey(key) {
			return nil, nestedFresh, fmt.Errorf("nested parent state contains unknown private key %q", key)
		}
	}
	hasContinuation, childRunID, err := nestedContinuationIdentity(parent)
	if err != nil {
		return nil, nestedFresh, err
	}
	if hasContinuation {
		input := cloneNestedState(parent)
		if childRunID == runID {
			return input, nestedLegacyResume, nil
		}
		if err := p.validateNestedCheckpointStamp(ctx, childRunID); err != nil {
			return nil, nestedResume, err
		}
		return input, nestedResume, nil
	}

	input := cloneNestedState(parent)
	for key := range input {
		if strings.HasPrefix(key, "__") {
			if key != "__profile" {
				delete(input, key)
			}
		}
	}
	delete(input, "usage")
	delete(input, "output")
	delete(input, "yield_type")
	input["agent_name"] = p.agent
	input["__parent_run"] = runID
	input["__parent_seq"] = parentSeq + 1
	input["__run_id"] = uuid.NewString()
	input["__run_started_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	input["__agent_id"] = p.stamp.AgentID
	input["__agent_version"] = p.stamp.AgentVersion
	input["__execution_scope"] = string(p.stamp.ExecutionScope)
	if err := StoreUsageAccumulator(input, NewUsageAccumulator()); err != nil {
		return nil, nestedFresh, fmt.Errorf("initialize nested child usage accumulator: %w", err)
	}
	return input, nestedFresh, nil
}

func nestedParentSeq(raw any) (int64, error) {
	if raw == nil {
		return 0, nil
	}
	var seq int64
	switch value := raw.(type) {
	case int:
		seq = int64(value)
	case int64:
		seq = value
	case float64:
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 ||
			value >= float64(uint64(1)<<63) || math.Trunc(value) != value {
			return 0, fmt.Errorf("nested parent __seq must be a non-negative integer")
		}
		seq = int64(value)
	case json.Number:
		parsed, err := value.Int64()
		if err != nil {
			return 0, fmt.Errorf("nested parent __seq must be a non-negative integer")
		}
		seq = parsed
	default:
		return 0, fmt.Errorf("nested parent __seq has invalid type %T", raw)
	}
	if seq < 0 || seq == math.MaxInt64 {
		return 0, fmt.Errorf("nested parent __seq is negative or overflows child lineage")
	}
	return seq, nil
}

func nestedContinuationIdentity(parent loom.State) (bool, string, error) {
	keys := []string{
		"__child_run_id", "__child_graph", "__child_graph_revision",
		"__child_yield_token", "__child_checkpoint_seq", "__child_resume_input",
	}
	present := 0
	for _, key := range keys {
		if _, ok := parent[key]; ok {
			present++
		}
	}
	if present == 0 {
		if _, pending := parent["__child_pending"]; pending {
			return false, "", fmt.Errorf("nested child continuation is incomplete")
		}
		return false, "", nil
	}
	if present != len(keys) {
		return false, "", fmt.Errorf("nested child continuation is incomplete")
	}
	runID, runOK := parent["__child_run_id"].(string)
	graph, graphOK := parent["__child_graph"].(string)
	revision, revisionOK := parent["__child_graph_revision"].(string)
	token, tokenOK := parent["__child_yield_token"].(string)
	seq, seqOK := nestedPositiveInt64(parent["__child_checkpoint_seq"])
	if !runOK || !graphOK || !revisionOK || !tokenOK || !seqOK ||
		runID == "" || graph == "" || revision == "" || token == "" || seq < 1 {
		return false, "", fmt.Errorf("nested child continuation is invalid")
	}
	return true, runID, nil
}

func nestedPositiveInt64(raw any) (int64, bool) {
	switch value := raw.(type) {
	case int:
		if value > 0 {
			return int64(value), true
		}
	case int64:
		if value > 0 {
			return value, true
		}
	case float64:
		if value > 0 && value <= float64(1<<53) && math.Trunc(value) == value {
			return int64(value), true
		}
	}
	return 0, false
}

func (p PreparedRun) validateNestedCheckpointStamp(
	ctx context.Context,
	runID string,
) error {
	_, err := p.loadNestedCheckpoint(ctx, runID)
	return err
}

func (p PreparedRun) nestedCheckpointAttribution(
	runID string,
	state loom.State,
) (TerminalAttribution, error) {
	rawAttribution, present := state[terminalAttributionStateKey]
	if !present {
		return TerminalAttribution{}, fmt.Errorf(
			"nested child checkpoint %q terminal attribution is missing",
			runID,
		)
	}
	attribution, err := decodeTerminalAttributionCheckpointStamp(rawAttribution)
	if err != nil {
		return TerminalAttribution{}, fmt.Errorf(
			"nested child checkpoint %q terminal attribution: %w",
			runID,
			err,
		)
	}
	if err := validateTerminalAttributionForResult(
		attribution,
		p.tenant,
		Result{RunID: runID, State: state},
	); err != nil {
		return TerminalAttribution{}, fmt.Errorf(
			"nested child checkpoint %q terminal attribution: %w",
			runID,
			err,
		)
	}
	return attribution, nil
}

type nestedCheckpoint struct {
	RunID      string     `json:"run_id"`
	Graph      string     `json:"graph"`
	Seq        int64      `json:"seq"`
	YieldPhase string     `json:"yield_phase"`
	State      loom.State `json:"state"`
}

func (p PreparedRun) loadNestedCheckpoint(
	ctx context.Context,
	runID string,
) (nestedCheckpoint, error) {
	if p.store == nil {
		return nestedCheckpoint{}, fmt.Errorf("nested child checkpoint store is unavailable")
	}
	data, err := p.store.Get(ctx, "checkpoint:"+p.graph.Name, runID)
	if err != nil {
		return nestedCheckpoint{}, fmt.Errorf("load nested child checkpoint %q: %w", runID, err)
	}
	var checkpoint nestedCheckpoint
	if err := json.Unmarshal(data, &checkpoint); err != nil {
		return nestedCheckpoint{}, fmt.Errorf("decode nested child checkpoint %q: %w", runID, err)
	}
	if checkpoint.RunID != runID || checkpoint.Graph != p.graph.Name || checkpoint.State == nil {
		return nestedCheckpoint{}, fmt.Errorf("nested child checkpoint %q ownership mismatch", runID)
	}
	agentID, idOK := checkpoint.State["__agent_id"].(string)
	version, versionOK := checkpointInt64(checkpoint.State["__agent_version"])
	scope, scopeOK := checkpoint.State["__execution_scope"].(string)
	if !idOK || agentID == "" || !versionOK || version < 1 || !scopeOK || scope == "" {
		return nestedCheckpoint{}, fmt.Errorf("nested child checkpoint %q execution stamp is incomplete", runID)
	}
	if agentID != p.stamp.AgentID || version != int64(p.stamp.AgentVersion) ||
		scope != string(p.stamp.ExecutionScope) {
		return nestedCheckpoint{}, fmt.Errorf(
			"nested child checkpoint %q execution stamp does not match prepared child",
			runID,
		)
	}
	return checkpoint, nil
}

func (p PreparedRun) validateNestedContinuation(
	parent loom.State,
	checkpoint nestedCheckpoint,
) error {
	runID := checkpoint.RunID
	graph, _ := parent["__child_graph"].(string)
	if graph != p.graph.Name {
		return fmt.Errorf(
			"loom: child graph continuation names %q, current graph is %q",
			graph,
			p.graph.Name,
		)
	}
	revision, _ := parent["__child_graph_revision"].(string)
	if revision != nestedChildGraphRevision(p.graph) {
		return fmt.Errorf(
			"loom: child graph %q run %q stale continuation: topology revision does not match current child graph",
			p.graph.Name,
			runID,
		)
	}
	if checkpoint.State["__yield"] != true {
		return fmt.Errorf(
			"loom: child graph %q run %q continuation does not reference a yielded checkpoint",
			p.graph.Name,
			runID,
		)
	}
	seq, _ := nestedPositiveInt64(parent["__child_checkpoint_seq"])
	checkpointSeq, checkpointSeqOK := nestedPositiveInt64(checkpoint.State["__checkpoint_seq"])
	if !checkpointSeqOK || seq != checkpoint.Seq || seq != checkpointSeq {
		return fmt.Errorf(
			"loom: child graph %q run %q stale continuation: checkpoint seq does not match latest child checkpoint",
			p.graph.Name,
			runID,
		)
	}
	token, _ := parent["__child_yield_token"].(string)
	checkpointToken, checkpointTokenOK := checkpoint.State["__yield_token"].(string)
	if !checkpointTokenOK || checkpointToken != token {
		return fmt.Errorf(
			"loom: child graph %q run %q stale continuation: yield token does not match latest child checkpoint",
			p.graph.Name,
			runID,
		)
	}
	if err := validateNestedResumeInput(
		parent["__child_resume_input"],
		checkpoint.State["__toolloop_pending"],
	); err != nil {
		return fmt.Errorf(
			"loom: child graph %q run %q resume input: %w",
			p.graph.Name,
			runID,
			err,
		)
	}
	return nil
}

func nestedChildGraphRevision(child *loom.Graph) string {
	digest := sha256.Sum256(
		[]byte(child.Entry() + "\x00" + strings.Join(child.StepNames(), "\x00")),
	)
	return fmt.Sprintf("%x", digest)
}

type nestedPendingCall struct {
	CallID  string `json:"call_id"`
	Tool    string `json:"tool"`
	Args    string `json:"args"`
	ParkRef string `json:"park_ref"`
}

type nestedResumedToolResult struct {
	Content string `json:"content"`
	IsError bool   `json:"is_error"`
}

func validateNestedResumeInput(raw, pendingRaw any) error {
	data, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	var top map[string]json.RawMessage
	if err := decodeStrictNestedJSON(data, &top); err != nil {
		return err
	}
	resultsRaw, ok := top["__resumed_tool_results"]
	if !ok || len(top) != 1 {
		return fmt.Errorf("only __resumed_tool_results is allowed")
	}
	var rawResults map[string]map[string]json.RawMessage
	if err := decodeStrictNestedJSON(resultsRaw, &rawResults); err != nil {
		return fmt.Errorf("decode __resumed_tool_results: %w", err)
	}
	pendingData, err := json.Marshal(pendingRaw)
	if err != nil {
		return fmt.Errorf("encode __toolloop_pending: %w", err)
	}
	var pending []nestedPendingCall
	if pendingRaw != nil {
		if err := json.Unmarshal(pendingData, &pending); err != nil {
			return fmt.Errorf("decode __toolloop_pending: %w", err)
		}
	}
	allowed := make(map[string]bool, len(pending))
	for _, call := range pending {
		allowed[call.CallID] = true
	}
	for callID, fields := range rawResults {
		if !allowed[callID] {
			return fmt.Errorf("call ID %q is not pending in child checkpoint", callID)
		}
		if len(fields) != 2 || fields["content"] == nil || fields["is_error"] == nil {
			return fmt.Errorf("result %q must contain only content and is_error", callID)
		}
		resultData, _ := json.Marshal(fields)
		var result nestedResumedToolResult
		if err := decodeStrictNestedJSON(resultData, &result); err != nil {
			return fmt.Errorf("decode result %q: %w", callID, err)
		}
	}
	return nil
}

func decodeStrictNestedJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("trailing JSON value")
	}
	return nil
}

func projectNestedUpdate(update loom.State) (loom.State, error) {
	if update == nil {
		return nil, nil
	}
	projected := make(loom.State, len(update))
	yielded := update["__yield"] == true
	if yielded {
		if err := validateNestedYieldEnvelope(update); err != nil {
			return nil, err
		}
	}
	for key, value := range update {
		if key == "__deleted_keys" {
			continue
		}
		if strings.HasPrefix(key, "__") {
			if yielded && allowedNestedYieldKey(key) {
				projected[key] = cloneNestedValue(value)
				continue
			}
			if knownNestedPrivateKey(key) {
				continue
			}
			return nil, fmt.Errorf("nested child update contains unknown private key %q", key)
		}
		if key == "yield_type" {
			if yielded {
				projected[key] = cloneNestedValue(value)
			}
			continue
		}
		projected[key] = cloneNestedValue(value)
	}
	if raw, ok := update["__deleted_keys"]; ok {
		deleted, err := validateNestedDeletedKeys(raw, yielded)
		if err != nil {
			return nil, err
		}
		projected["__deleted_keys"] = deleted
	}
	return projected, nil
}

func validateNestedYieldEnvelope(update loom.State) error {
	if update["__yield_phase"] != "mid_step" {
		return fmt.Errorf("nested child yield has invalid phase")
	}
	for _, key := range []string{
		"__child_run_id", "__child_graph", "__child_graph_revision", "__child_yield_token",
	} {
		value, ok := update[key].(string)
		if !ok || value == "" {
			return fmt.Errorf("nested child yield has invalid %s", key)
		}
	}
	if _, ok := nestedPositiveInt64(update["__child_checkpoint_seq"]); !ok {
		return fmt.Errorf("nested child yield has invalid __child_checkpoint_seq")
	}
	if _, hasPending := update["__child_pending"]; hasPending {
		return fmt.Errorf("nested child yield has unsupported pending calls")
	}
	return nil
}

func allowedNestedYieldKey(key string) bool {
	switch key {
	case "__yield", "__yield_phase", "__child_run_id", "__child_graph",
		"__child_graph_revision", "__child_yield_token", "__child_checkpoint_seq",
		"yield_type":
		return true
	default:
		return false
	}
}

func validateNestedDeletedKeys(raw any, yielded bool) ([]string, error) {
	keys, ok := nestedStringSlice(raw)
	if !ok {
		return nil, fmt.Errorf("nested child update has invalid __deleted_keys")
	}
	if yielded {
		if reflect.DeepEqual(keys, []string{"__child_resume_input"}) {
			return keys, nil
		}
		return nil, fmt.Errorf("nested child yield has invalid continuation cleanup")
	}
	want := []string{
		"__child_run_id", "__child_graph", "__child_graph_revision",
		"__child_yield_token", "__child_checkpoint_seq", "__child_resume_input",
	}
	if !reflect.DeepEqual(keys, want) {
		return nil, fmt.Errorf("nested child success has invalid continuation cleanup")
	}
	return keys, nil
}

func nestedStringSlice(raw any) ([]string, bool) {
	switch values := raw.(type) {
	case []string:
		return append([]string(nil), values...), true
	case []any:
		result := make([]string, len(values))
		for index, value := range values {
			var ok bool
			result[index], ok = value.(string)
			if !ok {
				return nil, false
			}
		}
		return result, true
	default:
		return nil, false
	}
}

func knownNestedPrivateKey(key string) bool {
	switch key {
	case "__run_id", "__parent_run", "__parent_seq", "__parent_run_id",
		"__seq", "__checkpoint_seq", "__run_started_at",
		"__agent_id", "__agent_version", "__execution_scope",
		terminalAttributionStateKey,
		usageAccumulatorStateKey, "__budget_remaining", "__profile",
		"__system_prompt", "__active_skills", "__resumed_tool_results",
		"__error", "__failed_step", "__blocked", "__block_reason",
		"__delegate_to", "__delegate_error", "__delegate_failed",
		"__sse_send", "__deleted_keys":
		return true
	}
	return strings.HasPrefix(key, "__yield") ||
		strings.HasPrefix(key, "__toolloop_") ||
		strings.HasPrefix(key, "__child_") ||
		strings.HasPrefix(key, "__loop_count_")
}

func cloneNestedState(input loom.State) loom.State {
	cloned := make(loom.State, len(input))
	for key, value := range input {
		cloned[key] = cloneNestedValue(value)
	}
	return cloned
}

func cloneNestedValue(value any) any {
	switch value := value.(type) {
	case loom.State:
		return cloneNestedState(value)
	case map[string]any:
		cloned := make(map[string]any, len(value))
		for key, item := range value {
			cloned[key] = cloneNestedValue(item)
		}
		return cloned
	case []any:
		cloned := make([]any, len(value))
		for index, item := range value {
			cloned[index] = cloneNestedValue(item)
		}
		return cloned
	case []string:
		return append([]string(nil), value...)
	case UsageAccumulator:
		return value.clone()
	default:
		reflected := reflect.ValueOf(value)
		switch reflected.Kind() {
		case reflect.Slice:
			cloned := reflect.MakeSlice(reflected.Type(), reflected.Len(), reflected.Len())
			for index := 0; index < reflected.Len(); index++ {
				item := cloneNestedValue(reflected.Index(index).Interface())
				if item == nil {
					cloned.Index(index).Set(reflect.Zero(reflected.Type().Elem()))
				} else {
					cloned.Index(index).Set(reflect.ValueOf(item))
				}
			}
			return cloned.Interface()
		case reflect.Map:
			if reflected.Type().Key().Kind() != reflect.String {
				return value
			}
			cloned := reflect.MakeMapWithSize(reflected.Type(), reflected.Len())
			iterator := reflected.MapRange()
			for iterator.Next() {
				item := cloneNestedValue(iterator.Value().Interface())
				if item == nil {
					cloned.SetMapIndex(iterator.Key(), reflect.Zero(reflected.Type().Elem()))
				} else {
					cloned.SetMapIndex(iterator.Key(), reflect.ValueOf(item))
				}
			}
			return cloned.Interface()
		default:
			return value
		}
	}
}
