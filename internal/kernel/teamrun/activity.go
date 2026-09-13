package teamrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const ActivityDetailMaxBytes = 32 * 1024

type ActivityEvent struct {
	WorkspaceID   string          `json:"workspace_id"`
	RunID         string          `json:"run_id"`
	Seq           int64           `json:"seq"`
	EventID       string          `json:"event_id"`
	Kind          string          `json:"kind"`
	NodeID        string          `json:"node_id,omitempty"`
	MemberID      string          `json:"member_id,omitempty"`
	MemberVersion int64           `json:"member_version,omitempty"`
	Detail        json.RawMessage `json:"detail"`
	OccurredAt    time.Time       `json:"occurred_at"`
}

type ActivityRecorder interface {
	Record(context.Context, ActivityEvent) error
}

type PGActivityStore struct {
	Transactions TransactionBeginner
}

func (store *PGActivityStore) Record(ctx context.Context, event ActivityEvent) error {
	if store == nil || store.Transactions == nil {
		return errors.New("team run activity store is unavailable")
	}
	tx, err := store.Transactions.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin team run activity: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := store.RecordTx(ctx, tx, event); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RecordTx lets a runtime batch commit all of its stable event IDs before ACK.
func (store *PGActivityStore) RecordTx(ctx context.Context, tx pgx.Tx, event ActivityEvent) error {
	if tx == nil {
		return errors.New("activity transaction is required")
	}
	if event.WorkspaceID == "" || event.RunID == "" || event.Kind == "" ||
		event.MemberVersion < 0 || event.OccurredAt.IsZero() {
		return errors.New("team run activity event is invalid")
	}
	if event.EventID == "" {
		event.EventID = uuid.NewString()
	}
	if len(event.Detail) == 0 {
		event.Detail = json.RawMessage(`{}`)
	}
	if len(event.Detail) > ActivityDetailMaxBytes || !json.Valid(event.Detail) {
		return errors.New("team run activity detail is invalid")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(event.Detail, &object); err != nil || object == nil {
		return errors.New("team run activity detail must be an object")
	}
	_, err := tx.Exec(ctx, `INSERT INTO weave_team_run_activity_events (
		workspace_id,run_id,event_id,kind,node_id,member_id,member_version,detail,occurred_at
	) VALUES ($1,$2,$3,$4,NULLIF($5,''),NULLIF($6,''),NULLIF($7,0),$8::jsonb,$9)
	ON CONFLICT (workspace_id,run_id,event_id) DO NOTHING`,
		event.WorkspaceID, event.RunID, event.EventID, event.Kind, event.NodeID,
		event.MemberID, event.MemberVersion, string(event.Detail), event.OccurredAt.UTC(),
	)
	if err != nil {
		return fmt.Errorf("record team run activity: %w", err)
	}
	return nil
}

// List returns the most recent events in chronological order. A bounded activity
// view must retain the latest recovery facts even for a long-running task.
func (store *PGActivityStore) List(ctx context.Context, workspaceID, runID string, limit int) ([]ActivityEvent, error) {
	if store == nil || store.Transactions == nil {
		return nil, errors.New("team run activity store is unavailable")
	}
	if workspaceID == "" || runID == "" {
		return nil, errors.New("workspace_id and run_id are required")
	}
	if limit < 1 || limit > 1000 {
		limit = 250
	}
	tx, err := store.Transactions.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin list team run activity: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `SELECT workspace_id,run_id,seq,event_id,kind,
		COALESCE(node_id,''),COALESCE(member_id,''),COALESCE(member_version,0),detail,occurred_at
		FROM (SELECT * FROM weave_team_run_activity_events
			WHERE workspace_id=$1 AND run_id=$2
			ORDER BY seq DESC LIMIT $3) AS recent
		ORDER BY seq`, workspaceID, runID, limit)
	if err != nil {
		return nil, fmt.Errorf("list team run activity: %w", err)
	}
	defer rows.Close()
	events := make([]ActivityEvent, 0)
	for rows.Next() {
		var event ActivityEvent
		if err := rows.Scan(&event.WorkspaceID, &event.RunID, &event.Seq, &event.EventID,
			&event.Kind, &event.NodeID, &event.MemberID, &event.MemberVersion,
			&event.Detail, &event.OccurredAt); err != nil {
			return nil, fmt.Errorf("scan team run activity: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list team run activity rows: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit list team run activity: %w", err)
	}
	return events, nil
}

var _ ActivityRecorder = (*PGActivityStore)(nil)
