package workflowcatalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	workflowdef "github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

var workflowScheduleOccurrenceKeyPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// PrepareWorkflowScheduleTx makes a read-only admission decision from exact
// publication facts while leaving transaction ownership with the caller.
func (s *Store) PrepareWorkflowScheduleTx(
	ctx context.Context,
	tx pgx.Tx,
	request workflowdef.WorkflowScheduleAdmissionRequest, envelope frozen.ArtifactEnvelopeV1) (snapshot.TeamRunSnapshot, error) {
	if err := validateWorkflowScheduleAdmissionRequest(tx, request); err != nil {
		return snapshot.TeamRunSnapshot{}, err
	}

	var workspaceID string
	if err := tx.QueryRow(ctx, `
		SELECT id
		FROM weave_workspaces
		WHERE id=$1
		FOR SHARE
	`, request.WorkspaceID).Scan(&workspaceID); err != nil {
		return snapshot.TeamRunSnapshot{}, workflowScheduleAdmissionReadError(
			err, "workspace is unavailable",
		)
	}

	var (
		workflowStatus   string
		teamID           string
		publishedVersion *int
	)
	if err := tx.QueryRow(ctx, `
		SELECT status, team_id, published_version
		FROM weave_team_workflows
		WHERE workspace_id=$1 AND id=$2
		FOR SHARE
	`, request.WorkspaceID, request.WorkflowID).Scan(
		&workflowStatus, &teamID, &publishedVersion,
	); err != nil {
		return snapshot.TeamRunSnapshot{}, workflowScheduleAdmissionReadError(
			err, "workflow is unavailable",
		)
	}
	if workflowStatus != workflowdef.WorkflowStatusActive || publishedVersion == nil {
		return snapshot.TeamRunSnapshot{}, workflowScheduleAdmissionDenied(
			"workflow is not an active publication",
		)
	}

	var versionStatus string
	if err := tx.QueryRow(ctx, `
		SELECT status
		FROM weave_team_workflow_versions
		WHERE workspace_id=$1 AND workflow_id=$2 AND version=$3
		FOR SHARE
	`, request.WorkspaceID, request.WorkflowID, *publishedVersion).Scan(&versionStatus); err != nil {
		return snapshot.TeamRunSnapshot{}, workflowScheduleAdmissionReadError(
			err, "published workflow version is unavailable",
		)
	}
	if versionStatus != workflowdef.VersionStatusPublished {
		return snapshot.TeamRunSnapshot{}, workflowScheduleAdmissionDenied(
			"exact workflow version is not published",
		)
	}

	if envelope.WorkspaceID != request.WorkspaceID || envelope.WorkflowID != request.WorkflowID || envelope.WorkflowVersion != *publishedVersion {
		return snapshot.TeamRunSnapshot{}, workflowdef.ErrVersionConflict
	}
	payload, err := frozen.DecodeArtifactEnvelopeV1(envelope)
	if err != nil {
		return snapshot.TeamRunSnapshot{}, workflowScheduleAdmissionDenied(
			"published Artifact is invalid",
		)
	}
	trigger, triggerReport := machine.DecodeTriggerConfigV1(payload.TriggerConfig)
	if triggerReport != nil && len(triggerReport.Issues) != 0 {
		return snapshot.TeamRunSnapshot{}, workflowScheduleAdmissionDenied(
			"published trigger is invalid",
		)
	}
	scheduleConfig, scheduleTrigger := trigger.Config.(machine.ScheduleConfig)
	if trigger.Type != machine.TriggerSchedule || !scheduleTrigger ||
		scheduleConfig.ScheduleID != request.ScheduleID {
		return snapshot.TeamRunSnapshot{}, workflowScheduleAdmissionDenied(
			"published trigger does not match schedule",
		)
	}
	_, graphReport := machine.DecodeGraphDefinitionV1(payload.GraphDefinition)
	if graphReport != nil && len(graphReport.Issues) != 0 {
		return snapshot.TeamRunSnapshot{}, workflowScheduleAdmissionDenied(
			"published graph is invalid",
		)
	}
	if payload.Team.WorkspaceID != request.WorkspaceID || payload.Team.TeamID != teamID {
		return snapshot.TeamRunSnapshot{}, workflowScheduleAdmissionDenied(
			"published Artifact team does not match workflow",
		)
	}
	var lockedTeamID, teamStatus string
	if err := tx.QueryRow(ctx, `
		SELECT id, status
		FROM weave_teams
		WHERE workspace_id=$1 AND id=$2
		FOR SHARE
	`, request.WorkspaceID, teamID).Scan(&lockedTeamID, &teamStatus); err != nil {
		return snapshot.TeamRunSnapshot{}, workflowScheduleAdmissionReadError(
			err, "workflow team is unavailable",
		)
	}
	if teamStatus != "active" {
		return snapshot.TeamRunSnapshot{}, workflowScheduleAdmissionDenied(
			"workflow team is not active",
		)
	}
	if err := s.EvaluateFixedWorkflowAdmissionTx(ctx, tx, workflowdef.FixedWorkflowAdmissionRequest{
		WorkspaceID:     request.WorkspaceID,
		TeamID:          lockedTeamID,
		WorkflowID:      request.WorkflowID,
		WorkflowVersion: *publishedVersion,
		GraphDefinition: payload.GraphDefinition,
	}); err != nil {
		return snapshot.TeamRunSnapshot{}, err
	}

	decisionTime := s.clock.Now().UTC().Truncate(time.Microsecond)
	admissionDecision, err := json.Marshal(struct {
		SchemaVersion  int    `json:"schema_version"`
		TeamActive     bool   `json:"team_active"`
		WorkflowActive bool   `json:"workflow_active"`
		WorkersEnabled bool   `json:"workers_enabled"`
		VersionBlocked bool   `json:"version_blocked"`
		DecidedAt      string `json:"decided_at"`
	}{1, true, true, true, false, decisionTime.Format(time.RFC3339Nano)})
	if err != nil {
		return snapshot.TeamRunSnapshot{}, fmt.Errorf("encode workflow admission decision: %w", err)
	}
	runAssociations := json.RawMessage(
		`{"schema_version":1,"parent_run_id":null,"source_snapshot_id":null,"task_group_id":null}`,
	)
	triggerSource, err := json.Marshal(struct {
		SchemaVersion int    `json:"schema_version"`
		Type          string `json:"type"`
		SourceRef     string `json:"source_ref"`
		OccurrenceKey string `json:"occurrence_key"`
	}{1, "schedule", request.ScheduleID, request.OccurrenceKey})
	if err != nil {
		return snapshot.TeamRunSnapshot{}, fmt.Errorf("encode workflow trigger source: %w", err)
	}
	return snapshot.TeamRunSnapshot{
		RunID:                   "run-" + uuid.NewString(),
		WorkspaceID:             request.WorkspaceID,
		TeamID:                  lockedTeamID,
		SnapshotSchemaVersion:   2,
		Mode:                    "fixed_workflow",
		WorkflowID:              request.WorkflowID,
		WorkflowVersion:         *publishedVersion,
		ArtifactWorkflowID:      envelope.WorkflowID,
		ArtifactWorkflowVersion: envelope.WorkflowVersion,
		AdmissionDecision:       admissionDecision,
		RunAssociations:         runAssociations,
		TriggerSourceV2:         triggerSource,
	}, nil
}

func validateWorkflowScheduleAdmissionRequest(
	tx pgx.Tx,
	request workflowdef.WorkflowScheduleAdmissionRequest) error {
	if interfaceNil(tx) {
		return workflowScheduleAdmissionDenied("transaction is required")
	}
	for name, value := range map[string]string{
		"workspace": request.WorkspaceID,
		"schedule":  request.ScheduleID,
		"workflow":  request.WorkflowID,
	} {
		if value == "" || value != strings.TrimSpace(value) {
			return workflowScheduleAdmissionDenied(name + " identity is invalid")
		}
	}
	if !workflowScheduleOccurrenceKeyPattern.MatchString(request.OccurrenceKey) {
		return workflowScheduleAdmissionDenied("occurrence key is invalid")
	}
	if request.ScheduledFor.IsZero() {
		return workflowScheduleAdmissionDenied("scheduled time is required")
	}
	return nil
}

func interfaceNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan,
		reflect.Func,
		reflect.Interface,
		reflect.Map,
		reflect.Pointer,
		reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func (s *Store) readWorkflowScheduleArtifact(ctx context.Context, workspaceID, workflowID string, version int) (frozen.ArtifactEnvelopeV1, error) {
	if s.artifacts == nil {
		return frozen.ArtifactEnvelopeV1{}, errors.New("frozen publication reader unavailable")
	}
	artifact, err := s.artifacts.GetArtifact(ctx, workspaceID, workflowID, version)
	if err != nil {
		return frozen.ArtifactEnvelopeV1{}, err
	}
	return frozen.ArtifactEnvelopeV1{WorkspaceID: artifact.WorkspaceID, WorkflowID: artifact.WorkflowID, WorkflowVersion: artifact.WorkflowVersion, ArtifactSchemaVersion: artifact.ArtifactSchemaVersion, CanonicalizationAlgorithm: artifact.CanonicalizationAlgorithm, CanonicalizationVersion: artifact.CanonicalizationVersion, HashAlgorithm: artifact.HashAlgorithm, ContentHash: artifact.ContentHash, Payload: artifact.Payload}, nil
}

func workflowScheduleAdmissionReadError(err error, context string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return workflowScheduleAdmissionDenied(context)
	}
	return fmt.Errorf("%s: %w", context, err)
}

func workflowScheduleAdmissionDenied(context string) error {
	return fmt.Errorf("%w: %s", workflowdef.ErrWorkflowScheduleAdmissionDenied, context)
}
