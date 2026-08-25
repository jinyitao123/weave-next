// Package snapshot persists immutable team-run configuration snapshots.
package snapshot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrAlreadyExists indicates that a snapshot already owns the run ID.
	ErrAlreadyExists = errors.New("team run snapshot already exists")
	// ErrNotFound indicates that a run snapshot is absent from the workspace.
	ErrNotFound = errors.New("team run snapshot not found")
)

// TeamRunSnapshot freezes team configuration and dependency selections for one run.
type TeamRunSnapshot struct {
	RunID                   string          `json:"run_id"`
	WorkspaceID             string          `json:"workspace_id"`
	ProjectID               string          `json:"project_id,omitempty"`
	TeamID                  string          `json:"team_id"`
	SnapshotSchemaVersion   int             `json:"snapshot_schema_version"`
	Mode                    string          `json:"mode,omitempty"`
	WorkflowID              string          `json:"workflow_id,omitempty"`
	WorkflowVersion         int             `json:"workflow_version,omitempty"`
	LeadAvatarID            string          `json:"lead_avatar_id,omitempty"`
	LeadAvatarVersion       int             `json:"lead_avatar_version,omitempty"`
	WorkerVersions          json.RawMessage `json:"worker_versions,omitempty"`
	TeamWorkerSnapshot      json.RawMessage `json:"team_worker_snapshot,omitempty"`
	ArtifactRef             string          `json:"artifact_ref,omitempty"`
	ArtifactWorkflowID      string          `json:"artifact_workflow_id,omitempty"`
	ArtifactWorkflowVersion int             `json:"artifact_workflow_version,omitempty"`
	AdmissionDecision       json.RawMessage `json:"admission_decision"`
	InlineDependencies      json.RawMessage `json:"inline_dependencies,omitempty"`
	RunAssociations         json.RawMessage `json:"run_associations"`
	TriggerSource           string          `json:"trigger_source,omitempty"`
	TriggerSourceV2         json.RawMessage `json:"trigger_source_v2,omitempty"`
	RuntimeAssignment       json.RawMessage `json:"runtime_assignment,omitempty"`
	BuildRunID              string          `json:"build_run_id,omitempty"`
	// BuildRoundNo is the team build round that produced the candidate.
	// Zero means no round binding (legacy snapshots and admin API candidate
	// runs); when present it must be paired with the candidate identity.
	BuildRoundNo         int       `json:"build_round_no,omitempty"`
	CandidateContentHash string    `json:"candidate_content_hash,omitempty"`
	CreatedAt            time.Time `json:"created_at"`
}

// Store provides create-only persistence and workspace-scoped lookup.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore creates a TeamRunSnapshot store.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Create inserts an immutable snapshot. Existing run IDs are never overwritten.
func (s *Store) Create(ctx context.Context, snapshot TeamRunSnapshot) (*TeamRunSnapshot, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin team run snapshot transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	created, err := s.CreateTx(ctx, tx, snapshot)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit team run snapshot transaction: %w", err)
	}
	return created, nil
}

// CreateTx inserts an immutable schema-v2 snapshot in a caller-owned transaction.
func (s *Store) CreateTx(
	ctx context.Context,
	tx pgx.Tx,
	snapshot TeamRunSnapshot,
) (*TeamRunSnapshot, error) {
	triggerType, err := validateSnapshotV2(snapshot)
	if err != nil {
		return nil, fmt.Errorf("validate team run snapshot: %w", err)
	}

	created, err := scanSnapshot(tx.QueryRow(ctx, `
				INSERT INTO weave_team_run_snapshots (
					run_id, workspace_id, project_id, team_id, snapshot_schema_version, mode,
					workflow_id, workflow_version,
				lead_avatar_id, lead_avatar_version, worker_versions,
				team_worker_snapshot, artifact_ref, admission_decision,
				artifact_workflow_id, artifact_workflow_version,
				inline_dependencies, run_associations, trigger_source,
					trigger_source_v2, runtime_assignment,
					build_run_id, build_round_no, candidate_content_hash
				) VALUES (
					$1, $2, NULLIF($3, ''), $4, $5, $6,
					NULLIF($7, ''), NULLIF($8, 0),
					NULLIF($9, ''), NULLIF($10, 0), $11::jsonb,
					$12::jsonb, NULL, $13::jsonb,
					NULLIF($14, ''), NULLIF($15, 0),
					$16::jsonb, $17::jsonb, $18, $19::jsonb, $20::jsonb,
					NULLIF($21, ''), NULLIF($22, 0), NULLIF($23, '')
			)
			RETURNING `+snapshotColumns,
		snapshot.RunID,
		snapshot.WorkspaceID,
		snapshot.ProjectID,
		snapshot.TeamID,
		snapshot.SnapshotSchemaVersion,
		snapshot.Mode,
		snapshot.WorkflowID,
		snapshot.WorkflowVersion,
		snapshot.LeadAvatarID,
		snapshot.LeadAvatarVersion,
		jsonArgument(snapshot.WorkerVersions),
		jsonArgument(snapshot.TeamWorkerSnapshot),
		jsonArgument(snapshot.AdmissionDecision),
		snapshot.ArtifactWorkflowID,
		snapshot.ArtifactWorkflowVersion,
		jsonArgument(snapshot.InlineDependencies),
		jsonArgument(snapshot.RunAssociations),
		triggerType,
		jsonArgument(snapshot.TriggerSourceV2),
		jsonArgument(snapshot.RuntimeAssignment),
		snapshot.BuildRunID,
		snapshot.BuildRoundNo,
		snapshot.CandidateContentHash,
	))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, fmt.Errorf("%w: run %q", ErrAlreadyExists, snapshot.RunID)
		}
		return nil, fmt.Errorf("create team run snapshot: %w", err)
	}
	return created, nil
}

// GetByRunID returns one snapshot only when it belongs to the requested workspace.
func (s *Store) GetByRunID(ctx context.Context, workspaceID, runID string) (*TeamRunSnapshot, error) {
	snapshot, err := scanSnapshot(s.pool.QueryRow(ctx, `
		SELECT `+snapshotColumns+`
		FROM weave_team_run_snapshots
		WHERE workspace_id=$1 AND run_id=$2
	`, workspaceID, runID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: run %q", ErrNotFound, runID)
	}
	if err != nil {
		return nil, fmt.Errorf("get team run snapshot: %w", err)
	}
	return snapshot, nil
}

// ListByTeam returns workspace-scoped snapshot roots in stable run ID order.
func (s *Store) ListByTeam(
	ctx context.Context,
	workspaceID string,
	teamID string,
) ([]TeamRunSnapshot, error) {
	if workspaceID == "" || teamID == "" {
		return nil, errors.New("workspace_id and team_id are required")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+snapshotColumns+`
		FROM weave_team_run_snapshots
		WHERE workspace_id=$1 AND team_id=$2
		ORDER BY run_id
	`, workspaceID, teamID)
	if err != nil {
		return nil, fmt.Errorf("list team run snapshots: %w", err)
	}
	return scanSnapshots(rows, "list team run snapshots")
}

// ListByWorkflow returns fixed-workflow snapshot roots in stable run ID order.
func (s *Store) ListByWorkflow(
	ctx context.Context,
	workspaceID string,
	teamID string,
	workflowID string,
	workflowVersion int,
) ([]TeamRunSnapshot, error) {
	if workspaceID == "" || teamID == "" || workflowID == "" || workflowVersion < 1 {
		return nil, errors.New("workspace_id, team_id, workflow_id, and workflow_version are required")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+snapshotColumns+`
		FROM weave_team_run_snapshots
		WHERE workspace_id=$1 AND team_id=$2
			AND workflow_id=$3 AND workflow_version=$4
		ORDER BY run_id
	`, workspaceID, teamID, workflowID, workflowVersion)
	if err != nil {
		return nil, fmt.Errorf("list workflow run snapshots: %w", err)
	}
	return scanSnapshots(rows, "list workflow run snapshots")
}

const snapshotColumns = `
			run_id, workspace_id, project_id, team_id, snapshot_schema_version, COALESCE(mode, ''),
		COALESCE(workflow_id, ''), COALESCE(workflow_version, 0),
		COALESCE(lead_avatar_id, ''), COALESCE(lead_avatar_version, 0),
		worker_versions, team_worker_snapshot, COALESCE(artifact_ref, ''),
		COALESCE(artifact_workflow_id, ''),
		COALESCE(artifact_workflow_version, 0),
		admission_decision, inline_dependencies, run_associations, trigger_source,
		trigger_source_v2, runtime_assignment,
			build_run_id, build_round_no, candidate_content_hash, created_at
	`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanSnapshots(rows pgx.Rows, operation string) ([]TeamRunSnapshot, error) {
	defer rows.Close()
	snapshots := make([]TeamRunSnapshot, 0)
	for rows.Next() {
		snapshot, err := scanSnapshot(rows)
		if err != nil {
			return nil, fmt.Errorf("%s scan: %w", operation, err)
		}
		snapshots = append(snapshots, *snapshot)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s rows: %w", operation, err)
	}
	return snapshots, nil
}

func scanSnapshot(row rowScanner) (*TeamRunSnapshot, error) {
	var snapshot TeamRunSnapshot
	var workerVersions []byte
	var teamWorkers []byte
	var admission []byte
	var inlineDependencies []byte
	var associations []byte
	var triggerSourceV2 []byte
	var runtimeAssignment []byte
	var buildRunID, candidateContentHash *string
	var buildRoundNo *int64
	var legacyTriggerSource string
	var projectID *string
	if err := row.Scan(
		&snapshot.RunID,
		&snapshot.WorkspaceID,
		&projectID,
		&snapshot.TeamID,
		&snapshot.SnapshotSchemaVersion,
		&snapshot.Mode,
		&snapshot.WorkflowID,
		&snapshot.WorkflowVersion,
		&snapshot.LeadAvatarID,
		&snapshot.LeadAvatarVersion,
		&workerVersions,
		&teamWorkers,
		&snapshot.ArtifactRef,
		&snapshot.ArtifactWorkflowID,
		&snapshot.ArtifactWorkflowVersion,
		&admission,
		&inlineDependencies,
		&associations,
		&legacyTriggerSource,
		&triggerSourceV2,
		&runtimeAssignment,
		&buildRunID,
		&buildRoundNo,
		&candidateContentHash,
		&snapshot.CreatedAt,
	); err != nil {
		return nil, err
	}
	if projectID != nil {
		snapshot.ProjectID = *projectID
	}
	snapshot.WorkerVersions = append(json.RawMessage(nil), workerVersions...)
	snapshot.TeamWorkerSnapshot = append(json.RawMessage(nil), teamWorkers...)
	snapshot.AdmissionDecision = append(json.RawMessage(nil), admission...)
	snapshot.InlineDependencies = append(json.RawMessage(nil), inlineDependencies...)
	snapshot.RunAssociations = append(json.RawMessage(nil), associations...)
	snapshot.TriggerSourceV2 = append(json.RawMessage(nil), triggerSourceV2...)
	snapshot.RuntimeAssignment = append(json.RawMessage(nil), runtimeAssignment...)
	if buildRunID != nil {
		snapshot.BuildRunID = *buildRunID
	}
	if candidateContentHash != nil {
		snapshot.CandidateContentHash = *candidateContentHash
	}
	if buildRoundNo != nil {
		snapshot.BuildRoundNo = int(*buildRoundNo)
	}
	if snapshot.SnapshotSchemaVersion == 1 {
		snapshot.TriggerSource = legacyTriggerSource
	}
	return &snapshot, nil
}

func jsonArgument(value json.RawMessage) any {
	if len(value) == 0 {
		return nil
	}
	return string(value)
}

var (
	occurrenceKeyPattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	candidateHashPattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	businessRFC3339Pattern = regexp.MustCompile(
		`^([0-9]{4})-[0-9]{2}-[0-9]{2}T` +
			`(?:[01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9]` +
			`(?:\.[0-9]+)?` +
			`(?:Z|[+-](?:[01][0-9]|2[0-3]):[0-5][0-9])$`,
	)
)

func validateSnapshotV2(snapshot TeamRunSnapshot) (string, error) {
	if snapshot.SnapshotSchemaVersion != 2 {
		return "", fmt.Errorf(
			"snapshot_schema_version = %d, want 2",
			snapshot.SnapshotSchemaVersion,
		)
	}
	if snapshot.Mode != "fixed_workflow" && snapshot.Mode != "free_collab" {
		return "", fmt.Errorf("unsupported snapshot mode %q", snapshot.Mode)
	}
	if snapshot.ArtifactRef != "" {
		return "", errors.New("artifact_ref is read-only legacy data")
	}
	if err := validateAdmissionDecision(snapshot.Mode, snapshot.AdmissionDecision); err != nil {
		return "", fmt.Errorf("admission_decision: %w", err)
	}
	if err := validateRunAssociations(snapshot.RunAssociations); err != nil {
		return "", fmt.Errorf("run_associations: %w", err)
	}
	triggerType, err := validateTriggerSource(snapshot.TriggerSourceV2)
	if err != nil {
		return "", fmt.Errorf("trigger_source_v2: %w", err)
	}
	if snapshot.TriggerSource != "" && snapshot.TriggerSource != triggerType {
		return "", fmt.Errorf(
			"legacy trigger_source %q does not match typed source %q",
			snapshot.TriggerSource,
			triggerType,
		)
	}
	if err := validateCandidateIdentity(
		snapshot.BuildRunID, snapshot.BuildRoundNo, snapshot.CandidateContentHash,
	); err != nil {
		return "", fmt.Errorf("candidate identity: %w", err)
	}
	return triggerType, nil
}

func validateCandidateIdentity(buildRunID string, buildRoundNo int, contentHash string) error {
	trimmedRunID := strings.TrimSpace(buildRunID)
	trimmedHash := strings.TrimSpace(contentHash)
	if trimmedRunID == "" && trimmedHash == "" {
		if buildRoundNo != 0 {
			return errors.New("build_round_no must be paired with candidate identity")
		}
		return nil
	}
	if buildRunID != trimmedRunID || contentHash != trimmedHash {
		return errors.New("candidate identity must not contain surrounding whitespace")
	}
	if trimmedRunID == "" || trimmedHash == "" {
		return errors.New("build_run_id and candidate_content_hash must be set together")
	}
	if !candidateHashPattern.MatchString(trimmedHash) {
		return errors.New("candidate_content_hash must be 64 lowercase hex characters")
	}
	if buildRoundNo != 0 && buildRoundNo < 1 {
		return errors.New("build_round_no must be positive when present")
	}
	return nil
}

func validateAdmissionDecision(mode string, raw json.RawMessage) error {
	fields, err := exactJSONObject(raw, []string{
		"schema_version",
		"team_active",
		"workflow_active",
		"workers_enabled",
		"version_blocked",
		"decided_at",
	})
	if err != nil {
		return err
	}
	if !isJSONNumericOne(fields["schema_version"]) {
		return errors.New("schema_version must be numeric 1")
	}
	var teamActive, workersEnabled bool
	if err := json.Unmarshal(fields["team_active"], &teamActive); err != nil ||
		!teamActive {
		return errors.New("team_active must be true")
	}
	if err := json.Unmarshal(fields["workers_enabled"], &workersEnabled); err != nil ||
		!workersEnabled {
		return errors.New("workers_enabled must be true")
	}
	var decidedAt string
	if err := json.Unmarshal(fields["decided_at"], &decidedAt); err != nil {
		return errors.New("decided_at must be an RFC3339 string")
	}
	if !isBusinessRFC3339(decidedAt) {
		return errors.New("decided_at must be a valid business RFC3339 timestamp")
	}

	if mode == "fixed_workflow" {
		var workflowActive, versionBlocked bool
		if err := json.Unmarshal(fields["workflow_active"], &workflowActive); err != nil ||
			!workflowActive {
			return errors.New("fixed workflow_active must be true")
		}
		if err := json.Unmarshal(fields["version_blocked"], &versionBlocked); err != nil ||
			versionBlocked {
			return errors.New("fixed version_blocked must be false")
		}
		return nil
	}
	if !bytes.Equal(bytes.TrimSpace(fields["workflow_active"]), []byte("null")) ||
		!bytes.Equal(bytes.TrimSpace(fields["version_blocked"]), []byte("null")) {
		return errors.New("free collaboration workflow decision fields must be null")
	}
	return nil
}

func isBusinessRFC3339(value string) bool {
	matches := businessRFC3339Pattern.FindStringSubmatch(value)
	if matches == nil || matches[1] == "0000" {
		return false
	}
	_, err := time.Parse(time.RFC3339, value)
	return err == nil
}

func validateRunAssociations(raw json.RawMessage) error {
	fields, err := exactJSONObject(raw, []string{
		"schema_version",
		"parent_run_id",
		"source_snapshot_id",
		"task_group_id",
	})
	if err != nil {
		return err
	}
	if !isJSONNumericOne(fields["schema_version"]) {
		return errors.New("schema_version must be numeric 1")
	}
	for _, name := range []string{
		"parent_run_id",
		"source_snapshot_id",
		"task_group_id",
	} {
		value := bytes.TrimSpace(fields[name])
		if bytes.Equal(value, []byte("null")) {
			continue
		}
		var reference string
		if err := json.Unmarshal(value, &reference); err != nil || reference == "" {
			return fmt.Errorf("%s must be null or a non-empty string", name)
		}
	}
	return nil
}

func validateTriggerSource(raw json.RawMessage) (string, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return "", errors.New("must be a JSON object")
	}
	for _, name := range []string{"schema_version", "type", "source_ref"} {
		if _, ok := fields[name]; !ok {
			return "", fmt.Errorf("missing field %q", name)
		}
	}
	for name := range fields {
		switch name {
		case "schema_version", "type", "source_ref", "occurrence_key":
		default:
			return "", fmt.Errorf("unknown field %q", name)
		}
	}
	if !isJSONNumericOne(fields["schema_version"]) {
		return "", errors.New("schema_version must be numeric 1")
	}
	var triggerType string
	if err := json.Unmarshal(fields["type"], &triggerType); err != nil {
		return "", errors.New("type must be a string")
	}
	switch triggerType {
	case "conversation_explicit",
		"conversation_auto",
		"schedule",
		"manual",
		"api",
		"event",
		"fanout_synthesis":
	default:
		return "", fmt.Errorf("unsupported type %q", triggerType)
	}
	var sourceRef string
	if err := json.Unmarshal(fields["source_ref"], &sourceRef); err != nil ||
		sourceRef == "" {
		return "", errors.New("source_ref must be a non-empty string")
	}
	occurrence, hasOccurrence := fields["occurrence_key"]
	if triggerType == "schedule" {
		var occurrenceKey string
		if !hasOccurrence ||
			json.Unmarshal(occurrence, &occurrenceKey) != nil ||
			!occurrenceKeyPattern.MatchString(occurrenceKey) {
			return "", errors.New("schedule occurrence_key must be 64 lowercase hex characters")
		}
		return triggerType, nil
	}
	if triggerType == "manual" && hasOccurrence {
		return "", errors.New("manual occurrence_key must be absent")
	}
	if hasOccurrence && !bytes.Equal(bytes.TrimSpace(occurrence), []byte("null")) {
		return "", errors.New("non-schedule occurrence_key must be absent or null")
	}
	return triggerType, nil
}

func isJSONNumericOne(raw json.RawMessage) bool {
	if !json.Valid(raw) {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return false
	}
	number, ok := value.(json.Number)
	if !ok {
		return false
	}
	rational, ok := new(big.Rat).SetString(number.String())
	return ok && rational.Num().Cmp(rational.Denom()) == 0
}

func exactJSONObject(
	raw json.RawMessage,
	required []string,
) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, errors.New("must be a JSON object")
	}
	if len(fields) != len(required) {
		return nil, errors.New("must contain exactly the documented fields")
	}
	allowed := make(map[string]struct{}, len(required))
	for _, name := range required {
		allowed[name] = struct{}{}
		if _, ok := fields[name]; !ok {
			return nil, fmt.Errorf("missing field %q", name)
		}
	}
	for name := range fields {
		if _, ok := allowed[name]; !ok {
			return nil, fmt.Errorf("unknown field %q", name)
		}
	}
	return fields, nil
}
