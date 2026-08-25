package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

var workflowCandidateContentHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// WorkflowCandidateRunAdmissionRequest identifies one admin test run of a
// frozen publication candidate. The candidate's own content hash binds the
// workflow version, so no version travels in the request.
type WorkflowCandidateRunAdmissionRequest struct {
	WorkspaceID string
	WorkflowID  string
	BuildRunID  string
	// RoundNo is the team build round that produced the candidate. Zero
	// keeps the pre-T14B-2A admin API behavior (candidate test run outside a
	// build round); positive values are persisted on the snapshot and paired
	// with the candidate identity.
	RoundNo     int
	ContentHash string
	SourceRef   string
	TriggerType string
}

// AdmitWorkflowCandidateRunTx is the sibling of AdmitWorkflowManualRunTx for
// frozen candidates: it skips the published_version requirement and validates
// the candidate envelope by content hash instead of the published artifact.
// Every remaining gate (team active, roster, graph validity) is identical to
// the published path, and the produced snapshot keeps the exact fixed
// workflow shape plus the candidate run identity.
func (s *Store) AdmitWorkflowCandidateRunTx(
	ctx context.Context,
	tx pgx.Tx,
	request WorkflowCandidateRunAdmissionRequest,
) (snapshot.TeamRunSnapshot, error) {
	if err := validateWorkflowCandidateRunAdmissionRequest(tx, request); err != nil {
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
		workflowStatus string
		teamID         string
	)
	if err := tx.QueryRow(ctx, `
		SELECT status, team_id
		FROM weave_team_workflows
		WHERE workspace_id=$1 AND id=$2
		FOR SHARE
	`, request.WorkspaceID, request.WorkflowID).Scan(
		&workflowStatus, &teamID,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return snapshot.TeamRunSnapshot{}, ErrNotFound
		}
		return snapshot.TeamRunSnapshot{}, fmt.Errorf("workflow is unavailable: %w", err)
	}
	if workflowStatus == WorkflowStatusArchived {
		return snapshot.TeamRunSnapshot{}, ErrArchived
	}
	if workflowStatus != WorkflowStatusActive {
		return snapshot.TeamRunSnapshot{}, manualRunAdmissionDenied(
			"workflow is not active",
		)
	}

	candidate, err := s.GetCandidateTx(
		ctx, tx, request.WorkspaceID, request.WorkflowID, request.ContentHash,
	)
	if err != nil {
		return snapshot.TeamRunSnapshot{}, err
	}
	if candidate.ContentHash != request.ContentHash {
		return snapshot.TeamRunSnapshot{}, manualRunAdmissionDenied(
			"candidate content hash does not match request",
		)
	}
	payload := candidate.Payload
	if payload.Team.WorkspaceID != request.WorkspaceID ||
		payload.Team.TeamID != teamID {
		return snapshot.TeamRunSnapshot{}, manualRunAdmissionDenied(
			"candidate Artifact team does not match workflow",
		)
	}
	_, triggerReport := machine.DecodeTriggerConfigV1(payload.TriggerConfig)
	if triggerReport != nil && len(triggerReport.Issues) != 0 {
		return snapshot.TeamRunSnapshot{}, manualRunAdmissionDenied(
			"candidate trigger is invalid",
		)
	}
	_, graphReport := machine.DecodeGraphDefinitionV1(payload.GraphDefinition)
	if graphReport != nil && len(graphReport.Issues) != 0 {
		return snapshot.TeamRunSnapshot{}, manualRunAdmissionDenied(
			"candidate graph is invalid",
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
	if teamStatus != "active" && teamStatus != "building" {
		return snapshot.TeamRunSnapshot{}, manualRunAdmissionDenied(
			"workflow team is not publishable",
		)
	}
	// Reuse the published path's roster gate. The version-blocked half of
	// EvaluateFixedWorkflowAdmissionTx is intentionally skipped: admission
	// status rows only exist for published versions (the DB insert guard
	// enforces that), so a frozen draft candidate can never be blocked.
	if err := evaluateFixedWorkflowRosterGateTx(
		ctx, tx, request.WorkspaceID, lockedTeamID,
		payload.GraphDefinition, candidate.WorkflowVersion,
	); err != nil {
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
		triggerType = "api"
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
		WorkflowVersion:         candidate.WorkflowVersion,
		ArtifactWorkflowID:      candidate.WorkflowID,
		ArtifactWorkflowVersion: candidate.WorkflowVersion,
		AdmissionDecision:       admissionDecision,
		RunAssociations: json.RawMessage(
			`{"schema_version":1,"parent_run_id":null,"source_snapshot_id":null,"task_group_id":null}`,
		),
		TriggerSourceV2:      triggerSource,
		BuildRunID:           request.BuildRunID,
		BuildRoundNo:         request.RoundNo,
		CandidateContentHash: request.ContentHash,
	}, nil
}

func validateWorkflowCandidateRunAdmissionRequest(
	tx pgx.Tx,
	request WorkflowCandidateRunAdmissionRequest,
) error {
	if interfaceNil(tx) {
		return manualRunAdmissionDenied("transaction is required")
	}
	for name, value := range map[string]string{
		"workspace":  request.WorkspaceID,
		"workflow":   request.WorkflowID,
		"build_run":  request.BuildRunID,
		"source_ref": request.SourceRef,
		"content":    request.ContentHash,
	} {
		if value == "" || value != strings.TrimSpace(value) {
			return manualRunAdmissionDenied(name + " identity is invalid")
		}
	}
	if !workflowCandidateContentHashPattern.MatchString(request.ContentHash) {
		return manualRunAdmissionDenied("candidate content hash is invalid")
	}
	if request.RoundNo < 0 {
		return manualRunAdmissionDenied("candidate round_no is invalid")
	}
	if request.TriggerType != "" && request.TriggerType != "api" {
		return manualRunAdmissionDenied("trigger type is invalid")
	}
	return nil
}
