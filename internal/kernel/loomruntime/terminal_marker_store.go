package loomruntime

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type TerminalMarkerPhase string
type TerminalMarkerStatus string
type TerminalMarkerSource string
type TerminalMarkerEvidenceKind string
type TerminalMarkerAuditState string
type TerminalMarkerLineageState string

const (
	TerminalMarkerPhaseYielded         TerminalMarkerPhase        = "yielded"
	TerminalMarkerPhaseFinal           TerminalMarkerPhase        = "final"
	TerminalMarkerStatusYielded        TerminalMarkerStatus       = "yielded"
	TerminalMarkerStatusSuccess        TerminalMarkerStatus       = "success"
	TerminalMarkerStatusFailed         TerminalMarkerStatus       = "failed"
	TerminalMarkerSourceNormal         TerminalMarkerSource       = "normal"
	TerminalMarkerSourceReconciler     TerminalMarkerSource       = "reconciler"
	TerminalMarkerEvidenceRunResult    TerminalMarkerEvidenceKind = "run_result"
	TerminalMarkerEvidenceCheckpoint   TerminalMarkerEvidenceKind = "checkpoint"
	TerminalMarkerEvidenceRegistryOnly TerminalMarkerEvidenceKind = "registry_only"
	TerminalMarkerAuditMaterialized    TerminalMarkerAuditState   = "materialized"
	TerminalMarkerAuditBlocked         TerminalMarkerAuditState   = "blocked"
	TerminalMarkerLineagePending       TerminalMarkerLineageState = "pending"
	TerminalMarkerLineageComplete      TerminalMarkerLineageState = "complete"
	TerminalMarkerLineageFailed        TerminalMarkerLineageState = "failed"
)

type TerminalMarkerV1 struct {
	WorkspaceID            string
	RunID                  string
	SchemaVersion          int16
	AttemptGeneration      int64
	AttemptID              uuid.UUID
	Agent                  string
	AttributionScope       TerminalAttributionScope
	TeamID                 *string
	WorkflowID             *string
	WorkflowVersion        *int32
	RunSnapshotID          *string
	ConversationID         *string
	ParentRunID            *string
	ParentSeq              *int64
	AggregationParentRunID *string
	TaskGroupID            *string
	RunStartedAt           string
	Phase                  TerminalMarkerPhase
	Status                 TerminalMarkerStatus
	StopReason             string
	Source                 TerminalMarkerSource
	TerminalAt             time.Time
	EvidenceKind           TerminalMarkerEvidenceKind
	CheckpointGraph        *string
	CheckpointSeq          *int64
	CheckpointSavedAt      *time.Time
	UsageInputTokens       int64
	UsageOutputTokens      int64
	UsageCostUSD           float64
	UsageToolCalls         int64
	AuditState             TerminalMarkerAuditState
	AuditSchemaVersion     *int16
	LineageState           TerminalMarkerLineageState
	LastErrorCode          *string
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

var ErrIllegalTerminalMarkerTransition = errors.New("illegal terminal marker transition")

type terminalMarkerDecodeError struct{ cause error }

func (err *terminalMarkerDecodeError) Error() string {
	return "decode terminal marker: " + err.cause.Error()
}

func (err *terminalMarkerDecodeError) Unwrap() error { return err.cause }

type TerminalMarkerTransitionError struct {
	CurrentClass        string
	CurrentGeneration   int64
	CandidateClass      string
	CandidateGeneration int64
	Reason              string
	Field               string
}

func (e *TerminalMarkerTransitionError) Error() string {
	return fmt.Sprintf("%v: current=%s(%d) candidate=%s(%d) reason=%s field=%s", ErrIllegalTerminalMarkerTransition, e.CurrentClass, e.CurrentGeneration, e.CandidateClass, e.CandidateGeneration, e.Reason, e.Field)
}
func (e *TerminalMarkerTransitionError) Unwrap() error { return ErrIllegalTerminalMarkerTransition }

func canonicalRunStartedAt(raw string) error {
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil || parsed.Format(time.RFC3339Nano) != raw {
		return fmt.Errorf("run_started_at must be canonical RFC3339Nano")
	}
	return nil
}

func enumValue[T ~string](field string, value T, allowed ...T) error {
	for _, candidate := range allowed {
		if value == candidate {
			return nil
		}
	}
	return fmt.Errorf("%s has unknown value %q", field, value)
}

func ValidateTerminalMarkerV1(m TerminalMarkerV1) error {
	for field, value := range map[string]string{"workspace_id": m.WorkspaceID, "run_id": m.RunID, "agent": m.Agent, "run_started_at": m.RunStartedAt, "stop_reason": m.StopReason} {
		if value == "" {
			return fmt.Errorf("%s must be non-empty", field)
		}
	}
	if m.SchemaVersion != 1 {
		return fmt.Errorf("schema_version must equal 1")
	}
	if m.AttemptGeneration < 1 {
		return fmt.Errorf("attempt_generation must be >= 1")
	}
	if m.AttemptID == uuid.Nil {
		return fmt.Errorf("attempt_id must be non-zero")
	}
	if err := enumValue("attribution_scope", m.AttributionScope, TerminalAttributionLegacyUnattributed, TerminalAttributionTeamFreeCollab, TerminalAttributionFixedWorkflow); err != nil {
		return err
	}
	if err := enumValue("phase", m.Phase, TerminalMarkerPhaseYielded, TerminalMarkerPhaseFinal); err != nil {
		return err
	}
	if err := enumValue("status", m.Status, TerminalMarkerStatusYielded, TerminalMarkerStatusSuccess, TerminalMarkerStatusFailed); err != nil {
		return err
	}
	if err := enumValue("source", m.Source, TerminalMarkerSourceNormal, TerminalMarkerSourceReconciler); err != nil {
		return err
	}
	if err := enumValue("evidence_kind", m.EvidenceKind, TerminalMarkerEvidenceRunResult, TerminalMarkerEvidenceCheckpoint, TerminalMarkerEvidenceRegistryOnly); err != nil {
		return err
	}
	if err := enumValue("audit_state", m.AuditState, TerminalMarkerAuditMaterialized, TerminalMarkerAuditBlocked); err != nil {
		return err
	}
	if err := enumValue("lineage_state", m.LineageState, TerminalMarkerLineagePending, TerminalMarkerLineageComplete, TerminalMarkerLineageFailed); err != nil {
		return err
	}
	if err := canonicalRunStartedAt(m.RunStartedAt); err != nil {
		return err
	}
	for field, value := range map[string]*string{"team_id": m.TeamID, "workflow_id": m.WorkflowID, "run_snapshot_id": m.RunSnapshotID, "conversation_id": m.ConversationID, "parent_run_id": m.ParentRunID, "aggregation_parent_run_id": m.AggregationParentRunID, "task_group_id": m.TaskGroupID, "checkpoint_graph": m.CheckpointGraph, "last_error_code": m.LastErrorCode} {
		if value != nil && *value == "" {
			return fmt.Errorf("%s must be non-empty when present", field)
		}
	}
	if (m.WorkflowID == nil) != (m.WorkflowVersion == nil) {
		return fmt.Errorf("workflow_pair requires id and version together")
	}
	if m.WorkflowVersion != nil && *m.WorkflowVersion < 1 {
		return fmt.Errorf("workflow_version must be >= 1")
	}
	switch m.AttributionScope {
	case TerminalAttributionFixedWorkflow:
		if m.TeamID == nil || m.WorkflowID == nil || m.WorkflowVersion == nil || m.RunSnapshotID == nil {
			return fmt.Errorf("scope_association fixed_workflow requires team, workflow, version, snapshot")
		}
	case TerminalAttributionTeamFreeCollab:
		if m.TeamID == nil || m.RunSnapshotID == nil || m.WorkflowID != nil || m.WorkflowVersion != nil {
			return fmt.Errorf("scope_association team_free_collab requires team/snapshot and no workflow")
		}
	}
	if (m.ParentRunID == nil) != (m.ParentSeq == nil) {
		return fmt.Errorf("parent_pair requires id and seq together")
	}
	if m.ParentSeq != nil && *m.ParentSeq < 0 {
		return fmt.Errorf("parent_seq must be non-negative")
	}
	if m.ParentRunID != nil && *m.ParentRunID == m.RunID {
		return fmt.Errorf("parent_run_id must differ from run_id")
	}
	if m.AggregationParentRunID != nil && (m.ParentRunID == nil || *m.AggregationParentRunID != *m.ParentRunID) {
		return fmt.Errorf("aggregation_parent must equal parent_run_id")
	}
	if (m.Phase == TerminalMarkerPhaseYielded) != (m.Status == TerminalMarkerStatusYielded) {
		return fmt.Errorf("phase_status combination invalid")
	}
	if m.Source == TerminalMarkerSourceReconciler && (m.Phase != TerminalMarkerPhaseFinal || m.Status != TerminalMarkerStatusFailed || m.StopReason != "interrupted") {
		return fmt.Errorf("reconciler_shape must be final/failed/interrupted")
	}
	if m.EvidenceKind == TerminalMarkerEvidenceCheckpoint {
		if m.CheckpointGraph == nil || m.CheckpointSeq == nil || *m.CheckpointSeq < 1 || m.CheckpointSavedAt == nil {
			return fmt.Errorf("checkpoint_evidence requires graph, positive seq, saved_at")
		}
	} else if m.CheckpointGraph != nil || m.CheckpointSeq != nil || m.CheckpointSavedAt != nil {
		return fmt.Errorf("checkpoint_evidence forbidden for non-checkpoint")
	}
	if m.EvidenceKind == TerminalMarkerEvidenceRegistryOnly && m.AuditState != TerminalMarkerAuditBlocked {
		return fmt.Errorf("registry_only requires blocked audit")
	}
	if m.UsageInputTokens < 0 {
		return fmt.Errorf("usage_input_tokens must be non-negative")
	}
	if m.UsageOutputTokens < 0 {
		return fmt.Errorf("usage_output_tokens must be non-negative")
	}
	if m.UsageCostUSD < 0 || math.IsNaN(m.UsageCostUSD) || math.IsInf(m.UsageCostUSD, 0) {
		return fmt.Errorf("usage_cost_usd must be finite and non-negative")
	}
	if m.UsageToolCalls < 0 {
		return fmt.Errorf("usage_tool_calls must be non-negative")
	}
	if m.AuditState == TerminalMarkerAuditMaterialized && (m.AuditSchemaVersion == nil || *m.AuditSchemaVersion != 3) {
		return fmt.Errorf("audit_schema materialized requires version 3")
	}
	if m.AuditState == TerminalMarkerAuditBlocked && m.AuditSchemaVersion != nil {
		return fmt.Errorf("audit_schema blocked requires nil version")
	}
	if (m.AuditState == TerminalMarkerAuditBlocked || m.LineageState == TerminalMarkerLineageFailed) && m.LastErrorCode == nil {
		return fmt.Errorf("error_state requires last_error_code")
	}
	if m.TerminalAt.IsZero() {
		return fmt.Errorf("terminal_at must be non-zero")
	}
	if m.CreatedAt.IsZero() {
		return fmt.Errorf("created_at must be non-zero")
	}
	if m.UpdatedAt.IsZero() {
		return fmt.Errorf("updated_at must be non-zero")
	}
	return nil
}

func markerClass(m TerminalMarkerV1) string {
	if m.Phase == TerminalMarkerPhaseYielded && m.Status == TerminalMarkerStatusYielded && m.Source == TerminalMarkerSourceNormal {
		return "yielded"
	}
	if m.Phase == TerminalMarkerPhaseFinal && m.Source == TerminalMarkerSourceNormal {
		return "normal_final"
	}
	if m.Phase == TerminalMarkerPhaseFinal && m.Status == TerminalMarkerStatusFailed && m.StopReason == "interrupted" && m.Source == TerminalMarkerSourceReconciler {
		return "reconciled_interrupted"
	}
	return "invalid"
}

func transitionError(current *TerminalMarkerV1, candidate TerminalMarkerV1, reason, field string) error {
	cc, cg := "absent", int64(0)
	if current != nil {
		cc, cg = markerClass(*current), current.AttemptGeneration
	}
	return &TerminalMarkerTransitionError{CurrentClass: cc, CurrentGeneration: cg, CandidateClass: markerClass(candidate), CandidateGeneration: candidate.AttemptGeneration, Reason: reason, Field: field}
}

func markerIdentityEqual(a, b TerminalMarkerV1) (bool, string) {
	checks := []struct {
		name string
		a, b any
	}{{"workspace_id", a.WorkspaceID, b.WorkspaceID}, {"run_id", a.RunID, b.RunID}, {"schema_version", a.SchemaVersion, b.SchemaVersion}, {"agent", a.Agent, b.Agent}, {"attribution_scope", a.AttributionScope, b.AttributionScope}, {"team_id", a.TeamID, b.TeamID}, {"workflow_id", a.WorkflowID, b.WorkflowID}, {"workflow_version", a.WorkflowVersion, b.WorkflowVersion}, {"run_snapshot_id", a.RunSnapshotID, b.RunSnapshotID}, {"conversation_id", a.ConversationID, b.ConversationID}, {"parent_run_id", a.ParentRunID, b.ParentRunID}, {"parent_seq", a.ParentSeq, b.ParentSeq}, {"aggregation_parent_run_id", a.AggregationParentRunID, b.AggregationParentRunID}, {"task_group_id", a.TaskGroupID, b.TaskGroupID}, {"run_started_at", a.RunStartedAt, b.RunStartedAt}}
	for _, c := range checks {
		if !reflect.DeepEqual(c.a, c.b) {
			return false, c.name
		}
	}
	return true, ""
}

func markerLineageBusinessEqual(
	current TerminalMarkerV1,
	candidate TerminalMarkerV1,
) bool {
	if math.Float64bits(current.UsageCostUSD) !=
		math.Float64bits(candidate.UsageCostUSD) {
		return false
	}
	normalized := candidate
	normalized.LineageState = current.LineageState
	normalized.LastErrorCode = current.LastErrorCode
	normalized.UpdatedAt = current.UpdatedAt
	return reflect.DeepEqual(current, normalized)
}

func markerLineagePersistedBusinessEqual(
	current TerminalMarkerV1,
	candidate TerminalMarkerV1,
) bool {
	candidate.CreatedAt = current.CreatedAt
	return markerLineageBusinessEqual(current, candidate)
}

func markerPersistedBusinessDiffersOnlyByCostBits(
	current TerminalMarkerV1,
	candidate TerminalMarkerV1,
) bool {
	if current.UsageCostUSD != candidate.UsageCostUSD ||
		math.Float64bits(current.UsageCostUSD) ==
			math.Float64bits(candidate.UsageCostUSD) {
		return false
	}
	candidate.UsageCostUSD = current.UsageCostUSD
	return markerLineagePersistedBusinessEqual(current, candidate)
}

func markerLineageOnlyTransition(
	current TerminalMarkerV1,
	candidate TerminalMarkerV1,
) bool {
	if current.AuditState == TerminalMarkerAuditBlocked ||
		candidate.AuditState == TerminalMarkerAuditBlocked ||
		!markerLineageBusinessEqual(current, candidate) {
		return false
	}

	switch current.LineageState {
	case TerminalMarkerLineagePending:
		if candidate.LineageState != TerminalMarkerLineageComplete &&
			candidate.LineageState != TerminalMarkerLineageFailed {
			return false
		}
	case TerminalMarkerLineageFailed:
		if candidate.LineageState != TerminalMarkerLineageComplete &&
			(candidate.LineageState != TerminalMarkerLineageFailed ||
				reflect.DeepEqual(
					current.LastErrorCode,
					candidate.LastErrorCode,
				)) {
			return false
		}
	default:
		return false
	}

	if candidate.LineageState == TerminalMarkerLineageComplete {
		return candidate.LastErrorCode == nil
	}
	return terminalLineageFailureCodeStable(candidate.LastErrorCode)
}

const (
	recoveryAuditCorrupt  = "recovery_audit_corrupt"
	recoveryAuditConflict = "recovery_audit_conflict"
)

func isPermanentRecoveryDefectCode(code string) bool {
	switch code {
	case recoveryCheckpointCorrupt,
		recoveryCheckpointIdentityMismatch,
		recoveryCheckpointAttributionMismatch,
		recoveryCheckpointUsageInvalid,
		recoveryAuditCorrupt,
		recoveryAuditConflict:
		return true
	default:
		return false
	}
}

func blockedRecoveryMarkerCanAdvance(
	current TerminalMarkerV1,
	candidate TerminalMarkerV1,
) bool {
	if current.Source != TerminalMarkerSourceReconciler ||
		current.EvidenceKind != TerminalMarkerEvidenceRegistryOnly ||
		current.AuditState != TerminalMarkerAuditBlocked ||
		current.LastErrorCode == nil ||
		*current.LastErrorCode != recoveryCheckpointMissing ||
		candidate.Source != TerminalMarkerSourceReconciler ||
		candidate.AttemptGeneration != current.AttemptGeneration ||
		candidate.AttemptID != current.AttemptID ||
		candidate.LineageState != current.LineageState ||
		!candidate.TerminalAt.Equal(current.TerminalAt) {
		return false
	}
	if candidate.AuditState == TerminalMarkerAuditMaterialized {
		return candidate.EvidenceKind == TerminalMarkerEvidenceCheckpoint &&
			candidate.LastErrorCode == nil
	}
	return candidate.EvidenceKind == TerminalMarkerEvidenceRegistryOnly &&
		candidate.AuditState == TerminalMarkerAuditBlocked &&
		candidate.LastErrorCode != nil &&
		(*candidate.LastErrorCode == recoveryCheckpointMissing ||
			isPermanentRecoveryDefectCode(*candidate.LastErrorCode))
}

func ValidateTerminalMarkerTransition(current *TerminalMarkerV1, candidate TerminalMarkerV1) error {
	if err := ValidateTerminalMarkerV1(candidate); err != nil {
		return transitionError(current, candidate, "invalid_candidate", "")
	}
	cc := "absent"
	if current != nil {
		if err := ValidateTerminalMarkerV1(*current); err != nil {
			return transitionError(current, candidate, "invalid_current", "")
		}
		cc = markerClass(*current)
	}
	nc := markerClass(candidate)
	if nc == "invalid" {
		return transitionError(current, candidate, "invalid_candidate", "")
	}
	if current == nil {
		return nil
	}
	if ok, field := markerIdentityEqual(*current, candidate); !ok {
		return transitionError(current, candidate, "identity_conflict", field)
	}
	if candidate.UsageInputTokens < current.UsageInputTokens || candidate.UsageOutputTokens < current.UsageOutputTokens || candidate.UsageCostUSD < current.UsageCostUSD || candidate.UsageToolCalls < current.UsageToolCalls {
		return transitionError(current, candidate, "usage_regression", "usage")
	}
	if reflect.DeepEqual(*current, candidate) {
		return nil
	}
	businessEqual := markerLineageBusinessEqual(*current, candidate)
	persistedBusinessEqual := markerLineagePersistedBusinessEqual(
		*current,
		candidate,
	)
	if current.AuditState != TerminalMarkerAuditBlocked &&
		markerPersistedBusinessDiffersOnlyByCostBits(
			*current,
			candidate,
		) {
		return transitionError(
			current,
			candidate,
			"transition_forbidden",
			"usage_cost_usd",
		)
	}
	if current.AuditState != TerminalMarkerAuditBlocked &&
		persistedBusinessEqual {
		if !businessEqual {
			return transitionError(
				current,
				candidate,
				"transition_forbidden",
				"created_at",
			)
		}
		if markerLineageOnlyTransition(*current, candidate) {
			return nil
		}
		return transitionError(
			current,
			candidate,
			"transition_forbidden",
			"lineage_state",
		)
	}
	if current.AuditState != TerminalMarkerAuditBlocked &&
		!persistedBusinessEqual &&
		candidate.LineageState != TerminalMarkerLineagePending {
		return transitionError(
			current,
			candidate,
			"transition_forbidden",
			"lineage_state",
		)
	}
	switch cc {
	case "yielded":
		if candidate.AttemptGeneration != current.AttemptGeneration+1 {
			return transitionError(current, candidate, "generation_mismatch", "attempt_generation")
		}
		if candidate.AttemptID == current.AttemptID {
			return transitionError(current, candidate, "attempt_mismatch", "attempt_id")
		}
		if nc == "yielded" || nc == "normal_final" || nc == "reconciled_interrupted" {
			return nil
		}
	case "reconciled_interrupted":
		if nc == "reconciled_interrupted" {
			if blockedRecoveryMarkerCanAdvance(*current, candidate) {
				return nil
			}
			return transitionError(current, candidate, "transition_forbidden", "")
		}
		if candidate.AttemptGeneration != current.AttemptGeneration {
			return transitionError(current, candidate, "generation_mismatch", "attempt_generation")
		}
		if candidate.AttemptID != current.AttemptID {
			return transitionError(current, candidate, "attempt_mismatch", "attempt_id")
		}
		if nc == "yielded" || nc == "normal_final" {
			return nil
		}
	case "normal_final":
		if nc != "normal_final" {
			return transitionError(current, candidate, "transition_forbidden", "")
		}
		if candidate.AttemptGeneration != current.AttemptGeneration {
			return transitionError(current, candidate, "generation_mismatch", "attempt_generation")
		}
		if candidate.AttemptID != current.AttemptID {
			return transitionError(current, candidate, "attempt_mismatch", "attempt_id")
		}
		return nil
	default:
		return transitionError(current, candidate, "invalid_current", "")
	}
	return transitionError(current, candidate, "transition_forbidden", "")
}

const terminalMarkerColumns = "workspace_id,run_id,schema_version,attempt_generation,attempt_id,agent,attribution_scope,team_id,workflow_id,workflow_version,run_snapshot_id,conversation_id,parent_run_id,parent_seq,aggregation_parent_run_id,task_group_id,run_started_at,phase,status,stop_reason,source,terminal_at,evidence_kind,checkpoint_graph,checkpoint_seq,checkpoint_saved_at,usage_input_tokens,usage_output_tokens,usage_cost_usd,usage_tool_calls,audit_state,audit_schema_version,lineage_state,last_error_code,created_at,updated_at"

type rowScanner interface{ Scan(...any) error }

func scanTerminalMarker(row rowScanner) (TerminalMarkerV1, error) {
	var m TerminalMarkerV1
	var scope, phase, status, source, evidence, audit, lineage string
	err := row.Scan(&m.WorkspaceID, &m.RunID, &m.SchemaVersion, &m.AttemptGeneration, &m.AttemptID, &m.Agent, &scope, &m.TeamID, &m.WorkflowID, &m.WorkflowVersion, &m.RunSnapshotID, &m.ConversationID, &m.ParentRunID, &m.ParentSeq, &m.AggregationParentRunID, &m.TaskGroupID, &m.RunStartedAt, &phase, &status, &m.StopReason, &source, &m.TerminalAt, &evidence, &m.CheckpointGraph, &m.CheckpointSeq, &m.CheckpointSavedAt, &m.UsageInputTokens, &m.UsageOutputTokens, &m.UsageCostUSD, &m.UsageToolCalls, &audit, &m.AuditSchemaVersion, &lineage, &m.LastErrorCode, &m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		return TerminalMarkerV1{}, err
	}
	m.AttributionScope = TerminalAttributionScope(scope)
	m.Phase = TerminalMarkerPhase(phase)
	m.Status = TerminalMarkerStatus(status)
	m.Source = TerminalMarkerSource(source)
	m.EvidenceKind = TerminalMarkerEvidenceKind(evidence)
	m.AuditState = TerminalMarkerAuditState(audit)
	m.LineageState = TerminalMarkerLineageState(lineage)
	if err := ValidateTerminalMarkerV1(m); err != nil {
		return TerminalMarkerV1{}, &terminalMarkerDecodeError{cause: err}
	}
	return m, nil
}

type PGTerminalStateStore struct{}

func NewPGTerminalStateStore() *PGTerminalStateStore { return &PGTerminalStateStore{} }
func (*PGTerminalStateStore) LockTerminalRun(ctx context.Context, tx pgx.Tx, workspaceID, runID string) error {
	if tx == nil {
		return fmt.Errorf("tx must be non-nil")
	}
	if workspaceID == "" || runID == "" {
		return fmt.Errorf("physical key must be non-empty")
	}
	_, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0) # hashtextextended($2, 0))", workspaceID, runID)
	if err != nil {
		return fmt.Errorf("lock terminal run: %w", err)
	}
	return nil
}
func (*PGTerminalStateStore) ReadTerminalMarkerForUpdate(ctx context.Context, tx pgx.Tx, workspaceID, runID string) (TerminalMarkerV1, bool, error) {
	if tx == nil {
		return TerminalMarkerV1{}, false, fmt.Errorf("tx must be non-nil")
	}
	if workspaceID == "" || runID == "" {
		return TerminalMarkerV1{}, false, fmt.Errorf("physical key must be non-empty")
	}
	m, err := scanTerminalMarker(tx.QueryRow(ctx, "SELECT "+terminalMarkerColumns+" FROM weave_run_terminal_markers WHERE workspace_id=$1 AND run_id=$2 FOR UPDATE", workspaceID, runID))
	if errors.Is(err, pgx.ErrNoRows) {
		return TerminalMarkerV1{}, false, nil
	}
	if err != nil {
		return TerminalMarkerV1{}, false, fmt.Errorf("read weave_run_terminal_markers: %w", err)
	}
	return m, true, nil
}

func markerArgs(m TerminalMarkerV1) []any {
	return []any{m.WorkspaceID, m.RunID, m.SchemaVersion, m.AttemptGeneration, m.AttemptID, m.Agent, string(m.AttributionScope), m.TeamID, m.WorkflowID, m.WorkflowVersion, m.RunSnapshotID, m.ConversationID, m.ParentRunID, m.ParentSeq, m.AggregationParentRunID, m.TaskGroupID, m.RunStartedAt, string(m.Phase), string(m.Status), m.StopReason, string(m.Source), m.TerminalAt, string(m.EvidenceKind), m.CheckpointGraph, m.CheckpointSeq, m.CheckpointSavedAt, m.UsageInputTokens, m.UsageOutputTokens, m.UsageCostUSD, m.UsageToolCalls, string(m.AuditState), m.AuditSchemaVersion, string(m.LineageState), m.LastErrorCode}
}
func (*PGTerminalStateStore) ApplyTerminalMarkerTransition(ctx context.Context, tx pgx.Tx, c TerminalMarkerV1) (TerminalMarkerV1, error) {
	if tx == nil {
		return TerminalMarkerV1{}, fmt.Errorf("tx must be non-nil")
	}
	s := NewPGTerminalStateStore()
	cur, present, err := s.ReadTerminalMarkerForUpdate(ctx, tx, c.WorkspaceID, c.RunID)
	if err != nil {
		return TerminalMarkerV1{}, err
	}
	var cp *TerminalMarkerV1
	if present {
		cp = &cur
	}
	if err := ValidateTerminalMarkerTransition(cp, c); err != nil {
		return TerminalMarkerV1{}, err
	}
	if present && reflect.DeepEqual(cur, c) {
		return cur, nil
	}
	args := markerArgs(c)
	var q string
	if !present {
		q = "INSERT INTO weave_run_terminal_markers (" + strings.TrimSuffix(terminalMarkerColumns, ",created_at,updated_at") + ",created_at,updated_at) VALUES ("
		for i := 1; i <= 34; i++ {
			if i > 1 {
				q += ","
			}
			q += fmt.Sprintf("$%d", i)
		}
		q += ",statement_timestamp(),statement_timestamp()) RETURNING " + terminalMarkerColumns
	} else {
		q = "UPDATE weave_run_terminal_markers SET schema_version=$3,attempt_generation=$4,attempt_id=$5,agent=$6,attribution_scope=$7,team_id=$8,workflow_id=$9,workflow_version=$10,run_snapshot_id=$11,conversation_id=$12,parent_run_id=$13,parent_seq=$14,aggregation_parent_run_id=$15,task_group_id=$16,run_started_at=$17,phase=$18,status=$19,stop_reason=$20,source=$21,terminal_at=$22,evidence_kind=$23,checkpoint_graph=$24,checkpoint_seq=$25,checkpoint_saved_at=$26,usage_input_tokens=$27,usage_output_tokens=$28,usage_cost_usd=$29,usage_tool_calls=$30,audit_state=$31,audit_schema_version=$32,lineage_state=$33,last_error_code=$34,updated_at=statement_timestamp() WHERE workspace_id=$1 AND run_id=$2 RETURNING " + terminalMarkerColumns
	}
	out, err := scanTerminalMarker(tx.QueryRow(ctx, q, args...))
	if err != nil {
		return TerminalMarkerV1{}, fmt.Errorf("apply terminal marker transition: %w", err)
	}
	if out.ConversationID != nil {
		if _, err := tx.Exec(ctx, `
			UPDATE weave_projects AS project
			SET last_activity_at=GREATEST(
				COALESCE(project.last_activity_at, project.updated_at),
				$3
			)
			FROM weave_conversations AS conversation
			WHERE conversation.workspace_id=$1 AND conversation.id=$2
			  AND project.workspace_id=conversation.workspace_id
			  AND project.id=conversation.project_id
		`, out.WorkspaceID, *out.ConversationID, out.TerminalAt); err != nil {
			return TerminalMarkerV1{}, fmt.Errorf("update project terminal activity: %w", err)
		}
	}
	return out, nil
}
