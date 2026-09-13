package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jinyitao123/weave/internal/base/execution"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/app/workflowadmission"
	"github.com/jinyitao123/weave/internal/app/workflowcatalog"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/kernel/publication"
	"github.com/jinyitao123/weave/internal/kernel/schedule"
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
	PrepareWorkflowScheduleTx(
		context.Context,
		pgx.Tx,
		WorkflowScheduleAdmissionRequest, frozen.ArtifactEnvelopeV1,
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
	store *workflowcatalog.Store,
	artifacts *workflow.ArtifactStore,
) WorkflowScheduleAdmissionService {
	if store == nil || artifacts == nil {
		return nil
	}
	return workflowScheduleAdmissionAdapter{store: store, artifacts: artifacts}
}

type workflowScheduleAdmissionAdapter struct {
	store     *workflowcatalog.Store
	artifacts *workflow.ArtifactStore
}

func (a workflowScheduleAdmissionAdapter) PrepareWorkflowScheduleTx(
	ctx context.Context,
	tx pgx.Tx,
	request WorkflowScheduleAdmissionRequest, envelope frozen.ArtifactEnvelopeV1,
) (snapshot.TeamRunSnapshot, error) {
	return a.store.PrepareWorkflowScheduleTx(
		ctx,
		tx,
		workflow.WorkflowScheduleAdmissionRequest{
			WorkspaceID:   request.WorkspaceID,
			ScheduleID:    request.ScheduleID,
			WorkflowID:    request.WorkflowID,
			OccurrenceKey: request.OccurrenceKey,
			ScheduledFor:  request.ScheduledFor,
		}, envelope,
	)
}

func (a workflowScheduleAdmissionAdapter) RecordFixedWorkflowAdmissionDenial(
	ctx context.Context,
	attempt workflow.FixedWorkflowAdmissionDenialAttempt,
) (*workflow.FixedWorkflowAdmissionDenialRecord, error) {
	return a.artifacts.RecordFixedWorkflowAdmissionDenial(ctx, attempt)
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

// SweepSchedules admits due team-workflow occurrences into the durable task
// ledger. Retired single-agent schedules remain stored but are never executed.
func (s *Server) SweepSchedules(ctx context.Context, now time.Time) error {
	if s.AgentSchedules == nil {
		return fmt.Errorf("workflow schedule store is not available")
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
		itemCtx := execution.WithSubject(ctx, execution.Subject{WorkspaceID: item.WorkspaceID, ServiceID: "workflow-schedule:" + item.ID})
		itemErr := s.sweepWorkflowSchedule(itemCtx, item, now)
		if itemErr != nil {
			itemErrors = append(itemErrors, fmt.Errorf(
				"sweep schedule %q in workspace %q: %w",
				item.ID, item.WorkspaceID, itemErr,
			))
		}
	}
	return errors.Join(itemErrors...)
}

func (s *Server) sweepWorkflowSchedule(
	ctx context.Context,
	listed schedule.Schedule,
	now time.Time,
) error {
	if listed.TargetKind != schedule.TargetTeamWorkflow {
		return fmt.Errorf("unsupported workflow schedule target %q", listed.TargetKind)
	}
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

	admissions := s.workflowAdmissions()
	if admissions == nil || s.Workflow == nil {
		return ErrWorkflowScheduleAdmissionUnavailable
	}
	requestID := "schedule:" + candidate.OccurrenceKey
	if _, err := admissions.Get(ctx, listed.WorkspaceID, requestID); err == nil {
		_, err = admissions.Admit(ctx, listed.WorkspaceID, requestID, s.associateWorkflowAdmission)
		return err
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	envelope, err := s.Workflow.ResolvePublished(ctx, listed.WorkspaceID, listed.TargetWorkflowID, nil)
	if err != nil {
		return err
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
		if occurrence.Status == schedule.OccurrenceCommitted {
			return validateCommittedOccurrence(occurrence)
		}
		_ = tx.Rollback(ctx)
		_, err = admissions.Admit(ctx, listed.WorkspaceID, "schedule:"+occurrence.OccurrenceKey, s.associateWorkflowAdmission)
		return err
	}

	if err := s.runWorkflowScheduleStepHook(
		ctx,
		WorkflowScheduleStageOccurrenceInserted,
	); err != nil {
		return err
	}

	admitted, err := s.WorkflowScheduleAdmission.PrepareWorkflowScheduleTx(
		ctx,
		tx,
		WorkflowScheduleAdmissionRequest{
			WorkspaceID:   occurrence.WorkspaceID,
			ScheduleID:    occurrence.ScheduleID,
			WorkflowID:    occurrence.TargetWorkflowID,
			OccurrenceKey: occurrence.OccurrenceKey,
			ScheduledFor:  occurrence.ScheduledFor,
		}, envelope,
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
	identity := listed.WorkspaceID + "\x00" + occurrence.OccurrenceKey
	runID := "run-" + uuid.NewSHA1(uuid.NameSpaceOID, []byte("schedule-run:"+identity)).String()
	taskID := "task-" + uuid.NewSHA1(uuid.NameSpaceOID, []byte("schedule-task:"+identity)).String()
	captured, _ := json.Marshal(locked)
	intent := publication.PublishedRunRequest{Version: publication.ContractVersion, RequestID: "schedule:" + occurrence.OccurrenceKey,
		Revision: publication.CandidateRevision(envelope), RunID: runID, TaskID: taskID, Input: json.RawMessage(`{}`), InputVersion: "schedule:" + occurrence.OccurrenceKey,
		Trigger: publication.PublishedTrigger{Type: "schedule", SourceRef: occurrence.ScheduleID, OccurrenceKey: occurrence.OccurrenceKey}}
	if _, err = admissions.ReserveTx(ctx, tx, intent, workflowadmission.Target{TeamID: admitted.TeamID, ScheduleID: occurrence.ScheduleID, OccurrenceKey: occurrence.OccurrenceKey, ScheduledFor: occurrence.ScheduledFor, Schedule: captured}); err != nil {
		return err
	}
	if err = s.runWorkflowScheduleStepHook(ctx, WorkflowScheduleStageCommitBefore); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	if err = s.runWorkflowScheduleStepHook(ctx, WorkflowScheduleStageCommitAfter); err != nil {
		return err
	}
	_, err = admissions.Admit(ctx, listed.WorkspaceID, intent.RequestID, s.associateWorkflowAdmission)
	return err
}

func (s *Server) associateScheduledAdmission(ctx context.Context, tx pgx.Tx, record workflowadmission.Record) error {
	if s.AgentSchedules == nil || record.Receipt == nil {
		return ErrWorkflowScheduleAdmissionUnavailable
	}
	var captured schedule.Schedule
	if err := json.Unmarshal(record.Target.Schedule, &captured); err != nil {
		return err
	}
	occurrence := schedule.Occurrence{WorkspaceID: record.Subject.WorkspaceID, OccurrenceKey: record.Target.OccurrenceKey, ScheduleID: record.Target.ScheduleID,
		TargetWorkflowID: record.Receipt.Revision.WorkflowID, ScheduledFor: record.Target.ScheduledFor, Status: schedule.OccurrenceCommitted,
		WorkflowVersion: record.Receipt.Revision.WorkflowVersion, RunSnapshotID: record.Receipt.RunSnapshotID, TaskID: record.Receipt.TaskID}
	return s.AgentSchedules.CommitAcceptedOccurrenceTx(ctx, tx, occurrence, captured)
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
