// Package fanout persists fan-out task groups and their leg state.
package fanout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
)

const (
	StatusActive    = "active"
	StatusResolving = "resolving"
	StatusResolved  = "resolved"
)

// Clock supplies timestamps for fan-out state transitions.
type Clock interface {
	Now() time.Time
}

// RealClock is the production wall clock.
type RealClock struct{}

// Now returns the current wall-clock time.
func (RealClock) Now() time.Time { return time.Now() }

// Group is one fan-out task group.
type Group struct {
	ID              string     `json:"id"`
	WorkspaceID     string     `json:"workspace_id"`
	ProjectID       string     `json:"project_id,omitempty"`
	AvatarAgent     string     `json:"avatar_agent"`
	UserID          string     `json:"user_id"`
	Status          string     `json:"status"`
	OriginalRequest string     `json:"original_request"`
	Quorum          int        `json:"quorum"`
	DeadlineAt      *time.Time `json:"deadline_at,omitempty"`
	GroupOutcome    string     `json:"group_outcome,omitempty"`
	CardMessageID   string     `json:"card_message_id,omitempty"`
	ConversationID  string     `json:"conversation_id,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	ResolvedAt      *time.Time `json:"resolved_at,omitempty"`
}

// GroupListFilter selects a page of task groups within one workspace.
type GroupListFilter struct {
	WorkspaceID string
	ProjectID   string
	Statuses    []string
	UserID      string
	Limit       int
	Offset      int
}

// GroupLeg is the list-view state of one task in a fan-out group.
type GroupLeg struct {
	ID          string     `json:"id"`
	Agent       string     `json:"agent"`
	Status      string     `json:"status"`
	RunID       string     `json:"run_id,omitempty"`
	Error       string     `json:"error,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// GroupListItem combines one task group with its ordered task legs.
type GroupListItem struct {
	Group
	Legs []GroupLeg `json:"legs"`
}

// Snapshot is the state needed to resolve a group without conversation state.
type Snapshot struct {
	Group           Group         `json:"group"`
	OriginalRequest string        `json:"original_request"`
	Legs            []LegSnapshot `json:"legs"`
}

// LegSnapshot is one task result in a group snapshot.
type LegSnapshot struct {
	Agent  string          `json:"agent"`
	Status string          `json:"status"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// Store persists fan-out task groups.
type Store struct {
	pool  *pgxpool.Pool
	clock Clock
}

// New creates a fan-out store.
func New(pool *pgxpool.Pool, clock Clock) *Store {
	if clock == nil {
		clock = RealClock{}
	}
	return &Store{pool: pool, clock: clock}
}

// CreateGroup inserts an active task group after ensuring its workspace exists.
func (s *Store) CreateGroup(ctx context.Context, group Group) (Group, error) {
	if group.ID == "" {
		group.ID = "tg_" + uuid.NewString()
	}
	now := s.clock.Now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Group{}, fmt.Errorf("begin create task group: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := ensureWorkspace(ctx, tx, group.WorkspaceID, now); err != nil {
		return Group{}, err
	}
	row := tx.QueryRow(ctx, `
		INSERT INTO weave_task_group (
				id, workspace_id, project_id, avatar_agent, user_id, status, original_request,
				quorum, deadline_at, card_message_id, conversation_id, created_at, updated_at
			) VALUES ($1, $2, COALESCE(NULLIF($3, ''), (
				SELECT project_id FROM weave_conversations
				WHERE workspace_id=$2 AND id=NULLIF($11, '')
			)), $4, $5, $6, $7, $8, $9, NULLIF($10, ''), $11, $12, $12)
			RETURNING `+groupColumns,
		group.ID, group.WorkspaceID, group.ProjectID, group.AvatarAgent, group.UserID, StatusActive,
		group.OriginalRequest, group.Quorum, group.DeadlineAt, group.CardMessageID,
		group.ConversationID, now)
	created, err := scanGroup(row)
	if err != nil {
		return Group{}, fmt.Errorf("insert task group: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Group{}, fmt.Errorf("commit task group: %w", err)
	}
	return created, nil
}

// SetCardMessage attaches the in-conversation event message used to display a
// task group's progress.
func (s *Store) SetCardMessage(ctx context.Context, workspaceID, groupID, messageID string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE weave_task_group
		SET card_message_id=NULLIF($3, ''), updated_at=$4
		WHERE workspace_id=$1 AND id=$2
	`, workspaceID, groupID, messageID, s.clock.Now())
	if err != nil {
		return fmt.Errorf("set task group card message: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("task group %q not found", groupID)
	}
	return nil
}

// GroupExists reports whether a task group exists in one workspace.
func (s *Store) GroupExists(ctx context.Context, workspaceID, groupID string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1
			FROM weave_task_group
			WHERE workspace_id=$1 AND id=$2
		)
	`, workspaceID, groupID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check task group existence: %w", err)
	}
	return exists, nil
}

func ensureWorkspace(ctx context.Context, tx pgx.Tx, workspaceID string, now time.Time) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_workspaces (id, slug, name, created_at)
		VALUES ($1, $1, $1, $2)
		ON CONFLICT DO NOTHING
	`, workspaceID, now); err != nil {
		return fmt.Errorf("ensure task group workspace: %w", err)
	}
	return nil
}

// TerminalLegCounts returns terminal and total leg counts for one workspace group.
func (s *Store) TerminalLegCounts(ctx context.Context, workspaceID, groupID string) (terminal, total int, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE status IN ($3, $4, $5, $6, $7)),
			COUNT(*)
		FROM weave_task_queue
		WHERE workspace_id=$1 AND task_group_id=$2
	`, workspaceID, groupID,
		taskqueue.StatusCompleted, taskqueue.StatusFailed, taskqueue.StatusCancelled,
		taskqueue.StatusCut, taskqueue.StatusTimedOut,
	).Scan(&terminal, &total)
	if err != nil {
		return 0, 0, fmt.Errorf("count task group legs: %w", err)
	}
	return terminal, total, nil
}

// CompletedLegCount returns the successful leg count for one workspace group.
func (s *Store) CompletedLegCount(ctx context.Context, workspaceID, groupID string) (int, error) {
	var completed int
	if err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*) FILTER (WHERE status=$3)
		FROM weave_task_queue
		WHERE workspace_id=$1 AND task_group_id=$2
	`, workspaceID, groupID, taskqueue.StatusCompleted).Scan(&completed); err != nil {
		return 0, fmt.Errorf("count completed task group legs: %w", err)
	}
	return completed, nil
}

// ReadyToResolve reports whether the completed-leg quorum is met or every leg is terminal.
func (s *Store) ReadyToResolve(ctx context.Context, workspaceID, groupID string) (bool, error) {
	var quorum int
	err := s.pool.QueryRow(ctx, `
		SELECT quorum
		FROM weave_task_group
		WHERE workspace_id=$1 AND id=$2
	`, workspaceID, groupID).Scan(&quorum)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("task group %q not found", groupID)
	}
	if err != nil {
		return false, fmt.Errorf("read task group quorum: %w", err)
	}
	completed, err := s.CompletedLegCount(ctx, workspaceID, groupID)
	if err != nil {
		return false, err
	}
	terminal, total, err := s.TerminalLegCounts(ctx, workspaceID, groupID)
	if err != nil {
		return false, err
	}
	if quorum > 0 && completed >= quorum {
		return true, nil
	}
	return total > 0 && terminal == total, nil
}

// TryFinalize atomically lets exactly one caller move an active group to resolving.
func (s *Store) TryFinalize(ctx context.Context, workspaceID, groupID string) (bool, error) {
	now := s.clock.Now()
	rows, err := s.pool.Query(ctx, `
		UPDATE weave_task_group
		SET status=$3, updated_at=$4
		WHERE workspace_id=$1 AND id=$2 AND status=$5
		RETURNING id
	`, workspaceID, groupID, StatusResolving, now, StatusActive)
	if err != nil {
		return false, fmt.Errorf("finalize task group: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return false, fmt.Errorf("scan finalized task group: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("finalize task group: %w", err)
	}
	return rows.CommandTag().RowsAffected() == 1, nil
}

// CutPending marks queued, unclaimed legs as cut.
func (s *Store) CutPending(ctx context.Context, workspaceID, groupID string) (int, error) {
	now := s.clock.Now()
	rows, err := s.pool.Query(ctx, `
		UPDATE weave_task_queue
		SET status=$3, updated_at=$4, completed_at=$4
		WHERE workspace_id=$1 AND task_group_id=$2 AND status=$5
		RETURNING id
	`, workspaceID, groupID, taskqueue.StatusCut, now, taskqueue.StatusQueued)
	if err != nil {
		return 0, fmt.Errorf("cut pending task group legs: %w", err)
	}
	return countReturnedRows(rows, "cut pending task group legs")
}

// MarkResolved records the outcome of a resolving group.
func (s *Store) MarkResolved(ctx context.Context, workspaceID, groupID, outcome string) error {
	now := s.clock.Now()
	_, err := s.pool.Exec(ctx, `
		UPDATE weave_task_group
		SET status=$3, group_outcome=$4, resolved_at=$5, updated_at=$5
		WHERE workspace_id=$1 AND id=$2 AND status=$6
	`, workspaceID, groupID, StatusResolved, outcome, now, StatusResolving)
	if err != nil {
		return fmt.Errorf("resolve task group: %w", err)
	}
	return nil
}

// TimeOutOverdueLegs marks overdue non-terminal legs as timed out.
func (s *Store) TimeOutOverdueLegs(ctx context.Context, now time.Time) (int, error) {
	rows, err := s.pool.Query(ctx, `
		UPDATE weave_task_queue
		SET status=$2, completed_at=$1, updated_at=$1
		WHERE subtask_deadline_at < $1
			AND status IN ($3, $4, $5)
		RETURNING id
	`, now, taskqueue.StatusTimedOut,
		taskqueue.StatusQueued, taskqueue.StatusRunning, taskqueue.StatusDispatched)
	if err != nil {
		return 0, fmt.Errorf("time out overdue task group legs: %w", err)
	}
	return countReturnedRows(rows, "time out overdue task group legs")
}

// GroupsPastDeadline lists active groups whose deadline is before now.
func (s *Store) GroupsPastDeadline(ctx context.Context, now time.Time) ([]Group, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+groupColumns+`
		FROM weave_task_group
		WHERE status=$1 AND deadline_at < $2
		ORDER BY created_at, id
	`, StatusActive, now)
	if err != nil {
		return nil, fmt.Errorf("list task groups past deadline: %w", err)
	}
	defer rows.Close()
	groups := make([]Group, 0)
	for rows.Next() {
		group, err := scanGroup(rows)
		if err != nil {
			return nil, fmt.Errorf("scan task group past deadline: %w", err)
		}
		groups = append(groups, group)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list task groups past deadline: %w", err)
	}
	return groups, nil
}

// ListGroups returns a filtered page of groups and loads all page legs in one
// additional query.
func (s *Store) ListGroups(ctx context.Context, filter GroupListFilter) ([]GroupListItem, int, error) {
	var total int
	if err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM weave_task_group
		WHERE workspace_id=$1
			AND status = ANY($2)
				AND ($3 = '' OR user_id=$3)
				AND ($4 = '' OR project_id=$4)
		`, filter.WorkspaceID, filter.Statuses, filter.UserID, filter.ProjectID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count task groups: %w", err)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT `+groupColumns+`
		FROM weave_task_group
		WHERE workspace_id=$1
			AND status = ANY($2)
				AND ($3 = '' OR user_id=$3)
				AND ($4 = '' OR project_id=$4)
			ORDER BY created_at DESC, id DESC
			LIMIT $5 OFFSET $6
		`, filter.WorkspaceID, filter.Statuses, filter.UserID, filter.ProjectID, filter.Limit, filter.Offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list task groups: %w", err)
	}

	groups := make([]GroupListItem, 0)
	groupIDs := make([]string, 0)
	groupIndexes := make(map[string]int)
	for rows.Next() {
		group, err := scanGroup(rows)
		if err != nil {
			rows.Close()
			return nil, 0, fmt.Errorf("scan task group: %w", err)
		}
		groupIndexes[group.ID] = len(groups)
		groupIDs = append(groupIDs, group.ID)
		groups = append(groups, GroupListItem{Group: group, Legs: make([]GroupLeg, 0)})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, 0, fmt.Errorf("list task groups: %w", err)
	}
	rows.Close()
	if len(groupIDs) == 0 {
		return groups, total, nil
	}

	legRows, err := s.pool.Query(ctx, `
		SELECT task_group_id, id, agent, status, COALESCE(run_id, ''), COALESCE(error, ''),
			created_at, started_at, completed_at, updated_at
		FROM weave_task_queue
		WHERE workspace_id=$1 AND task_group_id = ANY($2)
		ORDER BY created_at, id
	`, filter.WorkspaceID, groupIDs)
	if err != nil {
		return nil, 0, fmt.Errorf("list task group legs: %w", err)
	}
	defer legRows.Close()
	for legRows.Next() {
		var groupID string
		var leg GroupLeg
		if err := legRows.Scan(
			&groupID, &leg.ID, &leg.Agent, &leg.Status, &leg.RunID, &leg.Error,
			&leg.CreatedAt, &leg.StartedAt, &leg.CompletedAt, &leg.UpdatedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("scan task group leg: %w", err)
		}
		if index, ok := groupIndexes[groupID]; ok {
			groups[index].Legs = append(groups[index].Legs, leg)
		}
	}
	if err := legRows.Err(); err != nil {
		return nil, 0, fmt.Errorf("list task group legs: %w", err)
	}
	return groups, total, nil
}

// Snapshot returns the original request and every leg result for one group.
func (s *Store) Snapshot(ctx context.Context, workspaceID, groupID string) (Snapshot, error) {
	var snapshot Snapshot
	group, err := scanGroup(s.pool.QueryRow(ctx, `
		SELECT `+groupColumns+`
		FROM weave_task_group
		WHERE workspace_id=$1 AND id=$2
	`, workspaceID, groupID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Snapshot{}, fmt.Errorf("task group %q not found", groupID)
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("read task group snapshot: %w", err)
	}
	snapshot.Group = group
	snapshot.OriginalRequest = group.OriginalRequest

	rows, err := s.pool.Query(ctx, `
		SELECT agent, status, result, COALESCE(error, '')
		FROM weave_task_queue
		WHERE workspace_id=$1 AND task_group_id=$2
		ORDER BY created_at, id
	`, workspaceID, groupID)
	if err != nil {
		return Snapshot{}, fmt.Errorf("read task group snapshot legs: %w", err)
	}
	defer rows.Close()
	snapshot.Legs = make([]LegSnapshot, 0)
	for rows.Next() {
		var leg LegSnapshot
		if err := rows.Scan(&leg.Agent, &leg.Status, &leg.Result, &leg.Error); err != nil {
			return Snapshot{}, fmt.Errorf("scan task group snapshot leg: %w", err)
		}
		snapshot.Legs = append(snapshot.Legs, leg)
	}
	if err := rows.Err(); err != nil {
		return Snapshot{}, fmt.Errorf("read task group snapshot legs: %w", err)
	}
	return snapshot, nil
}

func countReturnedRows(rows pgx.Rows, operation string) (int, error) {
	defer rows.Close()
	count := 0
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return 0, fmt.Errorf("scan %s: %w", operation, err)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("%s: %w", operation, err)
	}
	return count, nil
}

const groupColumns = `
		id, workspace_id, COALESCE(project_id, ''), avatar_agent, user_id, status, original_request, quorum,
	deadline_at, COALESCE(group_outcome, ''), COALESCE(card_message_id, ''),
	conversation_id, created_at, updated_at, resolved_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanGroup(row rowScanner) (Group, error) {
	var group Group
	err := row.Scan(
		&group.ID, &group.WorkspaceID, &group.ProjectID, &group.AvatarAgent, &group.UserID, &group.Status,
		&group.OriginalRequest, &group.Quorum, &group.DeadlineAt,
		&group.GroupOutcome, &group.CardMessageID, &group.ConversationID, &group.CreatedAt,
		&group.UpdatedAt, &group.ResolvedAt,
	)
	return group, err
}
