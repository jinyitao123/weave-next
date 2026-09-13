package workflowcatalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	workflowdef "github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

// AdmitWorkflowManualRunTx applies the same frozen publication and live
// admission gates as scheduled execution without creating schedule state.
func (s *Store) AdmitWorkflowManualRunTx(
	ctx context.Context,
	tx pgx.Tx,
	request workflowdef.WorkflowManualRunAdmissionRequest) (snapshot.TeamRunSnapshot, error) {
	if err := validateWorkflowManualRunAdmissionRequest(tx, request); err != nil {
		return snapshot.TeamRunSnapshot{}, err
	}

	var workspaceID string
	if err := tx.QueryRow(ctx, `
		SELECT id
		FROM weave_workspaces
		WHERE id=$1
		FOR SHARE
	`, request.WorkspaceID).Scan(&workspaceID); err != nil {
		return snapshot.TeamRunSnapshot{}, manualRunAdmissionReadError(
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
		if errors.Is(err, pgx.ErrNoRows) {
			return snapshot.TeamRunSnapshot{}, workflowdef.ErrNotFound
		}
		return snapshot.TeamRunSnapshot{}, fmt.Errorf("workflow is unavailable: %w", err)
	}
	if workflowStatus == workflowdef.WorkflowStatusArchived {
		return snapshot.TeamRunSnapshot{}, workflowdef.ErrArchived
	}
	if workflowStatus != workflowdef.WorkflowStatusActive || (publishedVersion == nil && request.WorkflowVersion == nil) {
		return snapshot.TeamRunSnapshot{}, workflowdef.ErrNotPublished
	}
	selectedVersion := 0
	if request.WorkflowVersion != nil {
		selectedVersion = *request.WorkflowVersion
	} else {
		selectedVersion = *publishedVersion
	}

	var versionStatus string
	if err := tx.QueryRow(ctx, `
		SELECT status
		FROM weave_team_workflow_versions
		WHERE workspace_id=$1 AND workflow_id=$2 AND version=$3
		FOR SHARE
	`, request.WorkspaceID, request.WorkflowID, selectedVersion).Scan(&versionStatus); err != nil {
		return snapshot.TeamRunSnapshot{}, manualRunAdmissionReadError(
			err, "published workflow version is unavailable",
		)
	}
	if versionStatus != workflowdef.VersionStatusPublished {
		return snapshot.TeamRunSnapshot{}, workflowdef.ErrNotPublished
	}

	envelope, err := s.readWorkflowScheduleArtifact(
		ctx, request.WorkspaceID, request.WorkflowID, selectedVersion,
	)
	if err != nil {
		return snapshot.TeamRunSnapshot{}, err
	}
	payload, err := frozen.DecodeArtifactEnvelopeV1(envelope)
	if err != nil {
		return snapshot.TeamRunSnapshot{}, manualRunAdmissionDenied(
			"published Artifact is invalid",
		)
	}
	_, triggerReport := machine.DecodeTriggerConfigV1(payload.TriggerConfig)
	if triggerReport != nil && len(triggerReport.Issues) != 0 {
		return snapshot.TeamRunSnapshot{}, manualRunAdmissionDenied(
			"published trigger is invalid",
		)
	}
	_, graphReport := machine.DecodeGraphDefinitionV1(payload.GraphDefinition)
	if graphReport != nil && len(graphReport.Issues) != 0 {
		return snapshot.TeamRunSnapshot{}, manualRunAdmissionDenied(
			"published graph is invalid",
		)
	}
	if payload.Team.WorkspaceID != request.WorkspaceID || payload.Team.TeamID != teamID {
		return snapshot.TeamRunSnapshot{}, manualRunAdmissionDenied(
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
		return snapshot.TeamRunSnapshot{}, manualRunAdmissionReadError(
			err, "workflow team is unavailable",
		)
	}
	if teamStatus != "active" {
		return snapshot.TeamRunSnapshot{}, manualRunAdmissionDenied(
			"workflow team is not active",
		)
	}
	if err := s.EvaluateFixedWorkflowAdmissionTx(ctx, tx, workflowdef.FixedWorkflowAdmissionRequest{
		WorkspaceID:     request.WorkspaceID,
		TeamID:          lockedTeamID,
		WorkflowID:      request.WorkflowID,
		WorkflowVersion: selectedVersion,
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
	triggerType := strings.TrimSpace(request.TriggerType)
	if triggerType == "" {
		triggerType = "manual"
	}
	triggerSource, err := json.Marshal(struct {
		SchemaVersion int    `json:"schema_version"`
		Type          string `json:"type"`
		SourceRef     string `json:"source_ref"`
	}{1, triggerType, request.SourceRef})
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
		WorkflowVersion:         selectedVersion,
		ArtifactWorkflowID:      envelope.WorkflowID,
		ArtifactWorkflowVersion: envelope.WorkflowVersion,
		AdmissionDecision:       admissionDecision,
		RunAssociations: json.RawMessage(
			`{"schema_version":1,"parent_run_id":null,"source_snapshot_id":null,"task_group_id":null}`,
		),
		TriggerSourceV2: triggerSource,
	}, nil
}

func validateWorkflowManualRunAdmissionRequest(
	tx pgx.Tx,
	request workflowdef.WorkflowManualRunAdmissionRequest) error {
	if interfaceNil(tx) {
		return manualRunAdmissionDenied("transaction is required")
	}
	for name, value := range map[string]string{
		"workspace":  request.WorkspaceID,
		"workflow":   request.WorkflowID,
		"source_ref": request.SourceRef,
	} {
		if value == "" || value != strings.TrimSpace(value) {
			return manualRunAdmissionDenied(name + " identity is invalid")
		}
	}
	if request.TriggerType != "" && request.TriggerType != "conversation_explicit" {
		return manualRunAdmissionDenied("trigger type is invalid")
	}
	if request.WorkflowVersion != nil && *request.WorkflowVersion <= 0 {
		return manualRunAdmissionDenied("workflow version is invalid")
	}
	return nil
}

func manualRunAdmissionReadError(err error, context string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return manualRunAdmissionDenied(context)
	}
	return fmt.Errorf("%s: %w", context, err)
}

func manualRunAdmissionDenied(context string) error {
	return fmt.Errorf("%w: %s", workflowdef.ErrWorkflowScheduleAdmissionDenied, context)
}
