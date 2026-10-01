package teamrun

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/execution"
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

// RecordBusinessActionEvent reserves an operation under the existing run lock
// before external dispatch. A precheck is advisory: the reservation repeats it
// atomically, including when another worker finished after both prechecks.
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
	var detail businessActionActivityDetailV1
	if err := json.Unmarshal(event.Detail, &detail); err != nil {
		return fmt.Errorf("decode business action activity: %w", err)
	}
	if event.Kind == "business_action_started" {
		if _, err := ProjectBusinessActionOutcomes([]ActivityEvent{event}); err != nil {
			return err
		}
	}
	if len(detail.OperationSlot) > MaxBusinessActionOperationSlotBytes {
		return errors.New("business action operation slot exceeds its metadata limit")
	}
	if detail.OperationSlot != "" && detail.OperationID != execution.EngineOperationID(detail.InputRevisionID, detail.InvocationID, detail.OperationSlot, detail.CapabilityID) {
		return businessaction.ErrActionOperationConflict
	}
	if detail.OperationID != "" {
		decoded, err := hex.DecodeString(detail.ParamsSHA256)
		if err != nil || len(decoded) != 32 {
			return errors.New("business action operation requires a request digest")
		}
	}
	if detail.Result != nil {
		cleaned := businessaction.SanitizeActionOutcomeResult(detail.Result)
		if cleaned == nil {
			return errors.New("business action result cache is invalid")
		}
		if err := businessaction.ValidateActionOutcomeResultStatus(cleaned, detail.Status); err != nil {
			return err
		}
		detail.Result = cleaned
		var err error
		event.Detail, err = json.Marshal(detail)
		if err != nil {
			return fmt.Errorf("encode business action result cache: %w", err)
		}
	}
	tx, err := store.Transactions.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin record business action activity: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1),hashtext($2))`, event.WorkspaceID, event.RunID); err != nil {
		return fmt.Errorf("lock business action activity: %w", err)
	}
	var existingRaw []byte
	var existingKind, existingNode, existingMember string
	err = tx.QueryRow(ctx, `SELECT detail,kind,COALESCE(node_id,''),COALESCE(member_id,'')
		FROM weave_team_run_activity_events WHERE workspace_id=$1 AND run_id=$2 AND event_id=$3`,
		event.WorkspaceID, event.RunID, event.EventID).Scan(&existingRaw, &existingKind, &existingNode, &existingMember)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("check business action event identity: %w", err)
	}
	if err == nil {
		var existing businessActionActivityDetailV1
		if json.Unmarshal(existingRaw, &existing) != nil || existingKind != event.Kind ||
			existingNode != event.NodeID || existingMember != event.MemberID || !sameBusinessActionDetail(existing, detail) {
			return businessaction.ErrActionOperationConflict
		}
	}
	check := BusinessActionReplayCheck{
		WorkspaceID: event.WorkspaceID, RunID: event.RunID, NodeID: event.NodeID,
		InvocationID: detail.InvocationID, CallID: detail.CallID, OperationID: detail.OperationID,
		InputRevisionID: detail.InputRevisionID, CapabilityID: detail.CapabilityID,
		RecordID: detail.RecordID, ParamsSHA256: detail.ParamsSHA256,
	}
	if event.Kind == "business_action_started" {
		decision, err := checkBusinessActionReplayTx(ctx, tx, check)
		if err != nil {
			return err
		}
		if decision.Blocked {
			if decision.Status == "unknown" {
				return businessaction.ErrActionOutcomeUnresolved
			}
			return businessaction.ErrActionAlreadyRecorded
		}
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM weave_team_run_activity_events
			WHERE workspace_id=$1 AND run_id=$2 AND kind='business_action_started'`, event.WorkspaceID, event.RunID).Scan(&count); err != nil {
			return fmt.Errorf("count business action starts: %w", err)
		}
		if count >= MaxBusinessActionOutcomesPerRun {
			return ErrBusinessActionOutcomeLimitExceeded
		}
	} else {
		// A result must belong to its reserved request. Stable operation IDs also
		// stop a reused model call ID from attaching to a different operation.
		var startedRaw, resultRaw []byte
		err := tx.QueryRow(ctx, `SELECT started.detail,latest.detail
			FROM weave_team_run_activity_events AS started
			LEFT JOIN LATERAL (
				SELECT result.detail FROM weave_team_run_activity_events AS result
				WHERE result.workspace_id=started.workspace_id AND result.run_id=started.run_id
				  AND result.kind='business_action_result'
				  AND result.node_id IS NOT DISTINCT FROM started.node_id
				  AND result.member_id IS NOT DISTINCT FROM started.member_id
				  AND result.detail->>'input_revision_id'=started.detail->>'input_revision_id'
				  AND COALESCE(result.detail->>'operation_id','')=COALESCE(started.detail->>'operation_id','')
				  AND result.detail->>'invocation_id'=started.detail->>'invocation_id'
				  AND result.detail->>'tool_call_id'=started.detail->>'tool_call_id'
				ORDER BY result.seq DESC LIMIT 1
			) AS latest ON true
			WHERE started.workspace_id=$1 AND started.run_id=$2 AND started.kind='business_action_started'
			  AND COALESCE(started.node_id,'')=$3 AND COALESCE(started.member_id,'')=$4
			  AND started.detail->>'input_revision_id'=$5
			  AND COALESCE(started.detail->>'operation_id','')=$6
			  AND started.detail->>'invocation_id'=$7 AND started.detail->>'tool_call_id'=$8
			ORDER BY started.seq DESC LIMIT 1`, event.WorkspaceID, event.RunID, event.NodeID, event.MemberID,
			detail.InputRevisionID, detail.OperationID, detail.InvocationID, detail.CallID).Scan(&startedRaw, &resultRaw)
		if err != nil {
			return fmt.Errorf("find reserved business action result: %w", err)
		}
		var startedDetail businessActionActivityDetailV1
		if err := json.Unmarshal(startedRaw, &startedDetail); err != nil || !sameBusinessActionDetail(startedDetail, detail) {
			return businessaction.ErrActionOperationConflict
		}
		if detail.Phase != "result" || (detail.Status != "succeeded" && detail.Status != "failed" && detail.Status != "unknown") {
			return errors.New("business action result status is invalid")
		}
		if len(resultRaw) != 0 {
			var prior businessActionActivityDetailV1
			if err := json.Unmarshal(resultRaw, &prior); err != nil {
				return fmt.Errorf("decode prior business action result: %w", err)
			}
			if prior.Status != detail.Status || !reflect.DeepEqual(prior.Result, detail.Result) {
				return businessaction.ErrActionOperationConflict
			}
			return tx.Commit(ctx)
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
	decision, err := checkBusinessActionReplayTx(ctx, tx, check)
	if err != nil {
		return BusinessActionReplayDecision{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return BusinessActionReplayDecision{}, fmt.Errorf("commit business action replay check: %w", err)
	}
	return decision, nil
}

func checkBusinessActionReplayTx(ctx context.Context, tx pgx.Tx, check BusinessActionReplayCheck) (BusinessActionReplayDecision, error) {
	rows, err := tx.Query(ctx, `SELECT COALESCE(started.node_id,''),started.detail,latest.detail
		FROM weave_team_run_activity_events AS started
		LEFT JOIN LATERAL (
			SELECT result.detail FROM weave_team_run_activity_events AS result
			WHERE result.workspace_id=started.workspace_id AND result.run_id=started.run_id
			  AND result.kind='business_action_result'
			  AND result.node_id IS NOT DISTINCT FROM started.node_id
			  AND result.member_id IS NOT DISTINCT FROM started.member_id
			  AND result.detail->>'input_revision_id'=started.detail->>'input_revision_id'
			  AND COALESCE(result.detail->>'operation_id','')=COALESCE(started.detail->>'operation_id','')
			  AND result.detail->>'invocation_id'=started.detail->>'invocation_id'
			  AND result.detail->>'tool_call_id'=started.detail->>'tool_call_id'
			ORDER BY result.seq DESC LIMIT 1
		) AS latest ON true
		WHERE started.workspace_id=$1 AND started.run_id=$2 AND started.kind='business_action_started'
		  AND started.detail->>'source'='forge_mcp.run_action'
		  AND started.detail->>'input_revision_id'=$3
		ORDER BY started.seq DESC`, check.WorkspaceID, check.RunID, check.InputRevisionID)
	if err != nil {
		return BusinessActionReplayDecision{}, fmt.Errorf("check business action replay: %w", err)
	}
	defer rows.Close()
	var identity, unresolved BusinessActionReplayDecision
	for rows.Next() {
		var nodeID string
		var startedRaw, resultRaw []byte
		if err := rows.Scan(&nodeID, &startedRaw, &resultRaw); err != nil {
			return BusinessActionReplayDecision{}, fmt.Errorf("scan business action replay: %w", err)
		}
		var started, result businessActionActivityDetailV1
		if err := json.Unmarshal(startedRaw, &started); err != nil {
			return BusinessActionReplayDecision{}, fmt.Errorf("decode business action start: %w", err)
		}
		status := "unknown"
		if len(resultRaw) != 0 {
			if err := json.Unmarshal(resultRaw, &result); err != nil || !sameBusinessActionDetail(started, result) {
				return BusinessActionReplayDecision{}, errors.New("business action result does not match its reserved request")
			}
			if result.Status == "succeeded" || result.Status == "failed" {
				status = result.Status
			}
		}
		sameOperation := check.OperationID != "" && started.OperationID == check.OperationID
		if sameOperation && (started.CapabilityID != check.CapabilityID || started.RecordID != check.RecordID ||
			started.ParamsSHA256 == "" || started.ParamsSHA256 != check.ParamsSHA256) {
			return BusinessActionReplayDecision{}, businessaction.ErrActionOperationConflict
		}
		// Only receipts without a stable operation use model call identity.
		// Two modern operations can intentionally reuse the same model call ID.
		legacyIdentity := started.OperationID == "" && nodeID == check.NodeID &&
			started.InvocationID == check.InvocationID && started.CallID == check.CallID
		decision := BusinessActionReplayDecision{Status: status, Blocked: true, SameOperation: sameOperation}
		if status != "unknown" && result.Result != nil {
			decision.Result = businessaction.SanitizeActionOutcomeResult(result.Result)
			if decision.Result != nil {
				if err := businessaction.ValidateActionOutcomeResultStatus(decision.Result, status); err != nil {
					return BusinessActionReplayDecision{}, err
				}
			}
		}
		if (sameOperation || legacyIdentity) && !identity.Blocked {
			identity = decision
		}
		if status == "unknown" && started.CapabilityID == check.CapabilityID && started.RecordID == check.RecordID && !unresolved.Blocked {
			unresolved = decision
		}
	}
	if err := rows.Err(); err != nil {
		return BusinessActionReplayDecision{}, fmt.Errorf("read business action replay: %w", err)
	}
	// An unresolved write on this business target stays authoritative even if
	// the requested operation has an older, confirmed cached receipt.
	if unresolved.Blocked {
		return unresolved, nil
	}
	return identity, nil
}

// ReconcileBusinessActionOperation reads only the exact Host journal slot.
// It never reserves, dispatches, or writes; only a confirmed native receipt
// can resolve a pending member intent. Other tools and old slotless receipts
// remain unresolved and cannot trigger a replay through this entry point.
func (store *PGActivityStore) ReconcileBusinessActionOperation(ctx context.Context, check BusinessActionOperationReconcileCheck) (BusinessActionReplayDecision, error) {
	if store == nil || store.Transactions == nil {
		return BusinessActionReplayDecision{}, errors.New("team run business action reader is unavailable")
	}
	if check.WorkspaceID == "" || check.RunID == "" || check.NodeID == "" || check.MemberID == "" ||
		check.InvocationID == "" || check.OperationSlot == "" || len(check.OperationSlot) > MaxBusinessActionOperationSlotBytes {
		return BusinessActionReplayDecision{}, errors.New("business action reconciliation scope is incomplete")
	}
	tx, err := store.Transactions.Begin(ctx)
	if err != nil {
		return BusinessActionReplayDecision{}, fmt.Errorf("begin business action reconciliation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `SELECT started.detail,latest.detail
		FROM weave_team_run_activity_events AS started
		LEFT JOIN LATERAL (
			SELECT result.detail FROM weave_team_run_activity_events AS result
			WHERE result.workspace_id=started.workspace_id AND result.run_id=started.run_id
			  AND result.kind='business_action_result'
			  AND result.node_id IS NOT DISTINCT FROM started.node_id
			  AND result.member_id IS NOT DISTINCT FROM started.member_id
			  AND result.detail->>'input_revision_id'=started.detail->>'input_revision_id'
			  AND result.detail->>'operation_id'=started.detail->>'operation_id'
			  AND result.detail->>'operation_slot'=started.detail->>'operation_slot'
			  AND result.detail->>'invocation_id'=started.detail->>'invocation_id'
			  AND result.detail->>'tool_call_id'=started.detail->>'tool_call_id'
			ORDER BY result.seq DESC LIMIT 1
		) AS latest ON true
		WHERE started.workspace_id=$1 AND started.run_id=$2 AND started.kind='business_action_started'
		  AND COALESCE(started.node_id,'')=$3 AND COALESCE(started.member_id,'')=$4
		  AND started.detail->>'source'='forge_mcp.run_action'
		  AND started.detail->>'invocation_id'=$5 AND started.detail->>'operation_slot'=$6
		ORDER BY started.seq DESC LIMIT 2`, check.WorkspaceID, check.RunID, check.NodeID, check.MemberID,
		check.InvocationID, check.OperationSlot)
	if err != nil {
		return BusinessActionReplayDecision{}, fmt.Errorf("read business action reconciliation: %w", err)
	}
	var startedRaw, resultRaw []byte
	found := false
	for rows.Next() {
		if found {
			rows.Close()
			return BusinessActionReplayDecision{}, businessaction.ErrActionOperationConflict
		}
		if err := rows.Scan(&startedRaw, &resultRaw); err != nil {
			rows.Close()
			return BusinessActionReplayDecision{}, fmt.Errorf("scan business action reconciliation: %w", err)
		}
		found = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return BusinessActionReplayDecision{}, fmt.Errorf("read business action reconciliation rows: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return BusinessActionReplayDecision{}, fmt.Errorf("commit read-only business action reconciliation: %w", err)
	}
	if !found {
		return BusinessActionReplayDecision{}, nil
	}
	var started, result businessActionActivityDetailV1
	if err := json.Unmarshal(startedRaw, &started); err != nil {
		return BusinessActionReplayDecision{}, fmt.Errorf("decode reconciled business action start: %w", err)
	}
	if started.Phase != "started" || started.Status != "" || started.Result != nil ||
		started.OperationID == "" || started.OperationID != execution.EngineOperationID(started.InputRevisionID,
		started.InvocationID, check.OperationSlot, started.CapabilityID) {
		return BusinessActionReplayDecision{}, businessaction.ErrActionOperationConflict
	}
	unresolved := BusinessActionReplayDecision{Blocked: true, Status: "unknown"}
	if len(resultRaw) == 0 {
		return unresolved, nil
	}
	if err := json.Unmarshal(resultRaw, &result); err != nil || result.Phase != "result" || !sameBusinessActionDetail(started, result) {
		return BusinessActionReplayDecision{}, businessaction.ErrActionOperationConflict
	}
	if result.Status != "succeeded" && result.Status != "failed" {
		return unresolved, nil
	}
	unresolved.Status = result.Status
	cleaned := businessaction.SanitizeActionOutcomeResult(result.Result)
	if cleaned == nil {
		return unresolved, nil
	}
	if err := businessaction.ValidateActionOutcomeResultStatus(cleaned, result.Status); err != nil {
		return BusinessActionReplayDecision{}, err
	}
	return BusinessActionReplayDecision{Blocked: true, SameOperation: true, Status: result.Status, Result: cleaned}, nil
}

var _ ActivityRecorder = (*PGActivityStore)(nil)
