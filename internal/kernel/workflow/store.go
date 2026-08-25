package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

const workflowColumns = `
	workspace_id, id, team_id, name, description, status,
	published_version, created_at, updated_at
`

const versionColumns = `
	workspace_id, workflow_id, version, status, trigger_config,
	graph_definition, created_by, created_at, updated_at, published_at
`

// PublicationDraftRead is the locked workflow and exact mutable draft used to
// build a publication candidate in the caller-owned transaction.
type PublicationDraftRead struct {
	Workflow TeamWorkflow
	Draft    TeamWorkflowVersion
}

// Store persists team workflows and mutable draft versions.
type Store struct {
	pool  *pgxpool.Pool
	clock Clock
}

// New creates a workflow store with an injected clock.
func New(pool *pgxpool.Pool, clock Clock) *Store {
	if clock == nil {
		clock = RealClock{}
	}
	return &Store{pool: pool, clock: clock}
}

// Create atomically creates a workflow and its initial version-one draft.
func (s *Store) Create(
	ctx context.Context,
	workflow *TeamWorkflow,
	initial DraftInput,
) (*TeamWorkflowVersion, error) {
	if workflow == nil {
		return nil, errors.New("create workflow: workflow is required")
	}

	created := *workflow
	if created.Status == "" {
		created.Status = WorkflowStatusActive
	}
	if created.Status == WorkflowStatusArchived {
		return nil, fmt.Errorf("%w: workflow %q", ErrArchived, created.ID)
	}
	created.PublishedVersion = nil
	created.CreatedAt = s.clock.Now()
	created.UpdatedAt = created.CreatedAt

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin create workflow: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	persisted, err := scanWorkflow(tx.QueryRow(ctx, `
		INSERT INTO weave_team_workflows (
			workspace_id, id, team_id, name, description, status,
			published_version, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, NULL, $7, $7)
		RETURNING `+workflowColumns,
		created.WorkspaceID, created.ID, created.TeamID, created.Name,
		created.Description, created.Status, created.CreatedAt,
	))
	if err != nil {
		return nil, fmt.Errorf("insert workflow: %w", err)
	}
	created = *persisted

	version, err := scanVersion(tx.QueryRow(ctx, `
		INSERT INTO weave_team_workflow_versions (
			workspace_id, workflow_id, version, status, trigger_config,
			graph_definition, created_by, created_at, updated_at, published_at
		) VALUES ($1, $2, 1, 'draft', $3, $4, $5, $6, $6, NULL)
		RETURNING `+versionColumns,
		created.WorkspaceID, created.ID, initial.TriggerConfig,
		initial.GraphDefinition, initial.CreatedBy, created.CreatedAt,
	))
	if err != nil {
		return nil, fmt.Errorf("insert initial workflow draft: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit create workflow: %w", err)
	}
	*workflow = created
	return version, nil
}

// Get returns a workflow from the exact workspace.
func (s *Store) Get(
	ctx context.Context,
	workspaceID, workflowID string,
) (*TeamWorkflow, error) {
	workflow, err := scanWorkflow(s.pool.QueryRow(ctx, `
		SELECT `+workflowColumns+`
		FROM weave_team_workflows
		WHERE workspace_id=$1 AND id=$2
	`, workspaceID, workflowID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: workflow %q", ErrNotFound, workflowID)
	}
	if err != nil {
		return nil, fmt.Errorf("get workflow: %w", err)
	}
	return workflow, nil
}

// GetVersion returns one exact workflow version from the requested workspace.
func (s *Store) GetVersion(
	ctx context.Context,
	workspaceID, workflowID string,
	version int,
) (*TeamWorkflowVersion, error) {
	workflowVersion, err := scanVersion(s.pool.QueryRow(ctx, `
		SELECT `+versionColumns+`
		FROM weave_team_workflow_versions
		WHERE workspace_id=$1 AND workflow_id=$2 AND version=$3
	`, workspaceID, workflowID, version))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf(
			"%w: workflow %q version %d",
			ErrNotFound,
			workflowID,
			version,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("get workflow version: %w", err)
	}
	return workflowVersion, nil
}

// ResolvePublicationDraftTx locks and reads one exact active workflow draft.
// The caller owns the transaction and must acquire later publication locks.
func (s *Store) ResolvePublicationDraftTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, workflowID string,
	version int,
) (*PublicationDraftRead, error) {
	if tx == nil {
		return nil, errors.New("resolve publication draft: transaction is required")
	}
	if workspaceID == "" || workspaceID != strings.TrimSpace(workspaceID) ||
		workflowID == "" || workflowID != strings.TrimSpace(workflowID) {
		return nil, fmt.Errorf("%w: workflow %q", ErrNotFound, workflowID)
	}
	if version < 1 {
		return nil, fmt.Errorf(
			"%w: workflow %q version %d",
			ErrVersionConflict,
			workflowID,
			version,
		)
	}

	var lockedWorkspaceID string
	err := tx.QueryRow(ctx, `
		SELECT id
		FROM weave_workspaces
		WHERE id=$1
		FOR SHARE
	`, workspaceID).Scan(&lockedWorkspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: workflow %q", ErrNotFound, workflowID)
	}
	if err != nil {
		return nil, fmt.Errorf("lock publication workspace: %w", err)
	}

	workflow, err := scanWorkflow(tx.QueryRow(ctx, `
		SELECT `+workflowColumns+`
		FROM weave_team_workflows
		WHERE workspace_id=$1 AND id=$2
		FOR UPDATE
	`, workspaceID, workflowID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: workflow %q", ErrNotFound, workflowID)
	}
	if err != nil {
		return nil, fmt.Errorf("lock publication workflow: %w", err)
	}
	if workflow.Status == WorkflowStatusArchived {
		return nil, fmt.Errorf("%w: workflow %q", ErrArchived, workflowID)
	}

	draft, err := scanVersion(tx.QueryRow(ctx, `
		SELECT `+versionColumns+`
		FROM weave_team_workflow_versions
		WHERE workspace_id=$1 AND workflow_id=$2 AND version=$3
		  AND status='draft'
		FOR UPDATE
	`, workspaceID, workflowID, version))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf(
			"%w: workflow %q version %d",
			ErrVersionConflict,
			workflowID,
			version,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("lock publication workflow draft: %w", err)
	}
	return &PublicationDraftRead{Workflow: *workflow, Draft: *draft}, nil
}

// ListByTeam returns one team's workflows in stable creation and ID order.
func (s *Store) ListByTeam(
	ctx context.Context,
	workspaceID, teamID string,
) ([]TeamWorkflow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+workflowColumns+`
		FROM weave_team_workflows
		WHERE workspace_id=$1 AND team_id=$2
		ORDER BY created_at, id
	`, workspaceID, teamID)
	if err != nil {
		return nil, fmt.Errorf("list team workflows: %w", err)
	}
	defer rows.Close()

	workflows := make([]TeamWorkflow, 0)
	for rows.Next() {
		workflow, err := scanWorkflow(rows)
		if err != nil {
			return nil, fmt.Errorf("scan team workflow: %w", err)
		}
		workflows = append(workflows, *workflow)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list team workflows: %w", err)
	}
	return workflows, nil
}

// ListVersionsByWorkflows returns every version of the requested workflows in
// stable workflow ID and version order. It is read-only and powers the HTTP
// workflow read models; an empty workflow ID list returns an empty result.
func (s *Store) ListVersionsByWorkflows(
	ctx context.Context,
	workspaceID string,
	workflowIDs []string,
) ([]TeamWorkflowVersion, error) {
	versions := make([]TeamWorkflowVersion, 0)
	if len(workflowIDs) == 0 {
		return versions, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+versionColumns+`
		FROM weave_team_workflow_versions
		WHERE workspace_id=$1 AND workflow_id=ANY($2)
		ORDER BY workflow_id, version
	`, workspaceID, workflowIDs)
	if err != nil {
		return nil, fmt.Errorf("list workflow versions: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		version, err := scanVersion(rows)
		if err != nil {
			return nil, fmt.Errorf("scan workflow version: %w", err)
		}
		versions = append(versions, *version)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list workflow versions: %w", err)
	}
	return versions, nil
}

// CreateDraft clones the published version into the next version number.
func (s *Store) CreateDraft(
	ctx context.Context,
	workspaceID, workflowID, createdBy string,
) (*TeamWorkflowVersion, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin create workflow draft: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status string
	var publishedVersion *int
	err = tx.QueryRow(ctx, `
		SELECT status, published_version
		FROM weave_team_workflows
		WHERE workspace_id=$1 AND id=$2
		FOR UPDATE
	`, workspaceID, workflowID).Scan(&status, &publishedVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: workflow %q", ErrNotFound, workflowID)
	}
	if err != nil {
		return nil, fmt.Errorf("lock workflow for draft creation: %w", err)
	}
	if status == WorkflowStatusArchived {
		return nil, fmt.Errorf("%w: workflow %q", ErrArchived, workflowID)
	}

	var draftExists bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM weave_team_workflow_versions
			WHERE workspace_id=$1 AND workflow_id=$2 AND status='draft'
		)
	`, workspaceID, workflowID).Scan(&draftExists); err != nil {
		return nil, fmt.Errorf("check existing workflow draft: %w", err)
	}
	if draftExists {
		return nil, fmt.Errorf("%w: workflow %q", ErrDraftExists, workflowID)
	}
	if publishedVersion == nil {
		return nil, fmt.Errorf("%w: workflow %q has no source version", ErrVersionConflict, workflowID)
	}

	var nextVersion int
	var triggerConfig, graphDefinition []byte
	err = tx.QueryRow(ctx, `
		SELECT
			(SELECT COALESCE(MAX(version), 0) + 1
			 FROM weave_team_workflow_versions
			 WHERE workspace_id=$1 AND workflow_id=$2),
			trigger_config,
			graph_definition
		FROM weave_team_workflow_versions
		WHERE workspace_id=$1 AND workflow_id=$2 AND version=$3 AND status='published'
	`, workspaceID, workflowID, *publishedVersion).Scan(
		&nextVersion, &triggerConfig, &graphDefinition,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: workflow %q published source is missing", ErrVersionConflict, workflowID)
	}
	if err != nil {
		return nil, fmt.Errorf("read published workflow source: %w", err)
	}

	now := s.clock.Now()
	version, err := scanVersion(tx.QueryRow(ctx, `
		INSERT INTO weave_team_workflow_versions (
			workspace_id, workflow_id, version, status, trigger_config,
			graph_definition, created_by, created_at, updated_at, published_at
		) VALUES ($1, $2, $3, 'draft', $4, $5, $6, $7, $7, NULL)
		RETURNING `+versionColumns,
		workspaceID, workflowID, nextVersion, triggerConfig,
		graphDefinition, createdBy, now,
	))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.ConstraintName == "uniq_weave_team_workflow_versions_draft" {
			return nil, fmt.Errorf("%w: workflow %q", ErrDraftExists, workflowID)
		}
		return nil, fmt.Errorf("insert workflow draft: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit workflow draft: %w", err)
	}
	return version, nil
}

// UpdateDraft replaces mutable draft content using version and timestamp CAS.
func (s *Store) UpdateDraft(
	ctx context.Context,
	workspaceID, workflowID string,
	version int,
	expectedUpdatedAt time.Time,
	input DraftInput,
) (*TeamWorkflowVersion, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin update workflow draft: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status string
	err = tx.QueryRow(ctx, `
		SELECT status
		FROM weave_team_workflows
		WHERE workspace_id=$1 AND id=$2
		FOR UPDATE
	`, workspaceID, workflowID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: workflow %q", ErrNotFound, workflowID)
	}
	if err != nil {
		return nil, fmt.Errorf("lock workflow for draft update: %w", err)
	}
	if status == WorkflowStatusArchived {
		return nil, fmt.Errorf("%w: workflow %q", ErrArchived, workflowID)
	}

	updated, err := scanVersion(tx.QueryRow(ctx, `
		UPDATE weave_team_workflow_versions
		SET trigger_config=$5,
			graph_definition=$6,
			updated_at=GREATEST($7::timestamptz, updated_at + interval '1 microsecond')
		WHERE workspace_id=$1 AND workflow_id=$2 AND version=$3
		  AND status='draft' AND updated_at=$4
		RETURNING `+versionColumns,
		workspaceID, workflowID, version, expectedUpdatedAt,
		input.TriggerConfig, input.GraphDefinition, s.clock.Now(),
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: workflow %q version %d", ErrVersionConflict, workflowID, version)
	}
	if err != nil {
		return nil, fmt.Errorf("update workflow draft: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit workflow draft update: %w", err)
	}
	return updated, nil
}

// RestoreDraftResult is the outcome of one baseline draft creation.
// AlreadyRestored is true when the workflow's published content already
// matches the frozen baseline content, in which case no draft was created and
// Version is nil.
type RestoreDraftResult struct {
	Version         *TeamWorkflowVersion
	AlreadyRestored bool
}

// CreateRestoreDraftTx creates a new draft at MAX(version)+1 from explicitly
// frozen trigger/graph content inside the caller-owned transaction. Unlike
// CreateDraft it never clones the current published version: the restore
// draft carries exactly the frozen baseline content. A pre-existing draft
// (including any draft unrelated to the rollback) fails with ErrDraftExists —
// a rollback never silently supersedes another draft. Replaying the same
// baseline content after it was already published returns AlreadyRestored
// without minting a second version.
func (s *Store) CreateRestoreDraftTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, workflowID, createdBy string,
	expectedContentHash string,
	input DraftInput,
) (*RestoreDraftResult, error) {
	if tx == nil {
		return nil, errors.New("create restore workflow draft: transaction is required")
	}
	if workspaceID == "" || workflowID == "" || createdBy == "" {
		return nil, errors.New(
			"create restore workflow draft: workspace, workflow, and creator are required",
		)
	}

	var (
		status           string
		publishedVersion *int
	)
	err := tx.QueryRow(ctx, `
		SELECT status, published_version
		FROM weave_team_workflows
		WHERE workspace_id=$1 AND id=$2
		FOR UPDATE
	`, workspaceID, workflowID).Scan(&status, &publishedVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: workflow %q", ErrNotFound, workflowID)
	}
	if err != nil {
		return nil, fmt.Errorf("lock workflow for restore draft: %w", err)
	}
	if status == WorkflowStatusArchived {
		return nil, fmt.Errorf("%w: workflow %q", ErrArchived, workflowID)
	}

	// Content-idempotent replay: when the published content already equals the
	// frozen baseline content, the restore is a no-op.
	if publishedVersion != nil {
		var triggerConfig, graphDefinition []byte
		err := tx.QueryRow(ctx, `
			SELECT trigger_config, graph_definition
			FROM weave_team_workflow_versions
			WHERE workspace_id=$1 AND workflow_id=$2 AND version=$3
			  AND status='published'
		`, workspaceID, workflowID, *publishedVersion).Scan(
			&triggerConfig,
			&graphDefinition,
		)
		if err == nil {
			publishedHash, hashErr := hashRestoreWorkflowContent(triggerConfig, graphDefinition)
			if hashErr != nil {
				return nil, fmt.Errorf("hash published workflow content: %w", hashErr)
			}
			if publishedHash == expectedContentHash {
				return &RestoreDraftResult{AlreadyRestored: true}, nil
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("read published workflow content: %w", err)
		}
	}

	var draftExists bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM weave_team_workflow_versions
			WHERE workspace_id=$1 AND workflow_id=$2 AND status='draft'
		)
	`, workspaceID, workflowID).Scan(&draftExists); err != nil {
		return nil, fmt.Errorf("check existing workflow draft: %w", err)
	}
	if draftExists {
		return nil, fmt.Errorf("%w: workflow %q", ErrDraftExists, workflowID)
	}

	var nextVersion int
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(MAX(version), 0) + 1
		FROM weave_team_workflow_versions
		WHERE workspace_id=$1 AND workflow_id=$2
	`, workspaceID, workflowID).Scan(&nextVersion); err != nil {
		return nil, fmt.Errorf("derive next workflow version: %w", err)
	}

	now := s.clock.Now()
	version, err := scanVersion(tx.QueryRow(ctx, `
		INSERT INTO weave_team_workflow_versions (
			workspace_id, workflow_id, version, status, trigger_config,
			graph_definition, created_by, created_at, updated_at, published_at
		) VALUES ($1, $2, $3, 'draft', $4, $5, $6, $7, $7, NULL)
		RETURNING `+versionColumns,
		workspaceID, workflowID, nextVersion, input.TriggerConfig,
		input.GraphDefinition, createdBy, now,
	))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.ConstraintName == "uniq_weave_team_workflow_versions_draft" {
			return nil, fmt.Errorf("%w: workflow %q", ErrDraftExists, workflowID)
		}
		return nil, fmt.Errorf("insert restore workflow draft: %w", err)
	}
	return &RestoreDraftResult{Version: version}, nil
}

// hashRestoreWorkflowContent computes the same canonical content hash the
// team-build baseline used for a frozen trigger+graph document.
func hashRestoreWorkflowContent(trigger, graph json.RawMessage) (string, error) {
	raw, err := json.Marshal(struct {
		Trigger json.RawMessage `json:"trigger"`
		Graph   json.RawMessage `json:"graph"`
	}{trigger, graph})
	if err != nil {
		return "", fmt.Errorf("marshal workflow restore content: %w", err)
	}
	canonical, err := frozen.CanonicalizeJSON(raw)
	if err != nil {
		return "", fmt.Errorf("canonicalize workflow restore content: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// Archive retires a workflow without removing its versions or history.
func (s *Store) Archive(ctx context.Context, workspaceID, workflowID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin archive workflow: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status string
	err = tx.QueryRow(ctx, `
		SELECT status
		FROM weave_team_workflows
		WHERE workspace_id=$1 AND id=$2
		FOR UPDATE
	`, workspaceID, workflowID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: workflow %q", ErrNotFound, workflowID)
	}
	if err != nil {
		return fmt.Errorf("lock workflow for archive: %w", err)
	}
	if status == WorkflowStatusActive {
		if _, err := tx.Exec(ctx, `
			UPDATE weave_team_workflows
			SET status='archived', updated_at=$3
			WHERE workspace_id=$1 AND id=$2
		`, workspaceID, workflowID, s.clock.Now()); err != nil {
			return fmt.Errorf("archive workflow: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit archive workflow: %w", err)
	}
	return nil
}

// DeletePureDraft removes only workflows that have never been published or
// admitted. Draft rows are removed before the owning workflow row.
func (s *Store) DeletePureDraft(
	ctx context.Context,
	workspaceID, workflowID string,
) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin delete pure workflow draft: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var publishedVersion *int
	err = tx.QueryRow(ctx, `
		SELECT published_version
		FROM weave_team_workflows
		WHERE workspace_id=$1 AND id=$2
		FOR UPDATE
	`, workspaceID, workflowID).Scan(&publishedVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: workflow %q", ErrNotFound, workflowID)
	}
	if err != nil {
		return fmt.Errorf("lock workflow for pure draft deletion: %w", err)
	}
	if publishedVersion != nil {
		return fmt.Errorf("%w: workflow %q", ErrNotPureDraft, workflowID)
	}

	versionRows, err := tx.Query(ctx, `
		SELECT version
		FROM weave_team_workflow_versions
		WHERE workspace_id=$1 AND workflow_id=$2 AND status='draft'
		ORDER BY version
		FOR UPDATE
	`, workspaceID, workflowID)
	if err != nil {
		return fmt.Errorf("lock workflow draft versions for deletion: %w", err)
	}
	for versionRows.Next() {
		var version int
		if err := versionRows.Scan(&version); err != nil {
			versionRows.Close()
			return fmt.Errorf("scan locked workflow draft version: %w", err)
		}
	}
	if err := versionRows.Err(); err != nil {
		versionRows.Close()
		return fmt.Errorf("lock workflow draft versions for deletion: %w", err)
	}
	versionRows.Close()

	var versionCount int
	var hasHistory bool
	if err := tx.QueryRow(ctx, `
		SELECT
			(SELECT count(*)
			 FROM weave_team_workflow_versions
			 WHERE workspace_id=$1 AND workflow_id=$2),
			EXISTS (
				SELECT 1 FROM weave_team_workflow_versions
				WHERE workspace_id=$1 AND workflow_id=$2 AND status <> 'draft'
				UNION ALL
				SELECT 1 FROM weave_published_artifact_contents
				WHERE workspace_id=$1 AND workflow_id=$2
				UNION ALL
				SELECT 1 FROM weave_workflow_version_admission_statuses
				WHERE workspace_id=$1 AND workflow_id=$2
				UNION ALL
				SELECT 1 FROM weave_workflow_version_admission_audits
				WHERE workspace_id=$1 AND workflow_id=$2
				UNION ALL
				SELECT 1 FROM weave_team_run_snapshots
				WHERE workspace_id=$1 AND workflow_id=$2
			)
	`, workspaceID, workflowID).Scan(&versionCount, &hasHistory); err != nil {
		return fmt.Errorf("check workflow publication history: %w", err)
	}
	if versionCount == 0 || hasHistory {
		return fmt.Errorf("%w: workflow %q", ErrNotPureDraft, workflowID)
	}

	if _, err := tx.Exec(ctx, `
		DELETE FROM weave_team_workflow_dependencies
		WHERE workspace_id=$1 AND workflow_id=$2
	`, workspaceID, workflowID); err != nil {
		return fmt.Errorf("delete workflow draft dependencies: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM weave_team_workflow_versions
		WHERE workspace_id=$1 AND workflow_id=$2 AND status='draft'
	`, workspaceID, workflowID); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) &&
			pgErr.Code == "23503" &&
			pgErr.ConstraintName == "weave_team_run_snapshots_workflow_version_fk" {
			return fmt.Errorf("%w: workflow %q", ErrNotPureDraft, workflowID)
		}
		return fmt.Errorf("delete workflow drafts: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM weave_team_workflows
		WHERE workspace_id=$1 AND id=$2
	`, workspaceID, workflowID); err != nil {
		return fmt.Errorf("delete pure draft workflow: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit pure draft workflow deletion: %w", err)
	}
	return nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanWorkflow(row rowScanner) (*TeamWorkflow, error) {
	var workflow TeamWorkflow
	err := row.Scan(
		&workflow.WorkspaceID,
		&workflow.ID,
		&workflow.TeamID,
		&workflow.Name,
		&workflow.Description,
		&workflow.Status,
		&workflow.PublishedVersion,
		&workflow.CreatedAt,
		&workflow.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &workflow, nil
}

func scanVersion(row rowScanner) (*TeamWorkflowVersion, error) {
	var version TeamWorkflowVersion
	var triggerConfig, graphDefinition []byte
	err := row.Scan(
		&version.WorkspaceID,
		&version.WorkflowID,
		&version.Version,
		&version.Status,
		&triggerConfig,
		&graphDefinition,
		&version.CreatedBy,
		&version.CreatedAt,
		&version.UpdatedAt,
		&version.PublishedAt,
	)
	if err != nil {
		return nil, err
	}
	version.TriggerConfig = append(version.TriggerConfig, triggerConfig...)
	version.GraphDefinition = append(version.GraphDefinition, graphDefinition...)
	return &version, nil
}
