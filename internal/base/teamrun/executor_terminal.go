package teamrun

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/storeext"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
)

const frozenAttemptLeaseTTL = 120 * time.Second

func frozenGraphName(run TeamRun) string {
	return fmt.Sprintf(
		"%s:team_workflow:%s:v%d",
		run.WorkspaceID,
		run.WorkflowID,
		run.WorkflowVersion,
	)
}

func frozenAttemptID(run TeamRun) uuid.UUID {
	return frozenAttemptIDForEpoch(run, run.ExecutionLeaseEpoch)
}

func frozenAttemptIDForEpoch(run TeamRun, epoch ExecutionLeaseEpoch) uuid.UUID {
	identity := fmt.Sprintf(
		"teamrun-attempt:%s:%s:%d",
		run.WorkspaceID,
		run.RunID,
		epoch,
	)
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(identity))
}

func FrozenAttemptIDForEpoch(run TeamRun, epoch ExecutionLeaseEpoch) uuid.UUID {
	return frozenAttemptIDForEpoch(run, epoch)
}

func frozenAttemptRunStartedAt(run TeamRun) string {
	return run.UpdatedAt.UTC().Format(time.RFC3339Nano)
}

func FrozenAttemptRunStartedAt(run TeamRun) string {
	return frozenAttemptRunStartedAt(run)
}

func frozenExpectedRun(run TeamRun) loomruntime.ExpectedRunRecordV1 {
	teamID := run.TeamID
	workflowID := run.WorkflowID
	workflowVersion := run.WorkflowVersion
	runSnapshotID := run.RunSnapshotID
	return loomruntime.ExpectedRunRecordV1{
		SchemaVersion:    1,
		WorkspaceID:      run.WorkspaceID,
		RunID:            run.RunID,
		Agent:            frozenGraphName(run),
		AttributionScope: loomruntime.TerminalAttributionFixedWorkflow,
		TeamID:           &teamID,
		WorkflowID:       &workflowID,
		WorkflowVersion:  &workflowVersion,
		RunSnapshotID:    &runSnapshotID,
		RegisteredAt:     run.UpdatedAt.UTC(),
	}
}

func frozenAttemptAdmission(run TeamRun) loomruntime.FrozenAttemptAdmission {
	return loomruntime.FrozenAttemptAdmission{
		Record:            frozenExpectedRun(run),
		AttemptGeneration: int64(run.ExecutionLeaseEpoch),
		AttemptID:         frozenAttemptID(run),
		GraphName:         frozenGraphName(run),
		RunStartedAt:      frozenAttemptRunStartedAt(run),
		LeaseTTL:          frozenAttemptLeaseTTL,
	}
}

func (e *Executor) runtimeRecordStore() (loomruntime.TerminalRecordStore, error) {
	if e.RuntimeRecords != nil {
		return e.RuntimeRecords, nil
	}
	pool, ok := e.Transactions.(*pgxpool.Pool)
	if !ok || pool == nil {
		return nil, errors.New("team run executor runtime record store is unavailable")
	}
	return storeext.New(pool), nil
}

func (e *Executor) admitFrozenAttempt(
	ctx context.Context,
	run TeamRun,
) (loomruntime.RunAttemptLease, error) {
	if run.ExecutionLeaseEpoch < 1 {
		return loomruntime.RunAttemptLease{}, errors.New(
			"team run execution_lease_epoch must be positive for frozen admission",
		)
	}
	records, err := e.runtimeRecordStore()
	if err != nil {
		return loomruntime.RunAttemptLease{}, err
	}
	lease, err := loomruntime.AdmitFrozenAttempt(
		ctx,
		records,
		frozenAttemptAdmission(run),
	)
	if err != nil {
		return loomruntime.RunAttemptLease{}, fmt.Errorf(
			"admit frozen team run attempt: %w", err,
		)
	}
	return lease, nil
}

func advanceFrozenAttemptTx(
	ctx context.Context,
	tx pgx.Tx,
	current loomruntime.RunAttemptLease,
	next TeamRun,
) (loomruntime.RunAttemptLease, error) {
	if current.AttemptGeneration+1 != int64(next.ExecutionLeaseEpoch) ||
		current.AttemptID != frozenAttemptIDForEpoch(
			next, ExecutionLeaseEpoch(current.AttemptGeneration),
		) ||
		current.GraphName != frozenGraphName(next) {
		return loomruntime.RunAttemptLease{}, errors.New(
			"current frozen attempt does not match the preceding TeamRun epoch",
		)
	}
	advanced, updated, err := loomruntime.AdvanceFrozenAttemptTx(
		ctx,
		tx,
		loomruntime.FrozenAttemptAdvance{
			Current: loomruntime.AttemptLeaseOwner{
				WorkspaceID:  current.WorkspaceID,
				RunID:        current.RunID,
				Generation:   current.AttemptGeneration,
				AttemptID:    current.AttemptID,
				GraphName:    current.GraphName,
				RunStartedAt: current.RunStartedAt,
			},
			NextGeneration: int64(next.ExecutionLeaseEpoch),
			NextAttemptID:  frozenAttemptID(next),
			LeaseTTL:       frozenAttemptLeaseTTL,
		},
	)
	if err != nil {
		return loomruntime.RunAttemptLease{}, err
	}
	if !updated {
		return loomruntime.RunAttemptLease{}, errors.New(
			"current frozen attempt lost epoch advance CAS",
		)
	}
	return advanced, nil
}

func frozenAttemptOwner(lease loomruntime.RunAttemptLease) loomruntime.AttemptLeaseOwner {
	return loomruntime.AttemptLeaseOwner{
		WorkspaceID:  lease.WorkspaceID,
		RunID:        lease.RunID,
		Generation:   lease.AttemptGeneration,
		AttemptID:    lease.AttemptID,
		GraphName:    lease.GraphName,
		RunStartedAt: lease.RunStartedAt,
	}
}

func validateFrozenAttemptLease(run TeamRun, lease loomruntime.RunAttemptLease) error {
	if lease.WorkspaceID != run.WorkspaceID ||
		lease.RunID != run.RunID ||
		lease.AttemptGeneration != int64(run.ExecutionLeaseEpoch) ||
		lease.AttemptID != frozenAttemptID(run) ||
		lease.GraphName != frozenGraphName(run) {
		return errors.New("frozen attempt lease differs from TeamRun epoch identity")
	}
	if lease.State != loomruntime.AttemptLeaseActive &&
		lease.State != loomruntime.AttemptLeaseClosed {
		return fmt.Errorf("frozen attempt lease state %q cannot commit terminal", lease.State)
	}
	return nil
}

func frozenTerminalCandidate(
	run TeamRun,
	lease loomruntime.RunAttemptLease,
	status string,
	stopReason string,
	endedAt time.Time,
	usage loomruntime.UsageTotals,
	usageCoverage *loomruntime.UsageCoverage,
	usageComplete bool,
	usageIncompleteReason string,
	conversationID *string,
) (loomruntime.TerminalEntryV3, error) {
	startedAt, err := time.Parse(time.RFC3339Nano, lease.RunStartedAt)
	if err != nil {
		return loomruntime.TerminalEntryV3{}, fmt.Errorf("parse frozen attempt start: %w", err)
	}
	endedAt = endedAt.UTC()
	if endedAt.Before(startedAt) {
		endedAt = startedAt
	}
	teamID := run.TeamID
	workflowID := run.WorkflowID
	workflowVersion := run.WorkflowVersion
	runSnapshotID := run.RunSnapshotID
	selfExclusive := loomruntime.TerminalUsage{
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
		CostUSD:      usage.CostUSD,
		ToolCalls:    usage.ToolCalls,
	}
	var usageCompletePtr *bool
	// The reason is the discriminator: callers that never set the annotation
	// carry zero false/empty and must produce a usage-complete entry.
	if !usageComplete && usageIncompleteReason != "" {
		falseValue := false
		usageCompletePtr = &falseValue
	}
	var usageHasTokens, usageHasCost *bool
	var usageSources []string
	if usageCoverage != nil {
		hasTokens := usageCoverage.HasTokens
		hasCost := usageCoverage.HasCost
		usageHasTokens = &hasTokens
		usageHasCost = &hasCost
		usageSources = append([]string(nil), usageCoverage.Sources...)
	}
	return loomruntime.TerminalEntryV3{
		SchemaVersion:         3,
		RunID:                 run.RunID,
		Agent:                 frozenGraphName(run),
		Tenant:                run.WorkspaceID,
		AttributionScope:      loomruntime.TerminalAttributionFixedWorkflow,
		TeamID:                &teamID,
		WorkflowID:            &workflowID,
		WorkflowVersion:       &workflowVersion,
		RunSnapshotID:         &runSnapshotID,
		ConversationID:        conversationID,
		Status:                status,
		StopReason:            stopReason,
		StartedAt:             startedAt.Format(time.RFC3339Nano),
		EndedAt:               endedAt.Format(time.RFC3339Nano),
		DurationMs:            endedAt.Sub(startedAt).Milliseconds(),
		TokensIn:              usage.InputTokens,
		TokensOut:             usage.OutputTokens,
		CostUSD:               usage.CostUSD,
		ToolCalls:             usage.ToolCalls,
		SelfExclusive:         selfExclusive,
		ChildBreakdown:        []loomruntime.TerminalChildBreakdownV3{},
		SubtreeTotal:          selfExclusive,
		UsageComplete:         usageCompletePtr,
		UsageIncompleteReason: usageIncompleteReason,
		UsageHasTokens:        usageHasTokens,
		UsageHasCost:          usageHasCost,
		UsageSources:          usageSources,
	}, nil
}

func (e *Executor) commitFrozenNormalTerminal(
	ctx context.Context,
	run TeamRun,
	status string,
	stopReason string,
	usage loomruntime.UsageTotals,
	usageCoverage *loomruntime.UsageCoverage,
	usageComplete bool,
	usageIncompleteReason string,
) error {
	records, err := e.runtimeRecordStore()
	if err != nil {
		return err
	}
	sink, err := loomruntime.NewLineageTerminalSink(records)
	if err != nil {
		return fmt.Errorf("construct frozen terminal sink: %w", err)
	}
	coordinator, ok := sink.(loomruntime.NormalTerminalCoordinator)
	if !ok || !coordinator.A4NormalTerminalGuaranteed() {
		return loomruntime.ErrA4NormalTerminalUnsupported
	}
	tx, err := e.Transactions.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin frozen terminal fact read: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	stateStore := loomruntime.NewPGTerminalStateStore()
	if err := stateStore.LockTerminalRun(ctx, tx, run.WorkspaceID, run.RunID); err != nil {
		return fmt.Errorf("lock frozen terminal fact read: %w", err)
	}
	lease, present, err := stateStore.ReadAttemptLeaseForUpdate(
		ctx, tx, run.WorkspaceID, run.RunID,
	)
	if err != nil {
		return fmt.Errorf("read frozen terminal attempt lease: %w", err)
	}
	if !present {
		return errors.New("frozen terminal attempt lease is missing")
	}
	if err := validateFrozenAttemptLease(run, lease); err != nil {
		return err
	}
	marker, markerPresent, err := stateStore.ReadTerminalMarkerForUpdate(
		ctx, tx, run.WorkspaceID, run.RunID,
	)
	if err != nil {
		return fmt.Errorf("read frozen terminal marker: %w", err)
	}
	endedAt := e.now()
	if markerPresent && marker.Source == loomruntime.TerminalMarkerSourceNormal &&
		marker.Phase == loomruntime.TerminalMarkerPhaseFinal &&
		marker.AttemptGeneration == lease.AttemptGeneration &&
		marker.AttemptID == lease.AttemptID &&
		marker.RunStartedAt == lease.RunStartedAt &&
		string(marker.Status) == status && marker.StopReason == stopReason {
		endedAt = marker.TerminalAt
	}
	if err := tx.Rollback(ctx); err != nil {
		return fmt.Errorf("finish frozen terminal fact read: %w", err)
	}
	conversationID, err := e.frozenTerminalConversationID(ctx, run)
	if err != nil {
		return err
	}
	candidate, err := frozenTerminalCandidate(
		run, lease, status, stopReason, endedAt, usage, usageCoverage,
		usageComplete, usageIncompleteReason, conversationID,
	)
	if err != nil {
		return err
	}
	if err := coordinator.CommitNormalTerminal(
		ctx,
		loomruntime.NormalTerminalCommit{
			Candidate: candidate,
			Owner:     frozenAttemptOwner(lease),
		},
	); err != nil {
		return fmt.Errorf("commit frozen normal terminal: %w", err)
	}
	return nil
}

func (e *Executor) frozenTerminalConversationID(
	ctx context.Context,
	run TeamRun,
) (*string, error) {
	if e.Consumer == nil || e.Consumer.Snapshots == nil ||
		run.RunSnapshotID == "" {
		return nil, nil
	}
	runSnapshot, err := e.Consumer.Snapshots.GetByRunID(
		ctx, run.WorkspaceID, run.RunSnapshotID,
	)
	if err != nil {
		return nil, fmt.Errorf("read frozen terminal conversation snapshot: %w", err)
	}
	var trigger struct {
		SchemaVersion int    `json:"schema_version"`
		Type          string `json:"type"`
		SourceRef     string `json:"source_ref"`
	}
	if err := decodeExact(runSnapshot.TriggerSourceV2, &trigger); err != nil ||
		trigger.SchemaVersion != 1 ||
		trigger.Type != "conversation_explicit" ||
		trigger.SourceRef == "" {
		return nil, nil
	}
	return &trigger.SourceRef, nil
}

func (e *Executor) replayFrozenTerminalTx(
	ctx context.Context,
	tx pgx.Tx,
	run TeamRun,
	task *taskqueue.Task,
	marker loomruntime.TerminalMarkerV1,
	lease loomruntime.RunAttemptLease,
) (TeamRun, error) {
	if err := validateFrozenAttemptLease(run, lease); err != nil {
		return TeamRun{}, err
	}
	owner := frozenAttemptOwner(lease)
	if marker.Source != loomruntime.TerminalMarkerSourceNormal ||
		marker.Phase != loomruntime.TerminalMarkerPhaseFinal ||
		marker.AttemptGeneration != owner.Generation ||
		marker.AttemptID != owner.AttemptID ||
		marker.Agent != frozenGraphName(run) ||
		marker.AttributionScope != loomruntime.TerminalAttributionFixedWorkflow ||
		marker.TeamID == nil || *marker.TeamID != run.TeamID ||
		marker.WorkflowID == nil || *marker.WorkflowID != run.WorkflowID ||
		marker.WorkflowVersion == nil || int(*marker.WorkflowVersion) != run.WorkflowVersion ||
		marker.RunSnapshotID == nil || *marker.RunSnapshotID != run.RunSnapshotID ||
		marker.RunStartedAt != owner.RunStartedAt {
		return TeamRun{}, errors.New("frozen terminal marker differs from TeamRun epoch identity")
	}
	if run.CurrentExecutorID == nil || *run.CurrentExecutorID == "" {
		return TeamRun{}, errors.New("running TeamRun lacks current executor for terminal replay")
	}
	executorID := *run.CurrentExecutorID
	switch marker.Status {
	case loomruntime.TerminalMarkerStatusSuccess:
		return e.Runs.SucceedTx(ctx, tx, SucceedRequest{
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
			OccurredAt:                  marker.TerminalAt,
		})
	case loomruntime.TerminalMarkerStatusFailed:
		code := ErrorCode(marker.StopReason)
		if !ValidateErrorCode(code) || code == ErrorCodeCancelled {
			return TeamRun{}, fmt.Errorf(
				"frozen failed terminal stop_reason %q is not a TeamRun error code",
				marker.StopReason,
			)
		}
		return e.Runs.FailTx(ctx, tx, FailRequest{
			WorkspaceID:                 run.WorkspaceID,
			RunID:                       run.RunID,
			ExpectedStatus:              StatusRunning,
			ExpectedTeamRunGeneration:   run.Generation,
			ExpectedExecutionLeaseEpoch: run.ExecutionLeaseEpoch,
			ExpectedResumeGeneration:    run.ResumeGeneration,
			ExecutorID:                  executorID,
			ErrorCode:                   code,
			Cause:                       errors.New(marker.StopReason),
			IdempotencyKey:              executorTerminalKey(task.ID, StatusFailed),
			Actor:                       executorID,
			Source:                      consumerSource,
			OccurredAt:                  marker.TerminalAt,
		})
	default:
		return TeamRun{}, fmt.Errorf(
			"frozen final terminal status %q cannot be replayed", marker.Status,
		)
	}
}

func writeFanoutYieldMarkerTx(
	ctx context.Context,
	tx pgx.Tx,
	running TeamRun,
	parked TeamRun,
	checkpoint WorkflowCheckpointV1,
	now time.Time,
) error {
	stateStore := loomruntime.NewPGTerminalStateStore()
	lease, present, err := stateStore.ReadAttemptLeaseForUpdate(ctx, tx, running.WorkspaceID, running.RunID)
	if err != nil || !present {
		if err == nil {
			err = errors.New("fanout creator attempt lease is missing")
		}
		return fmt.Errorf("read fanout creator attempt lease: %w", err)
	}
	if lease.AttemptGeneration != int64(running.ExecutionLeaseEpoch) || lease.AttemptID != frozenAttemptID(running) ||
		lease.State != loomruntime.AttemptLeaseActive {
		return errors.New("fanout creator attempt lease differs from running TeamRun")
	}
	teamID, workflowID, snapshotID := running.TeamID, running.WorkflowID, running.RunSnapshotID
	workflowVersion := int32(running.WorkflowVersion)
	graphName := frozenGraphName(running)
	checkpointSequence := int64(parked.ResumeGeneration)
	auditVersion := int16(3)
	usage := loomruntime.UsageTotals{}
	if len(checkpoint.Usage) != 0 {
		accumulator, err := loomruntime.UnmarshalUsageAccumulator(checkpoint.Usage)
		if err != nil {
			return fmt.Errorf("decode fanout checkpoint usage: %w", err)
		}
		usage = accumulator.Totals()
	}
	marker := loomruntime.TerminalMarkerV1{
		WorkspaceID: running.WorkspaceID, RunID: running.RunID, SchemaVersion: 1,
		AttemptGeneration: lease.AttemptGeneration, AttemptID: lease.AttemptID,
		Agent: graphName, AttributionScope: loomruntime.TerminalAttributionFixedWorkflow,
		TeamID: &teamID, WorkflowID: &workflowID, WorkflowVersion: &workflowVersion,
		RunSnapshotID: &snapshotID, RunStartedAt: lease.RunStartedAt,
		Phase: loomruntime.TerminalMarkerPhaseYielded, Status: loomruntime.TerminalMarkerStatusYielded,
		StopReason: "yielded", Source: loomruntime.TerminalMarkerSourceNormal, TerminalAt: now,
		EvidenceKind: loomruntime.TerminalMarkerEvidenceCheckpoint, CheckpointGraph: &graphName,
		CheckpointSeq: &checkpointSequence, CheckpointSavedAt: &checkpoint.WrittenAt,
		UsageInputTokens: int64(usage.InputTokens), UsageOutputTokens: int64(usage.OutputTokens),
		UsageCostUSD: usage.CostUSD, UsageToolCalls: int64(usage.ToolCalls),
		AuditState: loomruntime.TerminalMarkerAuditMaterialized, AuditSchemaVersion: &auditVersion,
		LineageState: loomruntime.TerminalMarkerLineagePending, CreatedAt: now, UpdatedAt: now,
	}
	if _, err := stateStore.ApplyTerminalMarkerTransition(ctx, tx, marker); err != nil {
		return fmt.Errorf("write fanout yielded marker: %w", err)
	}
	return nil
}
