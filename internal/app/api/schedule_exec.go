package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/schedule"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

var ErrWorkflowScheduleAdmissionUnavailable = errors.New(
	"workflow schedule admission is unavailable",
)

// ScheduleTransactionBeginner supplies the caller-owned transaction used by a
// workflow schedule occurrence.
type ScheduleTransactionBeginner interface {
	Begin(context.Context) (pgx.Tx, error)
}

// WorkflowScheduleAdmissionService locks and validates all live workflow gates
// inside the caller-owned transaction before returning a complete snapshot.
type WorkflowScheduleAdmissionService interface {
	AdmitWorkflowScheduleTx(
		context.Context,
		pgx.Tx,
		WorkflowScheduleAdmissionRequest,
	) (snapshot.TeamRunSnapshot, error)
	RecordFixedWorkflowAdmissionDenial(
		context.Context,
		workflow.FixedWorkflowAdmissionDenialAttempt,
	) (*workflow.FixedWorkflowAdmissionDenialRecord, error)
}

// WorkflowScheduleAdmissionRequest is the immutable schedule occurrence input
// presented to the workflow admission boundary.
type WorkflowScheduleAdmissionRequest struct {
	WorkspaceID   string
	ScheduleID    string
	WorkflowID    string
	OccurrenceKey string
	ScheduledFor  time.Time
}

// NewWorkflowScheduleAdmissionService adapts the workflow-owned admission
// store to the API-owned schedule request boundary.
func NewWorkflowScheduleAdmissionService(
	store *workflow.Store,
) WorkflowScheduleAdmissionService {
	if store == nil {
		return nil
	}
	return workflowScheduleAdmissionAdapter{store: store}
}

type workflowScheduleAdmissionAdapter struct {
	store *workflow.Store
}

func (a workflowScheduleAdmissionAdapter) AdmitWorkflowScheduleTx(
	ctx context.Context,
	tx pgx.Tx,
	request WorkflowScheduleAdmissionRequest,
) (snapshot.TeamRunSnapshot, error) {
	return a.store.AdmitWorkflowScheduleTx(
		ctx,
		tx,
		workflow.WorkflowScheduleAdmissionRequest{
			WorkspaceID:   request.WorkspaceID,
			ScheduleID:    request.ScheduleID,
			WorkflowID:    request.WorkflowID,
			OccurrenceKey: request.OccurrenceKey,
			ScheduledFor:  request.ScheduledFor,
		},
	)
}

func (a workflowScheduleAdmissionAdapter) RecordFixedWorkflowAdmissionDenial(
	ctx context.Context,
	attempt workflow.FixedWorkflowAdmissionDenialAttempt,
) (*workflow.FixedWorkflowAdmissionDenialRecord, error) {
	return a.store.RecordFixedWorkflowAdmissionDenial(ctx, attempt)
}

// WorkflowScheduleStage identifies a completed transactional write boundary.
type WorkflowScheduleStage string

const (
	WorkflowScheduleStageScheduleLocked      WorkflowScheduleStage = "schedule_locked"
	WorkflowScheduleStageOccurrenceInserted  WorkflowScheduleStage = "occurrence_inserted"
	WorkflowScheduleStageAdmissionDecided    WorkflowScheduleStage = "admission_decided"
	WorkflowScheduleStageSnapshotCreated     WorkflowScheduleStage = "snapshot_created"
	WorkflowScheduleStageTaskEnqueued        WorkflowScheduleStage = "task_enqueued"
	WorkflowScheduleStageOccurrenceCommitted WorkflowScheduleStage = "occurrence_committed"
	WorkflowScheduleStageCursorAdvanced      WorkflowScheduleStage = "cursor_advanced"
	WorkflowScheduleStageCommitBefore        WorkflowScheduleStage = "transaction_commit_before"
	WorkflowScheduleStageCommitAfter         WorkflowScheduleStage = "transaction_commit_after"
)

// SweepSchedules enqueues every due schedule in the durable task ledger and
// only then marks it as run. Execution is left to the leased task worker.
func (s *Server) SweepSchedules(ctx context.Context, now time.Time) error {
	if s.AgentSchedules == nil {
		return fmt.Errorf("agent schedule store is not available")
	}
	if s.Tasks == nil {
		return fmt.Errorf("task queue is not available")
	}
	due, err := s.AgentSchedules.DueSchedules(ctx, now)
	if err != nil {
		return err
	}
	var itemErrors []error
	for _, item := range due {
		var itemErr error
		switch item.TargetKind {
		case schedule.TargetAgent:
			itemErr = s.sweepAgentSchedule(ctx, item, now)
		case schedule.TargetTeamWorkflow:
			itemErr = s.sweepWorkflowSchedule(ctx, item, now)
		default:
			itemErr = fmt.Errorf("unsupported schedule target kind %q", item.TargetKind)
		}
		if itemErr != nil {
			itemErrors = append(itemErrors, fmt.Errorf(
				"sweep schedule %q in workspace %q: %w",
				item.ID, item.WorkspaceID, itemErr,
			))
		}
	}
	return errors.Join(itemErrors...)
}

func (s *Server) sweepAgentSchedule(
	ctx context.Context,
	item schedule.Schedule,
	now time.Time,
) error {
	if s.Registry == nil {
		return fmt.Errorf("agent registry is not available")
	}
	record, err := s.Registry.Get(ctx, item.WorkspaceID, item.Agent)
	if err != nil {
		return fmt.Errorf(
			"resolve schedule %q agent %q in workspace %q: %w",
			item.ID, item.Agent, item.WorkspaceID, err,
		)
	}
	if record == nil {
		return fmt.Errorf(
			"resolve schedule %q agent %q in workspace %q: registry returned no record",
			item.ID, item.Agent, item.WorkspaceID,
		)
	}
	_, parseIDErr := uuid.Parse(record.ID)
	if record.Name != item.Agent || parseIDErr != nil || record.Version < 1 ||
		record.WorkspaceID != item.WorkspaceID {
		return fmt.Errorf(
			"resolve schedule %q agent %q in workspace %q: invalid registry identity %q/%q@%d in workspace %q",
			item.ID, item.Agent, item.WorkspaceID,
			record.Name, record.ID, record.Version, record.WorkspaceID,
		)
	}
	payload, err := json.Marshal(struct {
		Agent   string `json:"agent"`
		Message string `json:"message"`
	}{
		Agent: item.Agent, Message: item.Message,
	})
	if err != nil {
		return fmt.Errorf("marshal schedule %q task: %w", item.ID, err)
	}
	task := &taskqueue.Task{
		ID:                    "task-" + uuid.NewString(),
		WorkspaceID:           item.WorkspaceID,
		Agent:                 record.Name,
		AgentID:               record.ID,
		AgentVersion:          record.Version,
		IdentityKind:          taskqueue.IdentityAgent,
		IdentitySchemaVersion: 2,
		ExecutionScope:        execution.ScopeLegacyOrchestrator,
		Source:                "calendar",
		Kind:                  "chat",
		Payload:               payload,
	}
	if err := s.Tasks.Enqueue(ctx, task); err != nil {
		return fmt.Errorf("enqueue schedule %q: %w", item.ID, err)
	}
	if err := s.AgentSchedules.MarkRan(ctx, item.WorkspaceID, item.ID, now); err != nil {
		return fmt.Errorf("mark schedule %q ran: %w", item.ID, err)
	}
	return nil
}

func (s *Server) sweepWorkflowSchedule(
	ctx context.Context,
	listed schedule.Schedule,
	now time.Time,
) error {
	if s.WorkflowScheduleAdmission == nil {
		return ErrWorkflowScheduleAdmissionUnavailable
	}
	if s.ScheduleTransactions == nil {
		return errors.New("workflow schedule transaction source is not available")
	}
	if s.Snapshots == nil {
		return errors.New("team run snapshot store is not available")
	}

	due, err := schedule.DueScheduledFor(listed, now)
	if err != nil {
		return fmt.Errorf("resolve workflow schedule occurrence: %w", err)
	}
	if due == nil {
		return schedule.ErrScheduleNotDue
	}
	candidate := schedule.Occurrence{
		WorkspaceID:      listed.WorkspaceID,
		OccurrenceKey:    schedule.OccurrenceKey(listed, due.ScheduledFor),
		ScheduleID:       listed.ID,
		TargetWorkflowID: listed.TargetWorkflowID,
		ScheduledFor:     due.ScheduledFor,
		Status:           schedule.OccurrencePending,
	}

	tx, err := s.ScheduleTransactions.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin workflow schedule transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	locked, err := s.AgentSchedules.LockDueTx(
		ctx,
		tx,
		listed.WorkspaceID,
		listed.ID,
		now,
	)
	if err != nil {
		if errors.Is(err, schedule.ErrScheduleNotDue) {
			existing, inserted, insertErr := s.AgentSchedules.InsertOccurrenceTx(
				ctx,
				tx,
				&candidate,
			)
			if insertErr == nil && !inserted {
				return validateCommittedOccurrence(existing)
			}
		}
		return fmt.Errorf("lock workflow schedule: %w", err)
	}
	if locked.TargetKind != schedule.TargetTeamWorkflow {
		return fmt.Errorf("locked schedule target kind = %q", locked.TargetKind)
	}
	lockedDue, err := schedule.DueScheduledFor(*locked, now)
	if err != nil {
		return fmt.Errorf("resolve locked workflow schedule occurrence: %w", err)
	}
	if lockedDue == nil {
		return schedule.ErrScheduleNotDue
	}
	if err := s.runWorkflowScheduleStepHook(
		ctx,
		WorkflowScheduleStageScheduleLocked,
	); err != nil {
		return err
	}
	candidate = schedule.Occurrence{
		WorkspaceID:      locked.WorkspaceID,
		OccurrenceKey:    schedule.OccurrenceKey(*locked, lockedDue.ScheduledFor),
		ScheduleID:       locked.ID,
		TargetWorkflowID: locked.TargetWorkflowID,
		ScheduledFor:     lockedDue.ScheduledFor,
		Status:           schedule.OccurrencePending,
	}
	occurrence, inserted, err := s.AgentSchedules.InsertOccurrenceTx(
		ctx,
		tx,
		&candidate,
	)
	if err != nil {
		return fmt.Errorf("insert workflow schedule occurrence: %w", err)
	}
	if !inserted {
		return validateCommittedOccurrence(occurrence)
	}
	if err := s.runWorkflowScheduleStepHook(
		ctx,
		WorkflowScheduleStageOccurrenceInserted,
	); err != nil {
		return err
	}

	admitted, err := s.WorkflowScheduleAdmission.AdmitWorkflowScheduleTx(
		ctx,
		tx,
		WorkflowScheduleAdmissionRequest{
			WorkspaceID:   occurrence.WorkspaceID,
			ScheduleID:    occurrence.ScheduleID,
			WorkflowID:    occurrence.TargetWorkflowID,
			OccurrenceKey: occurrence.OccurrenceKey,
			ScheduledFor:  occurrence.ScheduledFor,
		},
	)
	if err != nil {
		var denial *workflow.FixedWorkflowAdmissionDenial
		if errors.As(err, &denial) {
			if rollbackErr := tx.Rollback(ctx); rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
				return errors.Join(
					fmt.Errorf("admit workflow schedule occurrence: %w", denial),
					fmt.Errorf("rollback denied workflow schedule occurrence: %w", rollbackErr),
				)
			}
			record, auditErr := s.WorkflowScheduleAdmission.RecordFixedWorkflowAdmissionDenial(
				ctx,
				workflow.FixedWorkflowAdmissionDenialAttempt{
					WorkspaceID:         occurrence.WorkspaceID,
					WorkflowID:          occurrence.TargetWorkflowID,
					WorkflowVersion:     denial.WorkflowVersion,
					TriggerType:         "schedule",
					AdmissionAttemptKey: occurrence.OccurrenceKey,
					ReasonCode:          denial.ReasonCode,
				},
			)
			if auditErr != nil {
				return errors.Join(
					fmt.Errorf("admit workflow schedule occurrence: %w", denial),
					fmt.Errorf("record workflow schedule admission denial: %w", auditErr),
				)
			}
			return fmt.Errorf("admit workflow schedule occurrence: %w", &workflow.FixedWorkflowAdmissionDenial{
				ReasonCode:      record.ReasonCode,
				WorkflowVersion: record.WorkflowVersion,
			})
		}
		return fmt.Errorf("admit workflow schedule occurrence: %w", err)
	}
	if err := validateWorkflowScheduleSnapshot(admitted, *occurrence); err != nil {
		return fmt.Errorf("validate workflow schedule snapshot: %w", err)
	}
	if err := s.runWorkflowScheduleStepHook(
		ctx,
		WorkflowScheduleStageAdmissionDecided,
	); err != nil {
		return err
	}
	createdSnapshot, err := s.Snapshots.CreateTx(ctx, tx, admitted)
	if err != nil {
		return fmt.Errorf("create workflow schedule snapshot: %w", err)
	}
	if err := s.runWorkflowScheduleStepHook(
		ctx,
		WorkflowScheduleStageSnapshotCreated,
	); err != nil {
		return err
	}
	task := &taskqueue.Task{
		ID:                    "task-" + uuid.NewString(),
		WorkspaceID:           occurrence.WorkspaceID,
		IdentityKind:          taskqueue.IdentityTeamWorkflow,
		IdentitySchemaVersion: 2,
		WorkflowID:            occurrence.TargetWorkflowID,
		WorkflowVersion:       createdSnapshot.WorkflowVersion,
		RunSnapshotID:         createdSnapshot.RunID,
		Source:                "schedule",
		Kind:                  "team_workflow",
		Payload:               json.RawMessage(`{}`),
	}
	if err := s.Tasks.EnqueueTx(ctx, tx, task); err != nil {
		return fmt.Errorf("enqueue workflow schedule task: %w", err)
	}
	if err := s.runWorkflowScheduleStepHook(
		ctx,
		WorkflowScheduleStageTaskEnqueued,
	); err != nil {
		return err
	}
	occurrence.Status = schedule.OccurrenceCommitted
	occurrence.WorkflowVersion = createdSnapshot.WorkflowVersion
	occurrence.RunSnapshotID = createdSnapshot.RunID
	occurrence.TaskID = task.ID
	if err := s.AgentSchedules.CommitOccurrenceTx(ctx, tx, *occurrence); err != nil {
		return err
	}
	if err := s.runWorkflowScheduleStepHook(
		ctx,
		WorkflowScheduleStageOccurrenceCommitted,
	); err != nil {
		return err
	}
	if err := s.AgentSchedules.AdvanceCursorTx(
		ctx,
		tx,
		*locked,
		occurrence.ScheduledFor,
	); err != nil {
		return fmt.Errorf("advance workflow schedule cursor: %w", err)
	}
	if err := s.runWorkflowScheduleStepHook(
		ctx,
		WorkflowScheduleStageCursorAdvanced,
	); err != nil {
		return err
	}
	if err := s.runWorkflowScheduleStepHook(
		ctx,
		WorkflowScheduleStageCommitBefore,
	); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit workflow schedule transaction: %w", err)
	}
	if err := s.runWorkflowScheduleStepHook(
		ctx,
		WorkflowScheduleStageCommitAfter,
	); err != nil {
		return err
	}
	return nil
}

func (s *Server) runWorkflowScheduleStepHook(
	ctx context.Context,
	stage WorkflowScheduleStage,
) error {
	if s.WorkflowScheduleStepHook == nil {
		return nil
	}
	if err := s.WorkflowScheduleStepHook(ctx, stage); err != nil {
		return fmt.Errorf("workflow schedule step %s: %w", stage, err)
	}
	return nil
}

func validateCommittedOccurrence(occurrence *schedule.Occurrence) error {
	if occurrence == nil ||
		occurrence.Status != schedule.OccurrenceCommitted ||
		occurrence.WorkflowVersion < 1 ||
		occurrence.RunSnapshotID == "" ||
		occurrence.TaskID == "" {
		return errors.New("existing workflow schedule occurrence is not committed")
	}
	return nil
}

func validateWorkflowScheduleSnapshot(
	admitted snapshot.TeamRunSnapshot,
	occurrence schedule.Occurrence,
) error {
	if admitted.RunID == "" ||
		admitted.WorkspaceID != occurrence.WorkspaceID ||
		admitted.TeamID == "" ||
		admitted.SnapshotSchemaVersion != 2 ||
		admitted.Mode != "fixed_workflow" ||
		admitted.WorkflowID != occurrence.TargetWorkflowID ||
		admitted.WorkflowVersion < 1 ||
		admitted.ArtifactWorkflowID != occurrence.TargetWorkflowID ||
		admitted.ArtifactWorkflowVersion != admitted.WorkflowVersion {
		return errors.New("admission returned a mismatched fixed workflow identity")
	}
	if admitted.LeadAvatarID != "" ||
		admitted.LeadAvatarVersion != 0 ||
		len(admitted.WorkerVersions) != 0 ||
		len(admitted.TeamWorkerSnapshot) != 0 ||
		admitted.ArtifactRef != "" ||
		len(admitted.InlineDependencies) != 0 ||
		admitted.TriggerSource != "" {
		return errors.New("admission returned forbidden fixed workflow fields")
	}
	var trigger struct {
		SchemaVersion int    `json:"schema_version"`
		Type          string `json:"type"`
		SourceRef     string `json:"source_ref"`
		OccurrenceKey string `json:"occurrence_key"`
	}
	if err := decodeExactJSON(admitted.TriggerSourceV2, &trigger); err != nil {
		return fmt.Errorf("decode trigger source: %w", err)
	}
	if trigger.SchemaVersion != 1 ||
		trigger.Type != "schedule" ||
		trigger.SourceRef != occurrence.ScheduleID ||
		trigger.OccurrenceKey != occurrence.OccurrenceKey {
		return errors.New("admission returned a mismatched schedule trigger")
	}
	var associations struct {
		SchemaVersion    int     `json:"schema_version"`
		ParentRunID      *string `json:"parent_run_id"`
		SourceSnapshotID *string `json:"source_snapshot_id"`
		TaskGroupID      *string `json:"task_group_id"`
	}
	if err := decodeExactJSON(admitted.RunAssociations, &associations); err != nil {
		return fmt.Errorf("decode run associations: %w", err)
	}
	if associations.SchemaVersion != 1 ||
		associations.ParentRunID != nil ||
		associations.SourceSnapshotID != nil ||
		associations.TaskGroupID != nil {
		return errors.New("schedule run associations must be empty")
	}
	return nil
}

func decodeExactJSON(raw json.RawMessage, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
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
