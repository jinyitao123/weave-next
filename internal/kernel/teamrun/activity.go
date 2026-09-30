package teamrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/kernel/businessaction"
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

func (store *PGActivityStore) ListBusinessActionEvents(ctx context.Context, workspaceID, runID string) ([]ActivityEvent, error) {
	if store == nil || store.Transactions == nil || workspaceID == "" || runID == "" {
		return nil, errors.New("workspace_id and run_id are required")
	}
	tx, err := store.Transactions.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin list team run business action activity: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM weave_team_run_activity_events
		WHERE workspace_id=$1 AND run_id=$2 AND kind='business_action_started'`, workspaceID, runID).Scan(&count); err != nil {
		return nil, fmt.Errorf("count business action activity: %w", err)
	}
	if count > MaxBusinessActionOutcomesPerRun {
		return nil, ErrBusinessActionOutcomeLimitExceeded
	}
	rows, err := tx.Query(ctx, `SELECT workspace_id,run_id,seq,event_id,kind,
		COALESCE(node_id,''),COALESCE(member_id,''),COALESCE(member_version,0),detail,occurred_at
		FROM weave_team_run_activity_events
		WHERE workspace_id=$1 AND run_id=$2 AND kind IN ('business_action_started','business_action_result')
		ORDER BY seq`, workspaceID, runID)
	if err != nil {
		return nil, fmt.Errorf("list business action activity: %w", err)
	}
	defer rows.Close()
	events := make([]ActivityEvent, 0, count*2)
	for rows.Next() {
		var event ActivityEvent
		if err := rows.Scan(&event.WorkspaceID, &event.RunID, &event.Seq, &event.EventID,
			&event.Kind, &event.NodeID, &event.MemberID, &event.MemberVersion,
			&event.Detail, &event.OccurredAt); err != nil {
			return nil, fmt.Errorf("scan business action activity: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read business action activity rows: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit list business action activity: %w", err)
	}
	return events, nil
}

// RecordBusinessActionEvent serializes writes for one run, enforces the public
// outcome limit before each external action starts, and persists the receipt
// before the caller is allowed to dispatch.
func (store *PGActivityStore) RecordBusinessActionEvent(ctx context.Context, event ActivityEvent) error {
	if store == nil || store.Transactions == nil {
		return errors.New("team run activity store is unavailable")
	}
	if event.Kind != "business_action_started" && event.Kind != "business_action_result" {
		return errors.New("business action activity kind is invalid")
	}
	if event.EventID == "" {
		return errors.New("business action activity requires a stable event id")
	}
	tx, err := store.Transactions.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin record business action activity: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1),hashtext($2))`, event.WorkspaceID, event.RunID); err != nil {
		return fmt.Errorf("lock business action activity: %w", err)
	}
	if event.Kind == "business_action_started" {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM weave_team_run_activity_events
			WHERE workspace_id=$1 AND run_id=$2 AND event_id=$3)`, event.WorkspaceID, event.RunID, event.EventID).Scan(&exists); err != nil {
			return fmt.Errorf("check duplicate business action start: %w", err)
		}
		if !exists {
			var unresolved bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(
				SELECT 1 FROM weave_team_run_activity_events AS started
				LEFT JOIN LATERAL (
					SELECT result.detail->>'status' AS status
					FROM weave_team_run_activity_events AS result
					WHERE result.workspace_id=started.workspace_id AND result.run_id=started.run_id
					  AND result.kind='business_action_result'
					  AND result.node_id IS NOT DISTINCT FROM started.node_id
					  AND result.member_id IS NOT DISTINCT FROM started.member_id
					  AND result.detail->>'invocation_id'=started.detail->>'invocation_id'
					  AND result.detail->>'tool_call_id'=started.detail->>'tool_call_id'
					ORDER BY result.seq DESC LIMIT 1
				) AS latest ON true
				WHERE started.workspace_id=$1 AND started.run_id=$2 AND started.kind='business_action_started'
				  AND started.event_id<>$3
				  AND started.detail->>'source'=$4::jsonb->>'source'
				  AND started.detail->>'input_revision_id'=$4::jsonb->>'input_revision_id'
				  AND started.detail->>'capability_id'=$4::jsonb->>'capability_id'
				  AND COALESCE(started.detail->>'record_id','')=COALESCE($4::jsonb->>'record_id','')
				  AND COALESCE(latest.status,'unknown') NOT IN ('succeeded','failed')
			)`, event.WorkspaceID, event.RunID, event.EventID, string(event.Detail)).Scan(&unresolved); err != nil {
				return fmt.Errorf("check unresolved business action start: %w", err)
			}
			if unresolved {
				return businessaction.ErrActionOutcomeUnresolved
			}
			var count int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM weave_team_run_activity_events
				WHERE workspace_id=$1 AND run_id=$2 AND kind='business_action_started'`, event.WorkspaceID, event.RunID).Scan(&count); err != nil {
				return fmt.Errorf("count business action starts: %w", err)
			}
			if count >= MaxBusinessActionOutcomesPerRun {
				return ErrBusinessActionOutcomeLimitExceeded
			}
		}
	}
	if err := store.RecordTx(ctx, tx, event); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit business action activity: %w", err)
	}
	return nil
}

func (store *PGActivityStore) CheckBusinessActionReplay(ctx context.Context, check BusinessActionReplayCheck) (BusinessActionReplayDecision, error) {
	if store == nil || store.Transactions == nil || check.WorkspaceID == "" || check.RunID == "" {
		return BusinessActionReplayDecision{}, errors.New("team run business action reader is unavailable")
	}
	tx, err := store.Transactions.Begin(ctx)
	if err != nil {
		return BusinessActionReplayDecision{}, fmt.Errorf("begin business action replay check: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Rank the matching earlier starts: 0 the same call (identity), 1 an earlier
	// start on the same capability and record whose outcome is unresolved, 2 an
	// earlier success of the identical request under another call ID. Lower
	// ranks win so an unresolved write is always reported before a success.
	var status string
	var priority int
	err = tx.QueryRow(ctx, `SELECT ranked.status,ranked.priority FROM (
		SELECT outcome.status,outcome.seq,CASE
			WHEN outcome.node_id=$4 AND outcome.detail->>'invocation_id'=$5 AND outcome.detail->>'tool_call_id'=$6 THEN 0
			WHEN outcome.detail->>'capability_id'=$7 AND COALESCE(outcome.detail->>'record_id','')=$8
			 AND outcome.status NOT IN ('succeeded','failed') THEN 1
			WHEN $9<>'' AND outcome.detail->>'params_sha256'=$9 AND outcome.detail->>'capability_id'=$7
			 AND COALESCE(outcome.detail->>'record_id','')=$8 AND outcome.status='succeeded' THEN 2
		END AS priority
		FROM (
			SELECT started.seq,COALESCE(started.node_id,'') AS node_id,started.detail,
				COALESCE(latest.detail->>'status','unknown') AS status
			FROM weave_team_run_activity_events AS started
			LEFT JOIN LATERAL (
				SELECT result.detail FROM weave_team_run_activity_events AS result
				WHERE result.workspace_id=started.workspace_id AND result.run_id=started.run_id
				  AND result.kind='business_action_result'
				  AND result.node_id IS NOT DISTINCT FROM started.node_id
				  AND result.member_id IS NOT DISTINCT FROM started.member_id
				  AND result.detail->>'invocation_id'=started.detail->>'invocation_id'
				  AND result.detail->>'tool_call_id'=started.detail->>'tool_call_id'
				ORDER BY result.seq DESC LIMIT 1
			) AS latest ON true
			WHERE started.workspace_id=$1 AND started.run_id=$2 AND started.kind='business_action_started'
			  AND started.detail->>'source'='forge_mcp.run_action'
			  AND started.detail->>'input_revision_id'=$3
		) AS outcome
	) AS ranked
	WHERE ranked.priority IS NOT NULL
	ORDER BY ranked.priority,ranked.seq DESC LIMIT 1`,
		check.WorkspaceID, check.RunID, check.InputRevisionID, check.NodeID, check.InvocationID, check.CallID,
		check.CapabilityID, check.RecordID, check.ParamsSHA256).Scan(&status, &priority)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return BusinessActionReplayDecision{}, fmt.Errorf("commit business action replay check: %w", err)
		}
		return BusinessActionReplayDecision{}, nil
	}
	if err != nil {
		return BusinessActionReplayDecision{}, fmt.Errorf("check business action replay: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return BusinessActionReplayDecision{}, fmt.Errorf("commit business action replay check: %w", err)
	}
	if status != "succeeded" && status != "failed" {
		status = "unknown"
	}
	return BusinessActionReplayDecision{Status: status, Blocked: true, SameParams: priority == 2}, nil
}

var _ ActivityRecorder = (*PGActivityStore)(nil)
