package loomruntime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/jinyitao123/loom"
)

const (
	recoveryCheckpointMissing             = "recovery_checkpoint_missing"
	recoveryCheckpointCorrupt             = "recovery_checkpoint_corrupt"
	recoveryCheckpointIdentityMismatch    = "recovery_checkpoint_identity_mismatch"
	recoveryCheckpointAttributionMismatch = "recovery_checkpoint_attribution_mismatch"
	recoveryCheckpointUsageInvalid        = "recovery_checkpoint_usage_invalid"
)

// RecoveryTerminalAssemblyInput contains deterministic recovery evidence.
type RecoveryTerminalAssemblyInput struct {
	Registry   ExpectedRunRecordV1
	Lease      RunAttemptLease
	Checkpoint []byte
	RecordedAt time.Time
}

// RecoveryTerminalAssembly contains one reconciler marker and its optional audit.
type RecoveryTerminalAssembly struct {
	Marker    TerminalMarkerV1
	Candidate *TerminalEntryV3
}

type recoveryCheckpointEnvelope struct {
	Schema     int            `json:"schema_version"`
	RunID      string         `json:"run_id"`
	Graph      string         `json:"graph"`
	Seq        int64          `json:"seq"`
	ParentRun  string         `json:"parent_run,omitempty"`
	ParentSeq  int64          `json:"parent_seq,omitempty"`
	LastStep   string         `json:"last_step"`
	State      loom.State     `json:"state"`
	YieldPhase string         `json:"yield_phase"`
	SavedAt    time.Time      `json:"saved_at"`
	Meta       map[string]any `json:"meta,omitempty"`
}

// AssembleRecoveryTerminalV3 assembles one recovery terminal without I/O.
func AssembleRecoveryTerminalV3(
	input RecoveryTerminalAssemblyInput,
) (RecoveryTerminalAssembly, error) {
	startedAt, err := validateRecoveryAuthority(input)
	if err != nil {
		return RecoveryTerminalAssembly{}, err
	}
	if input.Checkpoint == nil {
		return blockedRecoveryAssembly(input, recoveryCheckpointMissing)
	}
	checkpoint, err := decodeRecoveryCheckpoint(input.Checkpoint)
	if err != nil {
		return blockedRecoveryAssembly(input, recoveryCheckpointCorrupt)
	}
	if err := validateRecoveryCheckpointIdentity(
		checkpoint,
		input.Registry,
		input.Lease,
	); err != nil {
		return blockedRecoveryAssembly(input, recoveryCheckpointIdentityMismatch)
	}
	attribution, err := recoveryCheckpointAttribution(
		checkpoint.State,
		input.Registry,
	)
	if err != nil {
		return blockedRecoveryAssembly(input, recoveryCheckpointAttributionMismatch)
	}
	exclusive, err := validatedTerminalExclusiveUsage(checkpoint.State)
	if err != nil {
		return blockedRecoveryAssembly(input, recoveryCheckpointUsageInvalid)
	}
	candidate, err := assembleTerminalV3Base(terminalV3BaseAssemblyInput{
		Tenant:      input.Registry.WorkspaceID,
		Agent:       input.Registry.Agent,
		RunID:       input.Registry.RunID,
		StartedAt:   startedAt,
		EndedAt:     input.Lease.LeaseExpiresAt,
		Status:      "failed",
		StopReason:  "interrupted",
		Step:        checkpoint.LastStep,
		Summary:     truncateRunes(stateString(checkpoint.State, "last_user_message"), 120),
		Attribution: attribution,
		Exclusive:   exclusive,
	})
	if err != nil {
		return RecoveryTerminalAssembly{}, fmt.Errorf(
			"assemble recovery terminal candidate: %w",
			err,
		)
	}
	marker, err := materializedRecoveryMarker(
		input,
		checkpoint,
		candidate.SelfExclusive,
	)
	if err != nil {
		return RecoveryTerminalAssembly{}, err
	}
	return RecoveryTerminalAssembly{
		Marker:    marker,
		Candidate: &candidate,
	}, nil
}

func validateRecoveryAuthority(
	input RecoveryTerminalAssemblyInput,
) (time.Time, error) {
	if err := validateExpectedRunRecordV1(input.Registry); err != nil {
		return time.Time{}, fmt.Errorf(
			"assemble recovery terminal: invalid registry: %w",
			err,
		)
	}
	if err := ValidateRunAttemptLease(input.Lease); err != nil {
		return time.Time{}, fmt.Errorf(
			"assemble recovery terminal: invalid lease: %w",
			err,
		)
	}
	if input.Lease.State != AttemptLeaseReconciling {
		return time.Time{}, fmt.Errorf(
			"assemble recovery terminal: lease state must be reconciling",
		)
	}
	if input.Registry.WorkspaceID != input.Lease.WorkspaceID ||
		input.Registry.RunID != input.Lease.RunID {
		return time.Time{}, fmt.Errorf(
			"assemble recovery terminal: registry and lease identity mismatch",
		)
	}
	if input.RecordedAt.IsZero() || input.RecordedAt.Location() != time.UTC {
		return time.Time{}, fmt.Errorf(
			"assemble recovery terminal: recorded_at must be non-zero UTC",
		)
	}
	if input.Registry.WorkflowVersion != nil &&
		*input.Registry.WorkflowVersion > math.MaxInt32 {
		return time.Time{}, fmt.Errorf(
			"assemble recovery terminal: workflow_version exceeds marker capacity",
		)
	}
	startedAt, err := time.Parse(time.RFC3339Nano, input.Lease.RunStartedAt)
	if err != nil {
		return time.Time{}, fmt.Errorf(
			"assemble recovery terminal: parse run start: %w",
			err,
		)
	}
	if input.Lease.LeaseExpiresAt.Before(startedAt) {
		return time.Time{}, fmt.Errorf(
			"assemble recovery terminal: lease expiry precedes run start",
		)
	}
	return startedAt, nil
}

func decodeRecoveryCheckpoint(
	raw []byte,
) (recoveryCheckpointEnvelope, error) {
	var checkpoint recoveryCheckpointEnvelope
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&checkpoint); err != nil {
		return recoveryCheckpointEnvelope{}, fmt.Errorf(
			"assemble recovery terminal: decode checkpoint: %w",
			err,
		)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return recoveryCheckpointEnvelope{}, fmt.Errorf(
			"assemble recovery terminal: decode checkpoint: trailing JSON content",
		)
	}
	if checkpoint.Schema < 0 ||
		checkpoint.Schema > loom.CurrentCheckpointSchema {
		return recoveryCheckpointEnvelope{}, fmt.Errorf(
			"assemble recovery terminal: unsupported checkpoint schema %d",
			checkpoint.Schema,
		)
	}
	if checkpoint.Seq < 1 {
		return recoveryCheckpointEnvelope{}, fmt.Errorf(
			"assemble recovery terminal: checkpoint seq must be positive",
		)
	}
	if checkpoint.SavedAt.IsZero() {
		return recoveryCheckpointEnvelope{}, fmt.Errorf(
			"assemble recovery terminal: checkpoint saved_at is required",
		)
	}
	if checkpoint.State == nil {
		return recoveryCheckpointEnvelope{}, fmt.Errorf(
			"assemble recovery terminal: checkpoint State is required",
		)
	}
	return checkpoint, nil
}

func validateRecoveryCheckpointIdentity(
	checkpoint recoveryCheckpointEnvelope,
	registry ExpectedRunRecordV1,
	lease RunAttemptLease,
) error {
	if checkpoint.RunID != registry.RunID ||
		checkpoint.RunID != lease.RunID {
		return fmt.Errorf(
			"assemble recovery terminal: checkpoint run identity mismatch",
		)
	}
	if checkpoint.Graph != lease.GraphName {
		return fmt.Errorf(
			"assemble recovery terminal: checkpoint graph identity mismatch",
		)
	}
	stateRunID, _ := checkpoint.State["__run_id"].(string)
	stateTenant, _ := checkpoint.State["tenant"].(string)
	stateAgent, _ := checkpoint.State["agent_name"].(string)
	if stateRunID != registry.RunID ||
		stateTenant != registry.WorkspaceID ||
		stateAgent != registry.Agent {
		return fmt.Errorf(
			"assemble recovery terminal: checkpoint State identity mismatch",
		)
	}
	stateSeq, ok := checkpointInt64(checkpoint.State["__seq"])
	if !ok || stateSeq != checkpoint.Seq {
		return fmt.Errorf(
			"assemble recovery terminal: checkpoint State seq mismatch",
		)
	}
	stateStartedAt, ok := checkpoint.State["__run_started_at"].(string)
	if !ok || stateStartedAt == "" {
		return fmt.Errorf(
			"assemble recovery terminal: checkpoint State run start is required",
		)
	}
	if err := canonicalRunStartedAt(stateStartedAt); err != nil {
		return fmt.Errorf(
			"assemble recovery terminal: checkpoint State run start: %w",
			err,
		)
	}
	if stateStartedAt != lease.RunStartedAt {
		return fmt.Errorf(
			"assemble recovery terminal: checkpoint State run start mismatch",
		)
	}
	return nil
}

func recoveryCheckpointAttribution(
	state loom.State,
	registry ExpectedRunRecordV1,
) (TerminalAttribution, error) {
	executionStamp, err := agentExecutionStampFromCheckpoint(
		state,
		registry.RunID,
	)
	if err != nil {
		return TerminalAttribution{}, fmt.Errorf(
			"assemble recovery terminal: checkpoint execution stamp: %w",
			err,
		)
	}
	if executionStamp == nil {
		return TerminalAttribution{}, fmt.Errorf(
			"assemble recovery terminal: checkpoint execution stamp is required",
		)
	}
	raw, exists := state[terminalAttributionStateKey]
	if !exists {
		return TerminalAttribution{}, fmt.Errorf(
			"assemble recovery terminal: checkpoint attribution stamp is required",
		)
	}
	attribution, err := decodeTerminalAttributionCheckpointStamp(raw)
	if err != nil {
		return TerminalAttribution{}, fmt.Errorf(
			"assemble recovery terminal: checkpoint attribution stamp: %w",
			err,
		)
	}
	if err := validateTerminalAttributionForState(
		attribution,
		registry.WorkspaceID,
		registry.RunID,
		state,
	); err != nil {
		return TerminalAttribution{}, fmt.Errorf(
			"assemble recovery terminal: checkpoint attribution State: %w",
			err,
		)
	}
	checkpointRecord := expectedRunRecordFromAttribution(
		registry.RunID,
		registry.Agent,
		attribution,
		registry.RegisteredAt,
	)
	if err := compareExpectedRunIdentity(
		checkpointRecord,
		registry,
	); err != nil {
		return TerminalAttribution{}, fmt.Errorf(
			"assemble recovery terminal: checkpoint attribution identity: %w",
			err,
		)
	}
	return attribution, nil
}

func blockedRecoveryAssembly(
	input RecoveryTerminalAssemblyInput,
	code string,
) (RecoveryTerminalAssembly, error) {
	errorCode := code
	marker := recoveryMarkerBase(input)
	marker.EvidenceKind = TerminalMarkerEvidenceRegistryOnly
	marker.AuditState = TerminalMarkerAuditBlocked
	marker.LastErrorCode = &errorCode
	if err := ValidateTerminalMarkerV1(marker); err != nil {
		return RecoveryTerminalAssembly{}, fmt.Errorf(
			"assemble blocked recovery terminal marker: %w",
			err,
		)
	}
	return RecoveryTerminalAssembly{Marker: marker}, nil
}

func materializedRecoveryMarker(
	input RecoveryTerminalAssemblyInput,
	checkpoint recoveryCheckpointEnvelope,
	usage TerminalUsage,
) (TerminalMarkerV1, error) {
	auditSchemaVersion := int16(terminalSchemaVersionV3)
	checkpointGraph := checkpoint.Graph
	checkpointSeq := checkpoint.Seq
	checkpointSavedAt := checkpoint.SavedAt
	marker := recoveryMarkerBase(input)
	marker.EvidenceKind = TerminalMarkerEvidenceCheckpoint
	marker.CheckpointGraph = &checkpointGraph
	marker.CheckpointSeq = &checkpointSeq
	marker.CheckpointSavedAt = &checkpointSavedAt
	marker.UsageInputTokens = int64(usage.InputTokens)
	marker.UsageOutputTokens = int64(usage.OutputTokens)
	marker.UsageCostUSD = usage.CostUSD
	marker.AuditState = TerminalMarkerAuditMaterialized
	marker.AuditSchemaVersion = &auditSchemaVersion
	if err := ValidateTerminalMarkerV1(marker); err != nil {
		return TerminalMarkerV1{}, fmt.Errorf(
			"assemble recovery terminal marker: %w",
			err,
		)
	}
	return marker, nil
}

func recoveryMarkerBase(
	input RecoveryTerminalAssemblyInput,
) TerminalMarkerV1 {
	return TerminalMarkerV1{
		WorkspaceID:            input.Registry.WorkspaceID,
		RunID:                  input.Registry.RunID,
		SchemaVersion:          1,
		AttemptGeneration:      input.Lease.AttemptGeneration,
		AttemptID:              input.Lease.AttemptID,
		Agent:                  input.Registry.Agent,
		AttributionScope:       input.Registry.AttributionScope,
		TeamID:                 recoveryStringPtr(input.Registry.TeamID),
		WorkflowID:             recoveryStringPtr(input.Registry.WorkflowID),
		WorkflowVersion:        recoveryInt32Ptr(input.Registry.WorkflowVersion),
		RunSnapshotID:          recoveryStringPtr(input.Registry.RunSnapshotID),
		ParentRunID:            recoveryStringPtr(input.Registry.ParentRunID),
		ParentSeq:              recoveryInt64Ptr(input.Registry.ParentSeq),
		AggregationParentRunID: recoveryStringPtr(input.Registry.AggregationParentRunID),
		TaskGroupID:            recoveryStringPtr(input.Registry.TaskGroupID),
		RunStartedAt:           input.Lease.RunStartedAt,
		Phase:                  TerminalMarkerPhaseFinal,
		Status:                 TerminalMarkerStatusFailed,
		StopReason:             "interrupted",
		Source:                 TerminalMarkerSourceReconciler,
		TerminalAt:             input.Lease.LeaseExpiresAt,
		LineageState:           TerminalMarkerLineagePending,
		CreatedAt:              input.RecordedAt,
		UpdatedAt:              input.RecordedAt,
	}
}

func recoveryStringPtr(value *string) *string {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func recoveryInt64Ptr(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func recoveryInt32Ptr(value *int) *int32 {
	if value == nil {
		return nil
	}
	copied := int32(*value)
	return &copied
}
